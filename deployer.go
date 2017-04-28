package main

import (
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"regexp"

	etcdClient "github.com/coreos/etcd/client"
	"github.com/coreos/fleet/client"
	"github.com/coreos/fleet/schema"
	"github.com/coreos/fleet/unit"
	"github.com/kr/pretty"
	"golang.org/x/net/context"
	"golang.org/x/net/proxy"
)

type deployer struct {
	fleetapi                client.API
	serviceDefinitionClient serviceDefinitionClient
	unitCache               map[string]unit.Hash
	nodeCountCache          map[string]int
	isDebug                 bool
	etcdapi                 etcdClient.KeysAPI
	httpClient              *http.Client
	healthURLPrefix         string
	healthEndpoint          string
	serviceNamePrefix       string
}

const launchedState = "launched"
const inactiveState = "inactive"
const launchTimeout = time.Duration(5) * time.Minute
const stateCheckInterval = time.Duration(60) * time.Second

var whitespaceMatcher, _ = regexp.Compile("\\s+")

func newDeployer() (*deployer, error) {
	u, err := url.Parse(*fleetEndpoint)
	if err != nil {
		return &deployer{}, err
	}
	httpClient := &http.Client{
		Timeout:   60 * time.Second,
		Transport: &http.Transport{MaxIdleConnsPerHost: 100},
	}

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
			MaxIdleConnsPerHost: 100,
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
	serviceDefinitionClient := &httpServiceDefinitionClient{
		httpClient: httpClient,
		rootURI:    *rootURI,
	}

	etcdClientCfg := etcdClient.Config{
		Endpoints:               []string{*etcdURL},
		Transport:               etcdClient.DefaultTransport,
		HeaderTimeoutPerRequest: time.Second * 5,
	}

	c, err := etcdClient.New(etcdClientCfg)
	if err != nil {
		return &deployer{}, err
	}
	etcdapi := etcdClient.NewKeysAPI(c)
	return &deployer{
		fleetapi:                fleetHTTPAPIClient,
		serviceDefinitionClient: serviceDefinitionClient,
		isDebug:                 *isDebug,
		etcdapi:                 etcdapi,
		httpClient:              httpClient,
		healthURLPrefix:         *healthURLPrefix,
		healthEndpoint:          *healthEndpoint,
		serviceNamePrefix:       *serviceNamePrefix,
	}, nil
}

func (d *deployer) deployAll() error {
	if d.isDebug {
		log.Println("Debug log enabled.")
	}
	if d.unitCache == nil {
		uc, ncc, err := d.buildCaches()
		if err != nil {
			log.Printf("ERROR Cannot build unit cache, aborting run: [%v]", err)
			return err
		}
		d.unitCache = uc
		if d.isDebug {
			log.Printf("Count cache:\n %# v \n", pretty.Formatter(ncc))
		}
		d.nodeCountCache = ncc
	}

	wantedServiceGroups, err := d.buildWantedUnits()
	if err != nil {
		return err
	}

	toDelete := d.identifyDeletedServiceGroups(wantedServiceGroups)
	toCreate := d.identifyNewServiceGroups(wantedServiceGroups)
	sgsWithNodesAdded := d.identifySGsWithNodesAdded(wantedServiceGroups)
	sgsWithNodesRemoved := d.identifySGsWithNodesRemoved(wantedServiceGroups)
	purgeProcessed(wantedServiceGroups, toCreate)
	toUpdateRegular, toUpdateSKRegular, toUpdateSequentially, toUpdateSKSequentially := d.identifyUpdatedServiceGroups(wantedServiceGroups)

	d.createServiceGroups(toCreate)
	d.createNodes(sgsWithNodesAdded)
	d.deleteNodes(sgsWithNodesRemoved)
	d.updateServiceGroupsNormally(toUpdateRegular)
	d.updateServiceGroupsSequentially(toUpdateSequentially)
	d.updateServiceGroupsSKsOnly(toUpdateSKRegular)
	d.updateServiceGroupsSKsSequentially(toUpdateSKSequentially)
	d.deleteServiceGroups(toDelete)

	if changesDone(toCreate, sgsWithNodesAdded, sgsWithNodesRemoved, toUpdateRegular, toUpdateSequentially, toUpdateSKRegular, toUpdateSKSequentially, toDelete) {
		if d.isDebug {
			log.Printf("Invalidating cache.")
		}
		d.unitCache = nil
	}

	return nil
}

