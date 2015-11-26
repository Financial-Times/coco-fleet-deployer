package main

import (
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coreos/fleet/client"
	"github.com/coreos/fleet/schema"
	"github.com/coreos/fleet/unit"
	"golang.org/x/net/proxy"
	"gopkg.in/yaml.v2"
)

var (
	destroyFlag             = flag.Bool("destroy", false, "Destroy units not found in the definition")
	fleetEndpoint           = flag.String("fleetEndpoint", "", "Fleet API http endpoint: `http://host:port`")
	socksProxy              = flag.String("socksProxy", "", "address of socks proxy, e.g., 127.0.0.1:9050")
	destroyServiceBlacklist = map[string]struct{}{"deployer.service": struct{}{}, "deployer.timer": struct{}{}}
	rootURI                 = flag.String("rootURI", "", "Base uri to use when constructing service file URI. Only used if service file URI is relative.")
)

type services struct {
	Services []service `yaml:"services"`
}

type service struct {
	Name                 string `yaml:"name"`
	Version              string `yaml:"version"`
	Count                int    `yaml:"count"`
	URI                  string `yaml:"uri"`
	DesiredState         string `yaml:"desiredState"`
	SequentialDeployment bool   `yaml:"sequentialDeployment"`
}

type serviceDefinitionClient interface {
	servicesDefinition() (services, error)
	serviceFile(service service) ([]byte, error)
}

type httpServiceDefinitionClient struct {
	httpClient *http.Client
	rootURI    string
}

type zddInfo struct {
	unit  *schema.Unit
	count int
}

func renderServiceDefinitionYaml(serviceYaml []byte) (services services, err error) {
	if err = yaml.Unmarshal(serviceYaml, &services); err != nil {
		panic(err)
	}
	return
}

func (hsdc *httpServiceDefinitionClient) servicesDefinition() (services, error) {
	resp, err := hsdc.httpClient.Get(hsdc.rootURI + "services.yaml")
	if err != nil {
		return services{}, err
	}
	defer resp.Body.Close()

	serviceYaml, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		panic(err)
	}
	return renderServiceDefinitionYaml(serviceYaml)
}

func (hsdc *httpServiceDefinitionClient) serviceFile(service service) ([]byte, error) {
	serviceFileURI, err := buildServiceFileURI(service, hsdc.rootURI)
	if err != nil {
		return nil, err
	}
	resp, err := hsdc.httpClient.Get(serviceFileURI)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	serviceTemplate, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return serviceTemplate, nil
}

func renderedServiceFile(serviceTemplate []byte, context map[string]interface{}) (string, error) {
	if context["version"] == "" {
		return string(serviceTemplate), nil
	}
	versionString := fmt.Sprintf("DOCKER_APP_VERSION=%s", context["version"])
	serviceTemplateString := strings.Replace(string(serviceTemplate), "DOCKER_APP_VERSION=latest", versionString, 1)
	return serviceTemplateString, nil
}

func main() {
	flag.Parse()
	if *fleetEndpoint == "" {
		log.Fatal("Fleet endpoint is required")
	}

	if *rootURI == "" {
		log.Fatal("Services definition file uri is required")
	}
	d, err := newDeployer()
	if err != nil {
		panic(err)
	}

	for {
		log.Printf("Starting deploy run")
		if err := d.deployAll(); err != nil {
			log.Fatalf("Failed to run deploy : %v\n", err.Error())
		}
		log.Printf("Finished deploy run")
		time.Sleep(1 * time.Minute)
	}
}

func (d *deployer) isNewUnit(u *schema.Unit) (bool, error) {
	currentUnit, err := d.fleetapi.Unit(u.Name)
	if err != nil {
		return false, err
	}
	return currentUnit == nil, nil
}

func (d *deployer) isUpdatedUnit(newUnit *schema.Unit) (bool, error) {
	currentUnit, err := d.fleetapi.Unit(newUnit.Name)
	if err != nil {
		return false, err
	}

	nuf := schema.MapSchemaUnitOptionsToUnitFile(newUnit.Options)
	cuf := schema.MapSchemaUnitOptionsToUnitFile(currentUnit.Options)

	return nuf.Hash() != cuf.Hash(), nil
}

