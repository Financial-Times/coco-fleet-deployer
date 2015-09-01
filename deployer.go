package main

import (
	"flag"
	"fmt"
	"github.com/coreos/fleet/client"
	"github.com/coreos/fleet/schema"
	"github.com/coreos/fleet/unit"
	"golang.org/x/net/proxy"
	"gopkg.in/yaml.v2"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/url"
	"runtime/debug"
	"strings"
	"time"
)

var (
	destroyFlag               = flag.Bool("destroy", false, "Destroy units not found in the definition")
	fleetEndpoint             = flag.String("fleetEndpoint", "", "Fleet API http endpoint: `http://host:port`")
	servicesDefinitionFileUri = flag.String("servicesDefinitionFileUri", "", "URI file that contains services definition: `https://raw.githubusercontent.com/Financial-Times/fleet/master/services.yaml`")
	socksProxy                = flag.String("socksProxy", "", "address of socks proxy, e.g., 127.0.0.1:9050")
	destroyServiceBlacklist   = map[string]struct{}{"deployer.service": struct{}{}, "deployer.timer": struct{}{}}
)

type services struct {
	Services []service `yaml:"services"`
}

type service struct {
	Name    string `yaml:"name"`
	Version string `yaml:"version"`
	Count   int    `yaml:"count"`
	Uri     string `yaml:"uri"`
}

type serviceDefinitionClient interface {
	servicesDefinition() (services, error)
	serviceFile(serviceFileUri string) ([]byte, error)
}

type httpServiceDefinitionClient struct {
	httpClient      *http.Client
	serviceFilesUri string
}

func renderServiceDefinitionYaml(serviceYaml []byte) (services services, err error) {
	if err = yaml.Unmarshal(serviceYaml, &services); err != nil {
		panic(err)
	}
	return
}

func (hsdc *httpServiceDefinitionClient) servicesDefinition() (services, error) {
	resp, err := hsdc.httpClient.Get(*servicesDefinitionFileUri)
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()

	serviceYaml, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		panic(err)
	}
	return renderServiceDefinitionYaml(serviceYaml)
}

func (hsdc *httpServiceDefinitionClient) serviceFile(serviceFileUri string) ([]byte, error) {
	resp, err := hsdc.httpClient.Get(serviceFileUri)
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
	version_string := fmt.Sprintf("DOCKER_APP_VERSION=%s", context["version"])
	serviceTemplateString := strings.Replace(string(serviceTemplate), "DOCKER_APP_VERSION=latest", version_string, 1)
	return serviceTemplateString, nil
}

func main() {
	flag.Parse()
	if *fleetEndpoint == "" {
		log.Fatal("Fleet endpoint is required")
	}

	if *servicesDefinitionFileUri == "" {
		log.Fatal("Services definition file uri is required")
	}

	d, err := newDeployer()
	if err != nil {
		panic(err)
	}

	log.Printf("INFO Starting deploy run")
	if err := d.deployAll(); err != nil {
		panic(err)
	}
	log.Printf("INFO Finished deploy run")
}

func deployAndRecover(d *deployer) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("ERROR Recovered: %s: %s", r, debug.Stack())
		}
	}()

	if err := d.deployAll(); err != nil {
		panic(err)
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

func (d *deployer) destroyUnwanted(wantedUnits map[string]*schema.Unit) error {
	currentUnits, err := d.buildCurrentUnits()
	if err != nil {
		return err
	}
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

func (d *deployer) launchAll() error {
	currentUnits, err := d.buildCurrentUnits()
	if err != nil {
		return err
	}

	// start everything that's not started
	for _, u := range currentUnits {
		if u.DesiredState != "launched" {
			log.Printf("INFO Desired state: %s", u.DesiredState)
			err := d.fleetapi.SetUnitTargetState(u.Name, "launched")
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (d *deployer) deployAll() error {
	// Get service definition - wanted units
	wantedUnits, err := d.buildWantedUnits()

	if err != nil {
		return err
	}

	// create any missing units
	for _, u := range wantedUnits {
		err = d.deployUnit(u)
		if err != nil {
			return err
		}
	}

	// remove any unwanted units if enabled
	err = d.destroyUnwanted(wantedUnits)
	if err != nil {
		return err
	}

	// launch all units in the cluster
	err = d.launchAll()
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
			log.Fatal("error with proxy %s: %v\n", socksProxy, err)
		}
		httpClient.Transport = &http.Transport{
			Proxy:               http.ProxyFromEnvironment,
			Dial:                dialer.Dial,
			TLSHandshakeTimeout: 10 * time.Second,
		}

	}

	fleetHttpApiClient, err := client.NewHTTPClient(httpClient, *u)
	if err != nil {
		return &deployer{}, err
	}
	fleetHttpApiClient = loggingFleetAPI{fleetHttpApiClient}
	if !*destroyFlag {
		log.Println("destroy not enabled (use -destroy to enable)")
		fleetHttpApiClient = noDestroyFleetAPI{fleetHttpApiClient}
	}
	serviceDefinitionClient := &httpServiceDefinitionClient{httpClient: &http.Client{}}
	return &deployer{fleetapi: fleetHttpApiClient, serviceDefinitionClient: serviceDefinitionClient}, nil
}

func (d *deployer) buildWantedUnits() (map[string]*schema.Unit, error) {
	units := make(map[string]*schema.Unit)
	servicesDefinition, err := d.serviceDefinitionClient.servicesDefinition()
	if err != nil {
		return nil, err
	}
	for _, srv := range servicesDefinition.Services {
		vars := make(map[string]interface{})
		if srv.Uri == "" {
			log.Printf("WARNING no uri for service: %s", srv.Name)
			continue
		}
		serviceTemplate, err := d.serviceDefinitionClient.serviceFile(srv.Uri)
		if err != nil {
			continue
		}
		vars["version"] = srv.Version
		serviceFile, err := renderedServiceFile(serviceTemplate, vars)
		if err != nil {
			return nil, err
		}

		// fleet deploy
		uf, err := unit.NewUnitFile(serviceFile)
		if err != nil {
			//Broken service file, skip it and continue
			log.Printf("WARNING: service file %s is incorrect, check syntax [SKIPPING]", srv.Name)
			continue
		}

		if srv.Count == 0 && !strings.Contains(srv.Name, "@") {
			u := &schema.Unit{
				Name:    srv.Name,
				Options: schema.MapUnitFileToSchemaUnitOptions(uf),
			}

			units[srv.Name] = u
		} else if srv.Count > 0 && strings.Contains(srv.Name, "@") {
			for i := 0; i < srv.Count; i++ {
				xName := strings.Replace(srv.Name, "@", fmt.Sprintf("@%d", i+1), -1)

				u := &schema.Unit{
					Name:    xName,
					Options: schema.MapUnitFileToSchemaUnitOptions(uf),
				}

				units[u.Name] = u
			}
		} else {
			log.Printf("WARNING skipping service: %s, incorrect service definition", srv.Name)
		}
	}
	return units, nil
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