func (d *deployer) updateCache(serviceGroups map[string]serviceGroup) {
	for _, sg := range serviceGroups {
		for _, u := range sg.getUnits() {
			d.unitCache[u.Name] = getUnitHash(u)
		}
	}
}

func (d *deployer) removeFromCache(serviceGroups map[string]serviceGroup) {
	for _, sg := range serviceGroups {
		for _, u := range sg.getUnits() {
			delete(d.unitCache, u.Name)
		}
	}
}

func (d *deployer) buildCaches() (map[string]unit.Hash, map[string]int, error) {
	units, err := d.fleetapi.Units()
	if err != nil {
		return nil, nil, err
	}

	unitCache := make(map[string]unit.Hash)
	countCache := make(map[string]int)
	for _, unit := range units {
		unitCache[unit.Name] = getUnitHash(unit)
		if !strings.Contains(unit.Name, "sidekick") {
			serviceName := getServiceName(unit.Name)
			currentCount := countCache[serviceName]
			countCache[serviceName] = currentCount + 1
		}
	}
	return unitCache, countCache, nil
}

func (d *deployer) identifyNewServiceGroups(serviceGroups map[string]serviceGroup) map[string]serviceGroup {
	newServiceGroups := make(map[string]serviceGroup)
	for name, sg := range serviceGroups {
		var unitToCheck *schema.Unit
		if len(sg.serviceNodes) > 0 {
			unitToCheck = sg.serviceNodes[0]

		} else if len(sg.sidekicks) > 0 {
			unitToCheck = sg.sidekicks[0]
		} else {
			log.Printf("ERROR invalid service group: [%v]", sg)
			continue
		}
		if d.isNewUnit(unitToCheck) {
			newServiceGroups[name] = sg
		}
	}
	return newServiceGroups
}

func (d *deployer) identifySGsWithNodesAdded(serviceGroups map[string]serviceGroup) map[string]serviceGroup {
	newNodes := make(map[string]serviceGroup)
	for name, sg := range serviceGroups {
		if len(sg.serviceNodes) > d.nodeCountCache[name] {
			if d.isDebug {
				log.Printf("Servicegroup %s had nodes added in servicefile.", name)
			}
			newNodes[name] = sg
		}
	}
	return newNodes
}

func (d *deployer) identifySGsWithNodesRemoved(serviceGroups map[string]serviceGroup) map[string]serviceGroup {
	extraNodes := make(map[string]serviceGroup)
	for name, sg := range serviceGroups {
		if len(sg.serviceNodes) < d.nodeCountCache[name] {
			if d.isDebug {
				log.Printf("Servicegroup %s had nodes removed in servicefile.", name)
			}
			extraNodes[name] = sg
		}
	}
	return extraNodes
}

