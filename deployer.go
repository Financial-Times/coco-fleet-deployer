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
	Name         string `yaml:"name"`
	Version      string `yaml:"version"`
	Count        int    `yaml:"count"`
	URI          string `yaml:"uri"`
	DesiredState string `yaml:"desiredState"`
}

type serviceDefinitionClient interface {
	servicesDefinition() (services, error)
	serviceFile(service service) ([]byte, error)
}

type httpServiceDefinitionClient struct {
	httpClient *http.Client
	rootURI    string
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

func (d *deployer) deployUnit(wantedUnit *schema.Unit) error {
	currentUnit, err := d.fleetapi.Unit(wantedUnit.Name)
	if err != nil {
		return err
	}
	if currentUnit == nil {
		err := d.fleetapi.CreateUnit(wantedUnit)
		if err != nil {
			return err
		}
		return nil
	}

	wuf := schema.MapSchemaUnitOptionsToUnitFile(wantedUnit.Options)
	cuf := schema.MapSchemaUnitOptionsToUnitFile(currentUnit.Options)
	if wuf.Hash() != cuf.Hash() {
		log.Printf("INFO Service %s differs from the cluster version", wantedUnit.Name)
		wantedUnit.DesiredState = "inactive"
		err = d.fleetapi.DestroyUnit(wantedUnit.Name)
		if err != nil {
			return err
		}
		err = d.fleetapi.CreateUnit(wantedUnit)
		if err != nil {
			return err
		}
	}
	return nil
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

func (d *deployer) launchAll(wantedUnits, currentUnits map[string]*schema.Unit, serviceCount map[string]int) error {
	deployedUnits := make(map[string]bool)

	for _, u := range wantedUnits {
		// unit may have already been deployed - all nodes of a service get deployed,
		// when the first one appears in the wanted list
		if _, ok := deployedUnits[u.Name]; ok {
			continue
		}

		if u.DesiredState == "" {
			u.DesiredState = "launched"
		}

		//units that are changed are set to Inactive in deployUnit()
		if currentUnits[u.Name].DesiredState != u.DesiredState {

			if !needsSequentialDeployment(u.Name, serviceCount) {
				err := d.fleetapi.SetUnitTargetState(u.Name, u.DesiredState)
				if err != nil {
					return err
				}
				continue
			}

			deployed, err := d.performSequentialDeployment(u, serviceCount)
			if err != nil {
				return err
			}
			for k, v := range deployed {
				deployedUnits[k] = v
			}
		}
	}

	return nil
}

func (d *deployer) performSequentialDeployment(u *schema.Unit, serviceCount map[string]int) (map[string]bool, error) {
	deployedUnits := make(map[string]bool)
	serviceName := strings.Split(u.Name, "@")[0]
	nrOfNodes := serviceCount[serviceName]

	for i := 1; i <= nrOfNodes; i++ {
		// generate unit name with number - the one we have originally might not be
		// the 1st node
		unitName := fmt.Sprintf("%v@%d%v", serviceName, i, strings.Split(u.Name, ".")[1])

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
		// we do this for a maximum of 1 minute
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
		}
	}
	return deployedUnits, nil
}

// Sidekicks and single-node applications don't need sequential deployment
func needsSequentialDeployment(unitName string, serviceCount map[string]int) bool {
	if strings.Contains(unitName, "sidekick") {
		return false
	}

	serviceName := strings.Split(unitName, "@")[0]
	if _, ok := serviceCount[serviceName]; ok {
		return true
	}

	return false
}

func (d *deployer) deployAll() error {
	// Get service definition - wanted units
	//get whitelist
	wantedUnits, serviceCount, err := d.buildWantedUnits()

	if err != nil {
		return err
	}

	// create any missing units
	for _, u := range wantedUnits {
		err = d.deployUnit(u) //replaces old with new units, stops sk
		if err != nil {
			log.Printf("WARNING Failed to deploy unit %s: %v [SKIPPING]", u.Name, err)
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
	err = d.launchAll(wantedUnits, currentUnits, serviceCount)
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

func (d *deployer) buildWantedUnits() (map[string]*schema.Unit, map[string]int, error) {
	units := make(map[string]*schema.Unit)
	serviceCount := make(map[string]int)

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
				serviceCount[strings.Split(srv.Name, "@")[0]] = srv.Count
			}
		} else {
			log.Printf("WARNING skipping service: %s, incorrect service definition", srv.Name)
		}
	}
	return units, serviceCount, nil
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
