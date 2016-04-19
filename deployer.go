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
	"github.com/kr/pretty"
)

type deployer struct {
	fleetapi                client.API
	serviceDefinitionClient serviceDefinitionClient
	currentUnits            map[string]*schema.Unit
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
	log.Printf("DEBUG Starting deployAll().")
	log.Printf("DEBUG ")
	wantedServiceGroups, err := d.buildWantedUnits()
	if err != nil {
		return err
	}
	log.Printf("DEBUG: Wanted Service groups \n: [%# v] \n", pretty.Formatter(wantedServiceGroups))

	d.currentUnits, err = d.buildCurrentUnits()
	if err != nil {
		return err
	}

	toCreate := d.identifyNewServiceGroups(wantedServiceGroups)
	toUpdate := d.identifyUpdatedServiceGroups(wantedServiceGroups)
	toDelete := d.identifyDeletedServiceGroups(wantedServiceGroups)

	log.Printf("DEBUG: Service groups to create: [%v]", toCreate)
	log.Printf("DEBUG: Service groups to update: [%v]", toUpdate)
	log.Printf("DEBUG: Service groups to delete: [%v]", toDelete)

	d.createServiceGroups(toCreate)
	d.updateServiceGroups(toUpdate)
	d.deleteServiceGroups(toDelete)

	d.launchAll(wantedServiceGroups)
	log.Printf("DEBUG Finished deployAll().")
	return nil
}

func (d *deployer) identifyNewServiceGroups(serviceGroups map[string]serviceGroup) map[string]serviceGroup {
	log.Printf("DEBUG Started identifyNewServiceGroups().")
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
	log.Printf("DEBUG Finished identifyNewServiceGroups().")
	return newServiceGroups
}

func (d *deployer) identifyUpdatedServiceGroups(serviceGroups map[string]serviceGroup) map[string]serviceGroup {
	log.Printf("DEBUG Started identifyUpdatedServiceGroups().")
	updatedServiceGroups := make(map[string]serviceGroup)
	for name, sg := range serviceGroups {
		isUpdated, err := d.isUpdatedUnit(sg.serviceNodes[0])
		if err != nil {
			log.Printf("WARNING Failed to determine if it's a new unit %s: %v [SKIPPING]", sg.serviceNodes[0].Name, err)
		}
		if isUpdated {
			updatedServiceGroups[name] = sg
		}
		//TODO should a sidekick-only update trigger a service restart too? Only for sequential deployments or generally?
	}
	log.Printf("DEBUG Finished identifyUpdatedServiceGroups().")
	return updatedServiceGroups
}

func (d *deployer) identifyDeletedServiceGroups(wantedServiceGroups map[string]serviceGroup) map[string]serviceGroup {
	log.Printf("DEBUG Started identifyUpdatedServiceGroups().")
	deletedServiceGroups := make(map[string]serviceGroup)
	for _, u := range d.currentUnits {
		serviceName := getServiceName(u.Name)
		if _, ok := wantedServiceGroups[serviceName]; !ok {
			//Do not destroy the deployer itself
			if _, ok := destroyServiceBlacklist[u.Name]; !ok {
				isSidekick := strings.Contains(u.Name, "sidekick")
				deletedServiceGroups = updateServiceGroupMap(u, serviceName, isSidekick, deletedServiceGroups)
			}
		}
	}
	log.Printf("DEBUG Finished identifyUpdatedServiceGroups().")
	return deletedServiceGroups
}

