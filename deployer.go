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
	unitCache               map[string]unit.Hash
}

const launchedState = "launched"
const inactiveState = "inactive"
const launchTimeout = time.Duration(5) * time.Minute
const stateCheckInterval = time.Duration(30) * time.Second

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
	if d.unitCache == nil {
		uc, err := d.buildUnitCache()
		if err != nil {
			log.Printf("Cannot build unit cache, aborting run: [%v]", err)
			return err
		}
		d.unitCache = uc
	}

	wantedServiceGroups, err := d.buildWantedUnits()
	if err != nil {
		return err
	}

	toDelete := d.identifyDeletedServiceGroups(wantedServiceGroups)
	toCreate := d.identifyNewServiceGroups(wantedServiceGroups)
	purgeProcessed(wantedServiceGroups, toCreate)
	toUpdateRegular, toUpdateSKRegular, toUpdateSequentially, toUpdateSKSequentially := d.identifyUpdatedServiceGroups(wantedServiceGroups)

	d.createServiceGroups(toCreate)
	d.updateServiceGroupsNormally(toUpdateRegular)
	d.updateServiceGroupsSequentially(toUpdateSequentially)
	d.updateServiceGroupsSKsOnly(toUpdateSKRegular)
	d.updateServiceGroupsSKsSequentially(toUpdateSKSequentially)
	d.deleteServiceGroups(toDelete)

	d.updateCache(mergeMaps(toCreate, toUpdateRegular, toUpdateSequentially, toUpdateSKRegular, toUpdateSKSequentially))
	d.removeFromCache(toDelete)

	toLaunch := mergeMaps(toCreate, toUpdateRegular, toUpdateSKRegular)
	err = d.launchAll(toLaunch)
	if err != nil {
		log.Printf("ERROR Cannot launch all services: [%v]", err)
	}
	return nil
}

func (d *deployer) updateCache(serviceGroups map[string]serviceGroup) {
	for _, sg := range serviceGroups {
		for _, u := range sg.serviceNodes {
			d.unitCache[u.Name] = getUnitHash(u)
		}
		for _, u := range sg.sidekicks {
			d.unitCache[u.Name] = getUnitHash(u)
		}
	}
}

func (d *deployer) removeFromCache(serviceGroups map[string]serviceGroup) {
	for _, sg := range serviceGroups {
		for _, u := range sg.serviceNodes {
			delete(d.unitCache, u.Name)
		}
		for _, u := range sg.sidekicks {
			delete(d.unitCache, u.Name)
		}
	}
}

func (d *deployer) buildUnitCache() (map[string]unit.Hash, error) {
	units, err := d.fleetapi.Units()
	if err != nil {
		return nil, err
	}

	unitCache := make(map[string]unit.Hash)
	for _, unit := range units {
		unitCache[unit.Name] = getUnitHash(unit)
	}
	return unitCache, nil
}

func (d *deployer) identifyNewServiceGroups(serviceGroups map[string]serviceGroup) map[string]serviceGroup {
	newServiceGroups := make(map[string]serviceGroup)
	for name, sg := range serviceGroups {
		if d.isNewUnit(sg.serviceNodes[0]) {
			newServiceGroups[name] = sg
		}
	}
	return newServiceGroups
}

func (d *deployer) identifyUpdatedServiceGroups(serviceGroups map[string]serviceGroup) (map[string]serviceGroup, map[string]serviceGroup, map[string]serviceGroup, map[string]serviceGroup) {
	updatedRegular := make(map[string]serviceGroup)
	skUpdatedRegular := make(map[string]serviceGroup)
	updatedSequential := make(map[string]serviceGroup)
	skUpdatedSequential := make(map[string]serviceGroup)

	for name, sg := range serviceGroups {
		if d.isUpdatedUnit(sg.serviceNodes[0]) {
			if sg.isZDD {
				updatedSequential[name] = sg
			} else {
				updatedRegular[name] = sg
			}
			continue
		}

		if len(sg.sidekicks) == 0 {
			continue
		}

		if d.isUpdatedUnit(sg.sidekicks[0]) {
			if sg.isZDD {
				skUpdatedSequential[name] = sg
			} else {
				skUpdatedRegular[name] = sg
			}
			continue
		}
	}
	return updatedRegular, skUpdatedRegular, updatedSequential, skUpdatedSequential
}

func (d *deployer) identifyDeletedServiceGroups(wantedServiceGroups map[string]serviceGroup) map[string]serviceGroup {
	deletedServiceGroups := make(map[string]serviceGroup)
	currentUnits, err := d.buildCurrentUnits()
	if err != nil {
		log.Printf("ERROR: Cannot build current units list -> Cannot identify deleted services: [%v]", err)
		return make(map[string]serviceGroup)
	}

	for _, u := range currentUnits {
		serviceName := getServiceName(u.Name)
		if _, ok := wantedServiceGroups[serviceName]; !ok {
			//Do not destroy the deployer itself
			if _, ok := destroyServiceBlacklist[u.Name]; !ok {
				isSidekick := strings.Contains(u.Name, "sidekick")
				deletedServiceGroups = updateServiceGroupMap(u, serviceName, isSidekick, deletedServiceGroups)
			}
		}
	}

	return deletedServiceGroups
}

