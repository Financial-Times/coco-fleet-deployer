package main

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"regexp"

	"github.com/coreos/fleet/client"
	"github.com/coreos/fleet/schema"
	"github.com/coreos/fleet/unit"
	"golang.org/x/net/proxy"
)

type deployer struct {
	fleetapi                client.API
	serviceDefinitionClient serviceDefinitionClient
}

var whitespaceMatcher, _ = regexp.Compile("\\s+")

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
			log.Fatalf("error with proxy %s: %v\n", *socksProxy, err)
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

func (d *deployer) deployAll() error {
	serviceGroups, err := d.buildWantedUnits()
	if err != nil {
		return err
	}

	toCreate := d.identifyNewServiceGroups(serviceGroups)
	toUpdate := d.identifyUpdatedServiceGroups(serviceGroups)
	toDelete := d.identifyDeletedServiceGroups(serviceGroups)

	d.createServiceGroups(toCreate)
	d.updateServiceGroups(toUpdate)
	d.deleteServiceGroups(toDelete)

	d.launchAll(serviceGroups)
	return nil
}

func (d *deployer) identifyNewServiceGroups(serviceGroups map[string]serviceGroup) map[string]serviceGroup {
	newServiceGroups := make(map[string]serviceGroup)
	for name, sg := range serviceGroups {
		isNew, err := d.isNewUnit(sg.serviceNodes[0])
		if err != nil {
			log.Printf("WARNING Failed to determine if it's a new unit %s: %v [SKIPPING]", sg.serviceNodes[0].Name, err)
		}
		if isNew {
			newServiceGroups[name] = sg
		}
	}
	return newServiceGroups
}

func (d *deployer) identifyUpdatedServiceGroups(serviceGroups map[string]serviceGroup) map[string]serviceGroup {
	updateDServiceGroups := make(map[string]serviceGroup)
	for name, sg := range serviceGroups {
		isNew, err := d.isUpdatedUnit(sg.serviceNodes[0])
		if err != nil {
			log.Printf("WARNING Failed to determine if it's a new unit %s: %v [SKIPPING]", sg.serviceNodes[0].Name, err)
		}
		if isNew {
			updateDServiceGroups[name] = sg
		}
	}
	return updateDServiceGroups
}

func (d *deployer) identifyDeletedServiceGroups(serviceGroups map[string]serviceGroup) map[string]serviceGroup {
	deletedServiceGroups := make(map[string]serviceGroup)
	currentUnits, err := d.buildCurrentUnits()
	if err != nil {
		return deletedServiceGroups
	}

	for name, u := range currentUnits {
		var serviceName string
		if strings.Contains(u.Name, "sidekick") {
			serviceName = strings.Split(u.Name, "-sidekick")[0]
		} else {
			serviceName = strings.Split(u.Name, ".service")[0]
		}

		if sg, ok := serviceGroups[serviceName]; !ok {
			//Do not destroy the deployer itself
			if _, ok := destroyServiceBlacklist[u.Name]; !ok {
				deletedServiceGroups[name] = sg
			}
		}
	}
	return deletedServiceGroups
}

func (d *deployer) createServiceGroups(serviceGroups map[string]serviceGroup) {
	for _, sg := range serviceGroups {
		for _, u := range sg.serviceNodes {
			if err := d.fleetapi.CreateUnit(u); err != nil {
				log.Printf("WARNING Failed to create unit %s: %v [SKIPPING]", u.Name, err)
				continue
			}
		}
		for _, u := range sg.sidekicks {
			if err := d.fleetapi.CreateUnit(u); err != nil {
				log.Printf("WARNING Failed to create unit %s: %v [SKIPPING]", u.Name, err)
				continue
			}
		}
	}

}

func (d *deployer) updateServiceGroups(serviceGroups map[string]serviceGroup) {
	for _, sg := range serviceGroups {
		if !sg.isZDD {
			for _, u := range sg.serviceNodes {
				u.DesiredState = "inactive"
				if err := d.fleetapi.DestroyUnit(u.Name); err != nil {
					log.Printf("WARNING Failed to destroy unit %s: %v [SKIPPING]", u.Name, err)
					continue
				}

				if err := d.fleetapi.CreateUnit(u); err != nil {
					log.Printf("WARNING Failed to create unit %s: %v [SKIPPING]", u.Name, err)
					continue
				}
				u.DesiredState = ""
			}
			for _, u := range sg.sidekicks {
				u.DesiredState = "inactive"
				if err := d.fleetapi.DestroyUnit(u.Name); err != nil {
					log.Printf("WARNING Failed to destroy unit %s: %v [SKIPPING]", u.Name, err)
					continue
				}

				if err := d.fleetapi.CreateUnit(u); err != nil {
					log.Printf("WARNING Failed to create unit %s: %v [SKIPPING]", u.Name, err)
					continue
				}
				u.DesiredState = ""
			}
		} else {
			d.performSequentialDeployment(sg)
		}
	}
}