func (d *deployer) createServiceGroups(serviceGroups map[string]serviceGroup) {
	log.Printf("DEBUG Starting createServiceGroups().")
	for _, sg := range serviceGroups {
		for _, u := range sg.serviceNodes {
			if err := d.fleetapi.CreateUnit(u); err != nil {
				log.Printf("WARNING Failed to create unit %s: %v [SKIPPING]", u.Name, err)
				//TODO this handling ok
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
	log.Printf("DEBUG Finished createServiceGroups().")

}

func (d *deployer) updateServiceGroups(serviceGroups map[string]serviceGroup) {
	log.Printf("DEBUG Starting updateServiceGroups().")
	for _, sg := range serviceGroups {
		if sg.isZDD {
			d.performSequentialDeployment(sg)
			continue
		}

		for _, u := range sg.serviceNodes {
			d.updateUnit(u)
		}
		for _, u := range sg.sidekicks {
			d.updateUnit(u)
		}
	}
	log.Printf("DEBUG Finish updateServiceGroups().")
}

func (d *deployer) deleteServiceGroups(serviceGroups map[string]serviceGroup) {
	log.Printf("DEBUG Starting deleteServiceGroups().")
	for _, sg := range serviceGroups {
		for _, u := range sg.serviceNodes {
			if err := d.fleetapi.DestroyUnit(u.Name); err != nil {
				log.Printf("WARNING Failed to destroy unit %s: %v [SKIPPING]", u.Name, err)
				//TODO this handling ok?
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
	log.Printf("DEBUG Finish deleteServiceGroups().")
}

func (d *deployer) buildWantedUnits() (map[string]serviceGroup, error) {
	log.Printf("DEBUG Starting buildWantedUnits()")
	servicesDefinition, err := d.serviceDefinitionClient.servicesDefinition()
	if err != nil {
		log.Printf("ERROR Cannot read services definition: [%v]. \nAborting run!", err)
		return nil, err
	}

	wantedUnits := make(map[string]serviceGroup)
	for _, srv := range servicesDefinition.Services {
		log.Printf("DEBUG Processing [%s].", srv.Name)
		serviceFile, err := d.makeServiceFile(srv)
		if err != nil {
			return nil, err
		}

		uf, err := d.makeUnitFile(serviceFile)
		if err != nil {
			log.Printf("WARNING service file %s is incorrect: %v [SKIPPING]", srv.Name, err)
			continue
		}

		serviceName := getServiceName(srv.Name)
		isSidekick := strings.Contains(srv.Name, "sidekick")

		if srv.Count == 0 && !strings.Contains(srv.Name, "@") {
			log.Printf("DEBUG [%s] non templated service.", serviceName)
			u := buildUnit(srv.Name, uf, srv.DesiredState)
			wantedUnits = updateServiceGroupMap(u, serviceName, isSidekick, wantedUnits)
		} else if srv.Count > 0 && strings.Contains(srv.Name, "@") {
			log.Printf("DEBUG [%s] templated service.", serviceName)
			for i := 0; i < srv.Count; i++ {
				nodeName := strings.Replace(srv.Name, "@", fmt.Sprintf("@%d", i+1), -1)
				u := buildUnit(nodeName, uf, srv.DesiredState)
				wantedUnits = updateServiceGroupMap(u, serviceName, isSidekick, wantedUnits)

				if srv.SequentialDeployment {
					sg, _ := wantedUnits[serviceName]
					sg.isZDD = true
				}
			}
		} else {
			log.Printf("WARNING skipping service: %s, incorrect service definition", srv.Name)
		}
	}
	log.Printf("DEBUG Finish buildWantedUnits()")
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
		d.updateUnit(u)
		if err := d.fleetapi.SetUnitTargetState(u.Name, "launched"); err != nil {
			log.Printf("WARNING Failed to set target state for unit %s: %v [SKIPPING]", u.Name, err)
			continue
		}
		if i == len(sg.serviceNodes) { //this is the last node, we're done
			break
		}

		var unitToWaitOn string
		if len(sg.sidekicks) == 0 {
			unitToWaitOn = u.Name
		} else {
			unitToWaitOn = strings.Replace(u.Name, "@", "-sidekick@", 1)
		}

		// every 30 seconds, check if the unit to wait on is up - for a maximum of 5 minutes
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
	log.Println("DEBUG Starting buildCurrentUnits()")
	all, err := d.fleetapi.Units()
	if err != nil {
		return nil, err
	}

	units := make(map[string]*schema.Unit)
	for _, u := range all {
		units[u.Name] = u
	}
	log.Println("DEBUG Finished buildCurrentUnits()")
	return units, nil
}

func (d *deployer) launchAll(serviceGroups map[string]serviceGroup) error {
	for _, sg := range serviceGroups {
		if sg.isZDD { //they are launched separately
			continue
		}
		for _, u := range sg.serviceNodes {
			d.launchUnit(u)
		}
		for _, u := range sg.sidekicks {
			d.launchUnit(u)
		}
	}
	//TODO handle
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

func getServiceName(unitName string) string {
	if strings.Contains(unitName, "sidekick") {
		return strings.Split(unitName, "-sidekick")[0]
	}
	return strings.Split(unitName, ".service")[0]
}

func (d *deployer) makeServiceFile(s service) (string, error) {
	vars := make(map[string]interface{})
	serviceTemplate, err := d.serviceDefinitionClient.serviceFile(s)
	if err != nil {
		log.Printf("ERROR  Cannot read service file for unit [%s]: %v \nAborting run!", s.Name, err)
		return "", err
	}
	vars["version"] = s.Version
	serviceFile, err := renderedServiceFile(serviceTemplate, vars)
	if err != nil {
		log.Printf("%v", err)
		return "", err
	}
	return serviceFile, nil
}

func (d *deployer) makeUnitFile(serviceFile string) (*unit.UnitFile, error) {
	uf, err := unit.NewUnitFile(serviceFile)
	if err != nil {
		return nil, err
	}

	for _, option := range uf.Options {
		option.Value = strings.Replace(option.Value, "\\\n", " ", -1)
	}
	return uf, nil
}

func (d *deployer) updateUnit(u *schema.Unit) {
	u.DesiredState = "inactive"
	if err := d.fleetapi.DestroyUnit(u.Name); err != nil {
		log.Printf("WARNING Failed to destroy unit %s: %v [SKIPPING]", u.Name, err)
		//TODO this handling ok?
		return
	}

	if err := d.fleetapi.CreateUnit(u); err != nil {
		log.Printf("WARNING Failed to create unit %s: %v [SKIPPING]", u.Name, err)
		return
	}
	u.DesiredState = ""
}

func (d *deployer) launchUnit(u *schema.Unit) {
	if u.DesiredState == "" {
		u.DesiredState = "launched"
	}
	if d.currentUnits[u.Name].DesiredState != u.DesiredState {
		err := d.fleetapi.SetUnitTargetState(u.Name, u.DesiredState)
		if err != nil {
			//TODO log
		}
	}
}

func updateServiceGroupMap(u *schema.Unit, serviceName string, isSidekick bool, serviceGroups map[string]serviceGroup) map[string]serviceGroup {
	if sg, ok := serviceGroups[serviceName]; ok {
		if isSidekick {
			sg.sidekicks = append(sg.sidekicks, u)
		} else {
			sg.serviceNodes = append(sg.serviceNodes, u)
		}
	} else {
		if isSidekick {
			serviceGroups[serviceName] = serviceGroup{serviceNodes: []*schema.Unit{}, sidekicks: []*schema.Unit{u}}
		} else {
			serviceGroups[serviceName] = serviceGroup{serviceNodes: []*schema.Unit{u}, sidekicks: []*schema.Unit{}}
		}
	}
	return serviceGroups
}

func buildUnit(name string, uf *unit.UnitFile, desiredState string) *schema.Unit {
	return &schema.Unit{
		Name:         name,
		Options:      schema.MapUnitFileToSchemaUnitOptions(uf),
		DesiredState: desiredState,
	}
}