func (d *deployer) createServiceGroups(serviceGroups map[string]serviceGroup) {
	for sgName, sg := range serviceGroups {
		log.Printf("INFO Creating SG [%s].", sgName)
		for _, u := range sg.serviceNodes {
			d.fleetapi.CreateUnit(u)
		}
		for _, u := range sg.sidekicks {
			d.fleetapi.CreateUnit(u)
		}
	}
}

func (d *deployer) updateServiceGroupsSKsOnly(serviceGroups map[string]serviceGroup) {
	for _, sg := range serviceGroups {
		for _, u := range sg.sidekicks {
			d.updateUnit(u)
		}
	}
}

func (d *deployer) updateServiceGroupsNormally(serviceGroups map[string]serviceGroup) {
	for _, sg := range serviceGroups {
		for _, u := range sg.serviceNodes {
			d.updateUnit(u)
		}
		for _, u := range sg.sidekicks {
			d.updateUnit(u)
		}
	}
}

func (d *deployer) updateServiceGroupsSequentially(serviceGroups map[string]serviceGroup) {
	for _, sg := range serviceGroups {
		d.performSequentialDeployment(sg)
	}
}

func (d *deployer) updateServiceGroupsSKsSequentially(serviceGroups map[string]serviceGroup) {
	for _, sg := range serviceGroups {
		d.performSequentialDeploymentSK(sg)
	}
}

func (d *deployer) deleteServiceGroups(serviceGroups map[string]serviceGroup) {
	for _, sg := range serviceGroups {
		for _, u := range sg.serviceNodes {
			d.fleetapi.DestroyUnit(u.Name)
		}
		for _, u := range sg.sidekicks {
			d.fleetapi.DestroyUnit(u.Name)
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
			u := buildUnit(srv.Name, uf, srv.DesiredState)
			wantedUnits = updateServiceGroupMap(u, serviceName, isSidekick, wantedUnits)
		} else if srv.Count > 0 && strings.Contains(srv.Name, "@") {
			for i := 0; i < srv.Count; i++ {
				nodeName := strings.Replace(srv.Name, "@", fmt.Sprintf("@%d", i+1), -1)
				u := buildUnit(nodeName, uf, srv.DesiredState)
				wantedUnits = updateServiceGroupMap(u, serviceName, isSidekick, wantedUnits)

				if srv.SequentialDeployment {
					sg, _ := wantedUnits[serviceName]
					sg.isZDD = true
					wantedUnits[serviceName] = sg
				}
			}
		} else {
			log.Printf("WARNING skipping service: %s, incorrect service definition", srv.Name)
		}
	}
	return wantedUnits, nil
}

func (d *deployer) isNewUnit(u *schema.Unit) bool {
	currentUnit, err := d.fleetapi.Unit(u.Name)
	if err != nil {
		log.Printf("WARN Failed to determine if it's a new unit %s: %v", u.Name, err)
		return false
	}
	return currentUnit == nil
}

func (d *deployer) performSequentialDeploymentSK(sg serviceGroup) {
	for i, u := range sg.sidekicks {
		d.updateUnit(u)
		if err := d.fleetapi.SetUnitTargetState(u.Name, "launched"); err != nil {
			continue
		}
		if (i + 1) == len(sg.sidekicks) { //this is the last node, we're done
			break
		}
		d.waitForUnitToLaunch(u.Name)
	}
}

func (d *deployer) performSequentialDeployment(sg serviceGroup) {
	for i, u := range sg.serviceNodes {
		d.updateCorrespondingSK(u, sg.sidekicks)
		d.updateUnit(u)
		if err := d.fleetapi.SetUnitTargetState(u.Name, "launched"); err != nil {
			continue
		}
		if (i + 1) == len(sg.serviceNodes) { //this is the last node, we're done
			break
		}

		var unitToWaitOn string
		if len(sg.sidekicks) == 0 {
			unitToWaitOn = u.Name
		} else {
			unitToWaitOn = strings.Replace(u.Name, "@", "-sidekick@", 1)
		}

		d.waitForUnitToLaunch(unitToWaitOn)
	}
}

func (d *deployer) updateCorrespondingSK(service *schema.Unit, sidekicks []*schema.Unit) {
	if len(sidekicks) == 0 {
		return
	}
	skName := strings.Replace(service.Name, "@", "-sidekick@", 1)
	for _, sk := range sidekicks {
		if sk.Name == skName {
			d.updateUnit(sk)
			d.fleetapi.SetUnitTargetState(sk.Name, "launched")
			break
		}
	}
}