func (d *deployer) deleteServiceGroups(serviceGroups map[string]serviceGroup) {
	for _, sg := range serviceGroups {
		for _, u := range sg.serviceNodes {
			if err := d.fleetapi.DestroyUnit(u.Name); err != nil {
				log.Printf("WARNING Failed to destroy unit %s: %v [SKIPPING]", u.Name, err)
				continue
			}
		}
		for _, u := range sg.sidekicks {
			if err := d.fleetapi.DestroyUnit(u.Name); err != nil {
				log.Printf("WARNING Failed to destroy unit %s: %v [SKIPPING]", u.Name, err)
				continue
			}
		}
	}
}

func (d *deployer) buildWantedUnits() (map[string]serviceGroup, error) {
	servicesDefinition, err := d.serviceDefinitionClient.servicesDefinition()
	if err != nil {
		log.Printf("ERROR Cannot read services definition: [%v]. \nAborting run!", err)
		return nil, err
	}

	wantedUnits := make(map[string]serviceGroup)
	for _, srv := range servicesDefinition.Services {
		var serviceName string
		var isSidekick = false
		if strings.Contains(srv.Name, "sidekick") {
			serviceName = strings.Split(srv.Name, "-sidekick")[0]
			isSidekick = true
		} else {
			serviceName = strings.Split(srv.Name, ".service")[0]
		}

		vars := make(map[string]interface{})
		serviceTemplate, err := d.serviceDefinitionClient.serviceFile(srv)
		if err != nil {
			log.Printf("ERROR  Cannot read service file for unit [%s]: %v \nAborting run!", srv.Name, err)
			return nil, err
		}
		vars["version"] = srv.Version
		serviceFile, err := renderedServiceFile(serviceTemplate, vars)
		if err != nil {
			log.Printf("%v", err)
			return nil, err
		}

		// fleet deploy
		uf, err := unit.NewUnitFile(serviceFile)
		if err != nil {
			//Broken service file, skip it and continue
			log.Printf("WARNING service file %s is incorrect: %v [SKIPPING]", srv.Name, err)
			continue
		}

		for _, option := range uf.Options {
			option.Value = strings.Replace(option.Value, "\\\n", " ", -1)
		}

		if srv.Count == 0 && !strings.Contains(srv.Name, "@") {
			u := &schema.Unit{
				Name:         srv.Name,
				Options:      schema.MapUnitFileToSchemaUnitOptions(uf),
				DesiredState: srv.DesiredState,
			}
			if sg, ok := wantedUnits[serviceName]; ok {
				if isSidekick {
					sg.sidekicks = append(sg.sidekicks, u)
				} else {
					sg.serviceNodes = append(sg.serviceNodes, u)
				}
			} else {
				if isSidekick {
					wantedUnits[serviceName] = serviceGroup{serviceNodes: []*schema.Unit{}, sidekicks: []*schema.Unit{u}}
				} else {
					wantedUnits[serviceName] = serviceGroup{serviceNodes: []*schema.Unit{u}, sidekicks: []*schema.Unit{}}
				}
			}
		} else if srv.Count > 0 && strings.Contains(srv.Name, "@") {
			for i := 0; i < srv.Count; i++ {
				xName := strings.Replace(srv.Name, "@", fmt.Sprintf("@%d", i+1), -1)
				u := &schema.Unit{
					Name:         xName,
					Options:      schema.MapUnitFileToSchemaUnitOptions(uf),
					DesiredState: srv.DesiredState,
				}
				if sg, ok := wantedUnits[serviceName]; ok {
					if isSidekick {
						sg.sidekicks = append(sg.sidekicks, u)
					} else {
						sg.serviceNodes = append(sg.serviceNodes, u)
					}
				} else {
					if isSidekick {
						wantedUnits[serviceName] = serviceGroup{serviceNodes: []*schema.Unit{}, sidekicks: []*schema.Unit{u}}
					} else {
						wantedUnits[serviceName] = serviceGroup{serviceNodes: []*schema.Unit{u}, sidekicks: []*schema.Unit{}}
					}
				}
				if srv.SequentialDeployment {
					sg, _ := wantedUnits[serviceName]
					sg.isZDD = true
				}
			}
		} else {
			log.Printf("WARNING skipping service: %s, incorrect service definition", srv.Name)
		}
	}
	return wantedUnits, nil
}

