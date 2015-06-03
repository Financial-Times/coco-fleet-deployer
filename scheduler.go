package main

import (
	"errors"
	"flag"
	"fmt"
	"github.com/coreos/fleet/client"
	"github.com/coreos/fleet/schema"
	"github.com/coreos/fleet/unit"
	"github.com/hoisie/mustache"
	"gopkg.in/yaml.v2"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"strings"
)

type services struct {
	Services []service `yaml:"services"`
}

type service struct {
	Name    string `yaml:"name"`
	Version string `yaml:"version"`
	Count   int    `yaml:"count"`
}

func check(e error) {
	if e != nil {
		panic(e)
	}
}

func getServiceDefinition() (services services) {
	serviceYaml, err := ioutil.ReadFile("services.yaml")
	check(err)
	err = yaml.Unmarshal(serviceYaml, &services)
	check(err)
	return services
}

func main() {
	destroyFlag := flag.Bool("destroy", false, "Destroy units not found in the definition")
	fleetEndpoint := flag.String("fleetEndpoint", "", "Fleet API http endpoint: `http://host:port`")
	flag.Parse()
	if *fleetEndpoint == "" {
		log.Fatal("Fleet endpoint is required")
	}

	d, err := newDeployer(fleetEndpoint, *destroyFlag)
	check(err)

	err = d.deployAll()
	check(err)
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
	if wuf.String() != cuf.String() {
		log.Printf("Service %s differs from the cluster version", wantedUnit.Name)
		err := d.fleetapi.CreateUnit(wantedUnit)
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
			err := d.fleetapi.DestroyUnit(u.Name)
			if err != nil {
				return err
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
		if u.CurrentState != "launched" {
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
	log.Printf("Creating or updating unit %s\n", unit.Name)
	return lapi.API.CreateUnit(unit)
}

func (lapi loggingFleetAPI) DestroyUnit(unit string) error {
	log.Printf("Destroying unit %s\n", unit)
	return lapi.API.DestroyUnit(unit)
}

func (lapi loggingFleetAPI) SetUnitTargetState(name, desiredState string) error {
	log.Printf("Setting target state for %s to %s\n", name, desiredState)
	return lapi.API.SetUnitTargetState(name, desiredState)
}

type noDestroyFleetAPI struct {
	client.API
}

func (api noDestroyFleetAPI) DestroyUnit(name string) error {
	log.Printf("skipping destroying for unit %v\n", name)
	return nil
}

func newDeployer(fleetEndpoint *string, destroyFlag bool) (deployer, error) {
	u, err := url.Parse(*fleetEndpoint)
	if err != nil {
		return deployer{}, err
	}
	httpClient := &http.Client{}
	hc, err := client.NewHTTPClient(httpClient, *u)
	if err != nil {
		return deployer{}, err
	}
	hc = loggingFleetAPI{hc}
	if !destroyFlag {
		log.Println("destroy not enabled (use -destroy to enable)")
		hc = noDestroyFleetAPI{hc}
	}
	return deployer{httpClient, hc}, nil
}

type deployer struct {
	httpClient *http.Client
	fleetapi   client.API
}

func (d *deployer) buildWantedUnits() (map[string]*schema.Unit, error) {
	units := make(map[string]*schema.Unit)
	for _, srv := range getServiceDefinition().Services {
		vars := make(map[string]interface{})
		vars["version"] = srv.Version
		serviceFile, err := d.renderServiceFile(srv.Name, vars)
		if err != nil {
			return nil, err
		}

		// fleet deploy
		uf, err := unit.NewUnitFile(serviceFile)
		if err != nil {
			return nil, err
		}

		if srv.Count == 0 {
			u := &schema.Unit{
				//	DesiredState: "launched",
				Name:    srv.Name,
				Options: schema.MapUnitFileToSchemaUnitOptions(uf),
			}

			units[srv.Name] = u
		} else {
			if !strings.Contains(srv.Name, "@") {
				return nil, errors.New("instances specified on non-template service file")
			}
			for i := 0; i < srv.Count; i++ {
				xName := strings.Replace(srv.Name, "@", fmt.Sprintf("@%d", i+1), -1)

				u := &schema.Unit{
					//	DesiredState: "launched",
					Name:    xName,
					Options: schema.MapUnitFileToSchemaUnitOptions(uf),
				}

				units[u.Name] = u
			}
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

func (d *deployer) renderServiceFile(name string, context ...interface{}) (string, error) {
	resp, err := d.httpClient.Get(fmt.Sprintf("https://raw.githubusercontent.com/Financial-Times/fleet/master/service-files/%s", name))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	serviceTemplate, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	tmpl, err := mustache.ParseString(string(serviceTemplate))
	if err != nil {
		return "", err
	}
	return tmpl.Render(context...), nil
}

func renderMustache(filename string, context ...interface{}) (string, error) {
	tmpl, err := mustache.ParseFile(filename)
	if err != nil {
		return "", err
	}
	return tmpl.Render(context...), nil
}