func (d *deployer) waitForUnitToLaunch(unitName string) {
	timeoutChan := make(chan bool)
	go func() {
		<-time.After(launchTimeout)
		close(timeoutChan)
	}()

	tickerChan := time.NewTicker(stateCheckInterval)
	for {
		select {
		case <-tickerChan.C:
			unitStatus, err := d.fleetapi.Unit(unitName)
			if err != nil {
				log.Printf("WARNING Failed to get unit %s: %v [SKIPPING]", unitName, err)
				continue
			}

			log.Printf("INFO UnitToWaitOn status: [%v]\n", unitStatus.CurrentState)
			if unitStatus.CurrentState == launchedState {
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

//only name and desireState of the units are used
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

func (d *deployer) launchAll(serviceGroups map[string]serviceGroup) error {
	currentUnits, err := d.buildCurrentUnits()
	if err != nil {
		log.Printf("Error building current Units: [%s]", err.Error())
		return err
	}
	for _, sg := range serviceGroups {
		for _, u := range sg.serviceNodes {
			d.launchUnit(u, currentUnits)
		}
		for _, u := range sg.sidekicks {
			d.launchUnit(u, currentUnits)
		}
	}
	return nil
}

func (d *deployer) isUpdatedUnit(newUnit *schema.Unit) bool {
	currentUnitHash, ok := d.unitCache[newUnit.Name]
	if !ok {
		log.Printf("ERROR Current unit not found in cache, marking as NOT updated: [%s]", newUnit.Name)
		return false
	}
	newUnitFile := schema.MapSchemaUnitOptionsToUnitFile(newUnit.Options)
	for _, option := range newUnitFile.Options {
		option.Value = whitespaceMatcher.ReplaceAllString(option.Value, " ")
	}
	return newUnitFile.Hash() != currentUnitHash
}

func (d *deployer) makeServiceFile(s service) (string, error) {
	vars := make(map[string]interface{})
	serviceTemplate, err := d.serviceDefinitionClient.serviceFile(s)
	if err != nil {
		log.Printf("ERROR  Cannot read service file for unit [%s]: %v \nAborting run!", s.Name, err)
		return "", err
	}
	vars["version"] = s.Version
	serviceFile, err := renderServiceFile(serviceTemplate, vars)
	if err != nil {
		log.Printf("ERROR Cannot render service file: %v", err)
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
	originalDesiredState := u.DesiredState
	u.DesiredState = inactiveState
	if err := d.fleetapi.DestroyUnit(u.Name); err != nil {
		return
	}
	if err := d.fleetapi.CreateUnit(u); err != nil {
		return
	}
	u.DesiredState = originalDesiredState
}

func (d *deployer) launchUnit(u *schema.Unit, currentUnits map[string]*schema.Unit) {
	if u.DesiredState == "" {
		u.DesiredState = launchedState
	}
	if currentUnit, ok := currentUnits[u.Name]; ok {
		if currentUnit.DesiredState != u.DesiredState {
			d.fleetapi.SetUnitTargetState(u.Name, u.DesiredState)
		}
	} else {
		d.fleetapi.SetUnitTargetState(u.Name, u.DesiredState)
	}
}

func updateServiceGroupMap(u *schema.Unit, serviceName string, isSidekick bool, serviceGroups map[string]serviceGroup) map[string]serviceGroup {
	if sg, ok := serviceGroups[serviceName]; ok {
		if isSidekick {
			sg.sidekicks = append(sg.sidekicks, u)
		} else {
			sg.serviceNodes = append(sg.serviceNodes, u)
		}
		serviceGroups[serviceName] = sg
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

func mergeMaps(maps ...map[string]serviceGroup) map[string]serviceGroup {
	merged := make(map[string]serviceGroup)
	for _, sgMap := range maps {
		for k, v := range sgMap {
			merged[k] = v
		}
	}
	return merged
}

func purgeProcessed(wanted map[string]serviceGroup, processed map[string]serviceGroup) {
	for key := range processed {
		delete(wanted, key)
	}
}

func renderServiceFile(serviceTemplate []byte, context map[string]interface{}) (string, error) {
	if context["version"] == "" {
		return string(serviceTemplate), nil
	}
	versionString := fmt.Sprintf("DOCKER_APP_VERSION=%s", context["version"])
	serviceTemplateString := strings.Replace(string(serviceTemplate), "DOCKER_APP_VERSION=latest", versionString, 1)
	return serviceTemplateString, nil
}

func getServiceName(unitName string) string {
	if strings.Contains(unitName, "sidekick") { //sidekick
		return strings.Split(unitName, "-sidekick")[0]
	}
	if strings.Contains(unitName, "@.service") { //templated without node number
		return strings.Split(unitName, "@.service")[0]
	}
	if strings.Contains(unitName, "@") { //templated with node number
		return strings.Split(unitName, "@")[0]
	}
	return strings.Split(unitName, ".service")[0] //not templated
}

func getUnitHash(unit *schema.Unit) unit.Hash {
	unitFile := schema.MapSchemaUnitOptionsToUnitFile(unit.Options)
	for _, option := range unitFile.Options {
		option.Value = whitespaceMatcher.ReplaceAllString(option.Value, " ")
	}
	return unitFile.Hash()
}