func (d *deployer) isNewUnit(u *schema.Unit) (bool, error) {
	currentUnit, err := d.fleetapi.Unit(u.Name)
	if err != nil {
		return false, err
	}
	return currentUnit == nil, nil
}

func (d *deployer) performSequentialDeployment(sg serviceGroup) {
	for i, u := range sg.serviceNodes {
		if u.DesiredState == "" {
			u.DesiredState = "launched"
		}

		if err := d.fleetapi.DestroyUnit(u.Name); err != nil {
			log.Printf("WARNING Failed to destroy unit %s: %v [SKIPPING]", u.Name, err)
			continue
		}
		if err := d.fleetapi.CreateUnit(u); err != nil {
			log.Printf("WARNING Failed to create unit %s: %v [SKIPPING]", u.Name, err)
			continue
		}
		if err := d.fleetapi.SetUnitTargetState(u.Name, u.DesiredState); err != nil {
			log.Printf("WARNING Failed to set target state for unit %s: %v [SKIPPING]", u.Name, err)
			continue
		}
		if i == len(sg.serviceNodes) {
			//this is the last node, we're done
			break
		}

		var unitToWaitOn string
		if len(sg.sidekicks) == 0 {
			unitToWaitOn = u.Name
		} else {
			unitToWaitOn = strings.Replace(u.Name, "@", "-sidekick@", 1)
		}

		// every 30 seconds, check if the corresponding sidekick is up
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
				unitStatus, err := d.fleetapi.Unit(unitToWaitOn)
				if err != nil {
					log.Printf("WARNING Failed to get unit %s: %v [SKIPPING]", u.Name, err)
					continue
				}

				log.Printf("INFO UnitToWaitOn status: [%v]\n", unitStatus.CurrentState)
				if unitStatus.CurrentState == "launched" {
					tickerChan.Stop()
					break
				}
				continue
			case <-timeoutChan:
				tickerChan.Stop()
				log.Printf("WARN Service [%v] didn't start up in time", u.Name)
				break
			}
			break
		}
	}

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

func (d *deployer) launchAll(serviceGroups map[string]serviceGroup) error {
	currentUnits, err := d.buildCurrentUnits()
	if err != nil {
		return err
	}
	for _, sg := range serviceGroups {
		if sg.isZDD {
			continue
		}
		for _, u := range sg.serviceNodes {
			if u.DesiredState == "" {
				u.DesiredState = "launched"
			}
			if currentUnits[u.Name].DesiredState != u.DesiredState {
				err := d.fleetapi.SetUnitTargetState(u.Name, u.DesiredState)
				if err != nil {
					return err
				}
			}
		}
		for _, u := range sg.sidekicks {
			if u.DesiredState == "" {
				u.DesiredState = "launched"
			}
			if currentUnits[u.Name].DesiredState != u.DesiredState {
				err := d.fleetapi.SetUnitTargetState(u.Name, u.DesiredState)
				if err != nil {
					return err
				}
			}
		}
	}

	return nil
}

func (d *deployer) isUpdatedUnit(newUnit *schema.Unit) (bool, error) {
	currentUnit, err := d.fleetapi.Unit(newUnit.Name)
	if err != nil {
		return false, err
	}

	nuf := schema.MapSchemaUnitOptionsToUnitFile(newUnit.Options)
	cuf := schema.MapSchemaUnitOptionsToUnitFile(currentUnit.Options)

	for _, option := range nuf.Options {
		option.Value = whitespaceMatcher.ReplaceAllString(option.Value, " ")
	}
	for _, option := range cuf.Options {
		option.Value = whitespaceMatcher.ReplaceAllString(option.Value, " ")
	}

	return nuf.Hash() != cuf.Hash(), nil
}

func renderedServiceFile(serviceTemplate []byte, context map[string]interface{}) (string, error) {
	if context["version"] == "" {
		return string(serviceTemplate), nil
	}
	versionString := fmt.Sprintf("DOCKER_APP_VERSION=%s", context["version"])
	serviceTemplateString := strings.Replace(string(serviceTemplate), "DOCKER_APP_VERSION=latest", versionString, 1)
	return serviceTemplateString, nil
}