func (d *deployer) destroyUnwanted(wantedUnits, currentUnits map[string]*schema.Unit) error {
	for _, u := range currentUnits {
		if wantedUnits[u.Name] == nil {
			//Do not destroy the deployer itself
			if _, ok := destroyServiceBlacklist[u.Name]; !ok {
				err := d.fleetapi.DestroyUnit(u.Name)
				if err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (d *deployer) launchAll(wantedUnits, currentUnits map[string]*schema.Unit, zddUnits map[string]zddInfo) error {
	for _, u := range wantedUnits {
		// unit may have already been deployed - all nodes of a service get deployed,
		// when the first one appears in the wanted list

		if u.DesiredState == "" {
			u.DesiredState = "launched"
		}

		//units that are changed are set to Inactive in deployUnit()
		if currentUnits[u.Name].DesiredState != u.DesiredState {
			err := d.fleetapi.SetUnitTargetState(u.Name, u.DesiredState)
			if err != nil {
				return err
			}
			continue
		}
	}

	return nil
}

func (d *deployer) performSequentialDeployment(u *schema.Unit, zddUnits map[string]zddInfo) (map[string]bool, error) {
	deployedUnits := make(map[string]bool)
	serviceName := strings.Split(u.Name, "@")[0]
	nrOfNodes := zddUnits[u.Name].count

	for i := 1; i <= nrOfNodes; i++ {
		// generate unit name with number - the one we have originally might not be
		// the 1st node
		unitName := fmt.Sprintf("%v@%d.%v", serviceName, i, strings.Split(u.Name, ".")[1])

		// start the service
		err := d.fleetapi.SetUnitTargetState(unitName, u.DesiredState)
		if err != nil {
			return nil, err
		}

		sidekickName := strings.Replace(unitName, "@", "-sidekick@", 1)

		//record the fact we deployed these
		deployedUnits[unitName] = true
		deployedUnits[sidekickName] = true

		//if it's the last one of the cluster, we're done
		if i == nrOfNodes {
			break
		}

		// every second, check if the corresponding sidekick is up
		// we do this for a maximum of 5 minutes
		timeoutChan := make(chan bool)
		go func() {
			<-time.After(time.Duration(5) * time.Minute)
			close(timeoutChan)
		}()

		tickerChan := time.NewTicker(time.Duration(30) * time.Second)

		for {
			select {
			case <-tickerChan.C:
				sidekickStatus, err := d.fleetapi.Unit(sidekickName)
				if err != nil {
					return nil, err
				}

				log.Printf("INFO Sidekick status: [%v]\n", sidekickStatus.CurrentState)
				if sidekickStatus.CurrentState == "launched" {
					tickerChan.Stop()
					break
				}

				continue
			case <-timeoutChan:
				tickerChan.Stop()
				log.Printf("WARN Service [%v] didn't start up in time", unitName)
				break
			}
			break
		}
	}
	return deployedUnits, nil
}

func (d *deployer) deployAll() error {
	// Get service definition - wanted units
	//get whitelist
	wantedUnits, zddUnits, err := d.buildWantedUnits()
	deployedUnits := make(map[string]bool)

	if err != nil {
		return err
	}

	// create any missing units
	for _, u := range wantedUnits {
		isNew, err := d.isNewUnit(u)
		if err != nil {
			log.Printf("WARNING Failed to determine if it's a new unit %s: %v [SKIPPING]", u.Name, err)
			continue
		}

		if isNew {
			err := d.fleetapi.CreateUnit(u)
			if err != nil {
				return err
			}
			log.Printf("WARNING Failed to create unit %s: %v [SKIPPING]", u.Name, err)
			continue
		}

		isUpdated, err := d.isUpdatedUnit(u)
		if err != nil {
			log.Printf("WARNING Failed to determine if updated unit %s: %v [SKIPPING]", u.Name, err)
			continue
		}

		if !isUpdated {
			continue
		}

		if _, ok := zddUnits[u.Name]; ok {
			if _, ok := deployedUnits[u.Name]; ok {
				continue
			}

			deployed, err := d.performSequentialDeployment(u, zddUnits)
			if err != nil {
				return err
			}
			for k, v := range deployed {
				deployedUnits[k] = v
			}
			continue
		}

		// continue existing handling for normal services
		log.Printf("INFO Service %s differs from the cluster version", u.Name)
		u.DesiredState = "inactive"

		err = d.fleetapi.DestroyUnit(u.Name)
		if err != nil {
			log.Printf("WARNING Failed to destroy unit %s: %v [SKIPPING]", u.Name, err)
			continue
		}

		err = d.fleetapi.CreateUnit(u)
		if err != nil {
			log.Printf("WARNING Failed to create unit %s: %v [SKIPPING]", u.Name, err)
			continue
		}

	}

	currentUnits, err := d.buildCurrentUnits()
	if err != nil {
		return err
	}

	// remove any unwanted units if enabled
	err = d.destroyUnwanted(wantedUnits, currentUnits)
	if err != nil {
		return err
	}
	// launch all units in the cluster
	err = d.launchAll(wantedUnits, currentUnits, zddUnits)
	if err != nil {
		return err
	}

	return nil
}

type loggingFleetAPI struct {
	client.API
}

func (lapi loggingFleetAPI) CreateUnit(unit *schema.Unit) error {
	log.Printf("INFO Creating or updating unit %s\n", unit.Name)
	return lapi.API.CreateUnit(unit)
}

func (lapi loggingFleetAPI) DestroyUnit(unit string) error {
	log.Printf("INFO Destroying unit %s\n", unit)
	return lapi.API.DestroyUnit(unit)
}

func (lapi loggingFleetAPI) SetUnitTargetState(name, desiredState string) error {
	log.Printf("INFO Setting target state for %s to %s\n", name, desiredState)
	return lapi.API.SetUnitTargetState(name, desiredState)
}

type noDestroyFleetAPI struct {
	client.API
}

func (api noDestroyFleetAPI) DestroyUnit(name string) error {
	log.Printf("INFO skipping destroying for unit %v\n", name)
	return nil
}

type deployer struct {
	fleetapi                client.API
	serviceDefinitionClient serviceDefinitionClient
}

func newDeployer() (*deployer, error) {
	u, err := url.Parse(*fleetEndpoint)
	if err != nil {
		return &deployer{}, err
	}
	httpClient := &http.Client{}

	if *socksProxy != "" {
		log.Printf("using proxy %s\n", *socksProxy)
		netDialler := &net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}
		dialer, err := proxy.SOCKS5("tcp", *socksProxy, nil, netDialler)
		if err != nil {
			log.Fatalf("error with proxy %s: %v\n", socksProxy, err)
		}
		httpClient.Transport = &http.Transport{
			Proxy:               http.ProxyFromEnvironment,
			Dial:                dialer.Dial,
			TLSHandshakeTimeout: 10 * time.Second,
		}

	}

	fleetHTTPAPIClient, err := client.NewHTTPClient(httpClient, *u)
	if err != nil {
		return &deployer{}, err
	}
	fleetHTTPAPIClient = loggingFleetAPI{fleetHTTPAPIClient}
	if !*destroyFlag {
		log.Println("destroy not enabled (use -destroy to enable)")
		fleetHTTPAPIClient = noDestroyFleetAPI{fleetHTTPAPIClient}
	}
	serviceDefinitionClient := &httpServiceDefinitionClient{httpClient: &http.Client{}, rootURI: *rootURI}
	return &deployer{fleetapi: fleetHTTPAPIClient, serviceDefinitionClient: serviceDefinitionClient}, nil
}

func buildServiceFileURI(service service, rootURI string) (string, error) {
	if strings.HasPrefix(service.URI, "http") == true {
		return service.URI, nil
	}
	if rootURI == "" {
		return "", errors.New("WARNING Service uri isn't absolute and rootURI not specified")
	}
	uri := fmt.Sprintf("%s%s", rootURI, service.Name)
	return uri, nil
}

func (d *deployer) buildWantedUnits() (map[string]*schema.Unit, map[string]zddInfo, error) {
	units := make(map[string]*schema.Unit)
	zddUnits := make(map[string]zddInfo)

	servicesDefinition, err := d.serviceDefinitionClient.servicesDefinition()
	if err != nil {
		return nil, nil, err
	}

	for _, srv := range servicesDefinition.Services {
		vars := make(map[string]interface{})
		serviceTemplate, err := d.serviceDefinitionClient.serviceFile(srv)
		if err != nil {
			log.Printf("%v", err)
			continue
		}
		vars["version"] = srv.Version
		serviceFile, err := renderedServiceFile(serviceTemplate, vars)
		if err != nil {
			log.Printf("%v", err)
			return nil, nil, err
		}

		// fleet deploy
		uf, err := unit.NewUnitFile(serviceFile)
		if err != nil {
			//Broken service file, skip it and continue
			log.Printf("WARNING service file %s is incorrect: %v [SKIPPING]", srv.Name, err)
			continue
		}

		if srv.Count == 0 && !strings.Contains(srv.Name, "@") {
			u := &schema.Unit{
				Name:         srv.Name,
				Options:      schema.MapUnitFileToSchemaUnitOptions(uf),
				DesiredState: srv.DesiredState,
			}

			units[srv.Name] = u
		} else if srv.Count > 0 && strings.Contains(srv.Name, "@") {
			for i := 0; i < srv.Count; i++ {
				xName := strings.Replace(srv.Name, "@", fmt.Sprintf("@%d", i+1), -1)

				u := &schema.Unit{
					Name:         xName,
					Options:      schema.MapUnitFileToSchemaUnitOptions(uf),
					DesiredState: srv.DesiredState,
				}

				units[u.Name] = u
				if srv.SequentialDeployment {
					zddUnits[u.Name] = zddInfo{u, srv.Count}
				}
			}
		} else {
			log.Printf("WARNING skipping service: %s, incorrect service definition", srv.Name)
		}
	}
	return units, zddUnits, nil
}

func (d *deployer) buildCurrentUnits() (map[string]*schema.Unit, error) {
	all, err := d.fleetapi.Units()
	if err != nil {
		return nil, err
	}

	units := make(map[string]*schema.Unit)
	for _, u := range all {
		units[u.Name] = u
	}
	return units, nil
}