func (d *deployer) identifyUpdatedServiceGroups(serviceGroups map[string]serviceGroup) (map[string]serviceGroup, map[string]serviceGroup, map[string]serviceGroup, map[string]serviceGroup) {
	updatedRegular := make(map[string]serviceGroup)
	skUpdatedRegular := make(map[string]serviceGroup)
	updatedSequential := make(map[string]serviceGroup)
	skUpdatedSequential := make(map[string]serviceGroup)

	for name, sg := range serviceGroups {
		if len(sg.serviceNodes) > 0 {
			if d.isUpdatedUnit(sg.serviceNodes[0]) {
				if d.isDebug {
					log.Printf("Unit %s detected as updated!", sg.serviceNodes[0].Name)
					log.Printf("Wanted unit options: \n\n %# v \n\n", pretty.Formatter(sg.serviceNodes[0].Options))
				}
				if sg.isZDD {
					updatedSequential[name] = sg
				} else {
					updatedRegular[name] = sg
				}
				continue
			}
		}
		if len(sg.sidekicks) > 0 {
			if d.isUpdatedUnit(sg.sidekicks[0]) {
				if d.isDebug {
					log.Printf("Unit %s detected as updated!", sg.sidekicks[0].Name)
					log.Printf("Wanted unit options: \n\n %# v \n\n", pretty.Formatter(sg.sidekicks[0].Options))
				}
				if sg.isZDD {
					skUpdatedSequential[name] = sg
				} else {
					skUpdatedRegular[name] = sg
				}
				continue
			}
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
		for _, u := range sg.getUnits() {
			d.fleetapi.CreateUnit(u)
		}
	}
	d.launch(serviceGroups)
}

func (d *deployer) createNodes(serviceGroups map[string]serviceGroup) {
	for _, sg := range serviceGroups {
		for _, u := range sg.getUnits() {
			if _, ok := d.unitCache[u.Name]; !ok {
				if d.isDebug {
					log.Printf("Unit %s is a new node for an existing service group.", u.Name)
				}
				d.fleetapi.CreateUnit(u)
				d.launchNewUnit(u)
			}
		}
	}
}

func (d *deployer) deleteNodes(serviceGroups map[string]serviceGroup) {
	for _, sg := range serviceGroups {
		currentNodes := []string{}
		serviceName := getServiceName(sg.serviceNodes[0].Name)

		// build the list of current nodes of this SG
		for k := range d.unitCache {
			existingServiceName := getServiceName(k)
			if serviceName == existingServiceName {
				currentNodes = append(currentNodes, k)
			}
		}
		if d.isDebug {
			log.Printf("Current nodes for sg %s: %# v", serviceName, pretty.Formatter(currentNodes))
		}
		//for each node of the current servicegroup, see if it's in the wanted list
		for _, currentNode := range currentNodes {
			isNeeded := false
			for _, u := range sg.getUnits() {
				if currentNode == u.Name {
					isNeeded = true
					break
				}
			}
			if isNeeded {
				continue
			}
			d.fleetapi.DestroyUnit(currentNode)
		}
	}
}

func (d *deployer) updateServiceGroupsSKsOnly(serviceGroups map[string]serviceGroup) {
	for _, sg := range serviceGroups {
		for _, u := range sg.sidekicks {
			d.updateUnit(u)
		}
	}
	d.launch(serviceGroups)
}

func (d *deployer) updateServiceGroupsNormally(serviceGroups map[string]serviceGroup) {
	for _, sg := range serviceGroups {
		for _, u := range sg.getUnits() {
			d.updateUnit(u)
		}
	}
	d.launch(serviceGroups)
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
		for _, u := range sg.getUnits() {
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
			if len(u.Options) == 0 {
				return nil, fmt.Errorf("ERROR Invalid unit: [%v]", u)
			}
			wantedUnits = updateServiceGroupMap(u, serviceName, isSidekick, wantedUnits)
		} else if srv.Count > 0 && strings.Contains(srv.Name, "@") {
			for i := 0; i < srv.Count; i++ {
				nodeName := strings.Replace(srv.Name, "@", fmt.Sprintf("@%d", i+1), -1)
				u := buildUnit(nodeName, uf, srv.DesiredState)
				if len(u.Options) == 0 {
					return nil, fmt.Errorf("ERROR Invalid unit: [%v]", u)
				}
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
	if len(wantedUnits) == 0 {
		return nil, fmt.Errorf("ERROR Wanted units list is empty, aborting so we don't delete all services")
	}
	for _, sg := range wantedUnits {
		if len(sg.serviceNodes) == 0 && len(sg.sidekicks) == 0 {
			return nil, fmt.Errorf("ERROR Service group empty, aborting")
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

		d.waitForUnitToLaunch(u.Name, d.checkUnitState)
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

		if d.hasHealthcheck(u.Name) {
			if d.isDebug {
				log.Printf("DEBUG Service %s has a healthcheck endpoint - will be used for sequential deployment check.", u.Name)
			}
			d.waitForUnitToLaunch(u.Name, d.checkUnitHealth)
		} else {
			if d.isDebug {
				log.Printf("DEBUG Service %s doesn't have a healthcheck endpoint - unit status will be used for sequential deployment check.", u.Name)
			}
			var unitToWaitOn string
			if len(sg.sidekicks) == 0 {
				unitToWaitOn = u.Name
			} else {
				unitToWaitOn = strings.Replace(u.Name, "@", "-sidekick@", 1)
			}
			d.waitForUnitToLaunch(unitToWaitOn, d.checkUnitState)
		}
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

func (d *deployer) waitForUnitToLaunch(unitName string, isUnitLaunched func(string) bool) {
	timeoutChan := make(chan bool)
	go func() {
		<-time.After(launchTimeout)
		close(timeoutChan)
	}()

	tickerChan := time.NewTicker(stateCheckInterval)
	for {
		select {
		case <-tickerChan.C:
			if launched := isUnitLaunched(unitName); launched {
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

func (d *deployer) checkUnitState(unitName string) bool {
	unitStatus, err := d.fleetapi.Unit(unitName)
	if err != nil {
		log.Printf("WARNING Failed to get unit %s: %v [SKIPPING]", unitName, err)
		return false
	}
	log.Printf("INFO UnitToWaitOn status: [%v]\n", unitStatus.CurrentState)
	return unitStatus.CurrentState == launchedState
}

func (d *deployer) checkUnitHealth(unitName string) bool {
	url := fmt.Sprintf("%s/%s%s/%s", d.healthURLPrefix, d.serviceNamePrefix, getServiceName(unitName), d.healthEndpoint)
	resp, err := d.httpClient.Get(url)
	if err != nil {
		log.Printf("ERROR Error calling %s: %v", url, err.Error())
		return false
	}

	defer func() {
		io.Copy(ioutil.Discard, resp.Body)
		resp.Body.Close()
	}()

	if d.isDebug {
		log.Printf("DEBUG Called %s to get healthcheck for %s.", url, unitName)
	}

	if resp.StatusCode != http.StatusOK {
		return false
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Printf("ERROR Error reading healthcheck response: " + err.Error())
		return false
	}

	health := &healthcheckResponse{}
	if err := json.Unmarshal(body, &health); err != nil {
		log.Printf("ERROR Error parsing healthcheck response: " + err.Error())
		return false
	}

	if d.isDebug {
		fmt.Printf("DEBUG healthcheck response: \n %# v \n", pretty.Formatter(health))
	}

	for _, check := range health.Checks {
		if check.OK != true {
			return false
		}
	}

	return true
}

func (d *deployer) hasHealthcheck(unitName string) bool {
	etcdValuePath := fmt.Sprintf("/ft/services/%s/healthcheck", getServiceName(unitName))
	etcdResp, err := d.etcdapi.Get(context.Background(), etcdValuePath, nil)
	if err != nil {
		log.Printf("WARNING Error while getting etcd key for app from %s: %v", etcdValuePath, err.Error())
		return false
	}
	if etcdResp.Node == nil {
		log.Printf("WARNING Error while getting etcd key for app from %s: node is nil", etcdValuePath)
		return false
	}
	if d.isDebug {
		log.Printf("DEBUG Healthcheck setting value at %s is %s", etcdValuePath, etcdResp.Node.Value)
	}
	return etcdResp.Node.Value == "true"
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

func (d *deployer) launch(serviceGroups map[string]serviceGroup) error {
	currentUnits, err := d.buildCurrentUnits()
	if err != nil {
		log.Printf("Error building current Units: [%s]", err.Error())
		return err
	}
	for _, sg := range serviceGroups {
		for _, u := range sg.getUnits() {
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
	return getUnitHash(newUnit) != currentUnitHash
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

func (d *deployer) launchNewUnit(u *schema.Unit) {
	if d.isDebug {
		log.Printf("Launching unit %s", u.Name)
	}
	if u.DesiredState == "" {
		u.DesiredState = launchedState
	}
	d.fleetapi.SetUnitTargetState(u.Name, u.DesiredState)
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

func changesDone(maps ...map[string]serviceGroup) bool {
	for _, sgMap := range maps {
		if len(sgMap) > 0 {
			return true
		}
	}
	return false
}

func (sg *serviceGroup) getUnits() []*schema.Unit {
	units := []*schema.Unit{}
	for _, u := range sg.serviceNodes {
		units = append(units, u)
	}
	for _, u := range sg.sidekicks {
		units = append(units, u)
	}
	return units
}
