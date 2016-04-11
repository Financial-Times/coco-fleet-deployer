package main

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coreos/fleet/client"
	"github.com/coreos/fleet/schema"
	"github.com/coreos/fleet/unit"
	"github.com/kr/pretty"
	"golang.org/x/net/proxy"
)

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
	// Get service definition - wanted units
	//get whitelist
	wantedUnits, zddUnits, err := d.buildWantedUnits()
	log.Printf("DEBUG wantedUnits: %# v\n", pretty.Formatter(wantedUnits))
	log.Printf("DEBUG zddUnits: %# v\n", pretty.Formatter(wantedUnits))

	deployedUnits := make(map[string]bool)

	if err != nil {
		return err
	}

	// create any missing units
	for _, u := range wantedUnits {
		//basically reproduce the behavior of the deployUnit() function but with the
		//sequential deployment thrown in

		isNew, err := d.isNewUnit(u)
		if err != nil {
			log.Printf("WARNING Failed to determine if it's a new unit %s: %v [SKIPPING]", u.Name, err)
			continue
		}

		if isNew {
			log.Printf("DEBUG: Unit [%v] is new", u.Name)
			err := d.fleetapi.CreateUnit(u)
			if err != nil {
				log.Printf("WARNING Failed to create unit %s: %v [SKIPPING]", u.Name, err)
				continue
			}
		}

		isUpdated, err := d.isUpdatedUnit(u)
		if err != nil {
			log.Printf("WARNING Failed to determine if updated unit %s: %v [SKIPPING]", u.Name, err)
			continue
		}

		if !isUpdated {
			continue
		}

		//for updated apps which need zero downtime deployment, we do that here
		log.Printf("DEBUG Unit [%v] is  updated", u.Name)
		if _, ok := zddUnits[u.Name]; ok {
			log.Printf("DEBUG Unit [%v] is ZDD ", u.Name)

			if _, ok := deployedUnits[u.Name]; ok {
				log.Printf("DEBUG Unit [%v] was already deployed", u.Name)
				continue
			}

			deployed, err := d.performSequentialDeployment(u, zddUnits, wantedUnits)
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

func (d *deployer) buildWantedUnits() (map[string]*schema.Unit, map[string]zddInfo, error) {
	units := make(map[string]*schema.Unit)
	zddUnits := make(map[string]zddInfo)

	servicesDefinition, err := d.serviceDefinitionClient.servicesDefinition()
	log.Printf("DEBUG Services Definitions: [%# v]\n", pretty.Formatter(servicesDefinition))
	if err != nil {
		return nil, nil, err
	}

	for _, srv := range servicesDefinition.Services {
		vars := make(map[string]interface{})
		serviceTemplate, err := d.serviceDefinitionClient.serviceFile(srv)
		/*
		service template on output:
		interprets newlines
		\ at line endings are left alone
		\\\"url\\\" - left as it is in the service file
		*/
		 
		log.Printf("DEBUG Service Template: [%s]\n", string(serviceTemplate))

		if err != nil {
			log.Printf("%v", err)
			continue
		}
		vars["version"] = srv.Version
		serviceFile, err := renderedServiceFile(serviceTemplate, vars)
		/*
		Service file on output: 
		same as service template
		 */
		log.Printf("DEBUG Service File : [%s]\n", serviceFile)

		if err != nil {
			log.Printf("%v", err)
			return nil, nil, err
		}

		// fleet deploy
		uf, err := unit.NewUnitFile(serviceFile)
		/*
		New Unit file on output:
		does not interpret newline
		\ at line endings left alone
		newline is represented as: \\n 
		\\\\\\\"url\\\\\\\" - escapes every character - 3x\ and 1x"
		 */

		if err != nil {
			//Broken service file, skip it and continue
			log.Printf("WARNING service file %s is incorrect: %v [SKIPPING]", srv.Name, err)
			continue
		}
		
		log.Printf("DEBUG New Unit File: \n[%# v]\n", pretty.Formatter(uf))
		for _, option := range uf.Options{
			option.Value =  strings.Replace(option.Value,"\\\n"," ",-1)
		}
		log.Printf("DEBUG New Unit File after newline removal: \n [%# v]\n", pretty.Formatter(uf))

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

func (d *deployer) isNewUnit(u *schema.Unit) (bool, error) {
	currentUnit, err := d.fleetapi.Unit(u.Name)
	if err != nil {
		return false, err
	}
	return currentUnit == nil, nil
}

func (d *deployer) performSequentialDeployment(u *schema.Unit, zddUnits map[string]zddInfo, wantedUnits map[string]*schema.Unit) (map[string]bool, error) {
	deployedUnits := make(map[string]bool)
	serviceName := strings.Split(u.Name, "@")[0]
	nrOfNodes := zddUnits[u.Name].count

	if u.DesiredState == "" {
		u.DesiredState = "launched"
	}

	for i := 1; i <= nrOfNodes; i++ {
		// generate unit name with number - the one we have originally might not be
		// the 1st node
		unitName := fmt.Sprintf("%v@%d.%v", serviceName, i, strings.Split(u.Name, ".")[1])

		err := d.fleetapi.DestroyUnit(unitName)
		if err != nil {
			log.Printf("WARNING Failed to destroy unit %s: %v [SKIPPING]", u.Name, err)
			continue
		}

		err = d.fleetapi.CreateUnit(wantedUnits[unitName])
		if err != nil {
			log.Printf("WARNING Failed to create unit %s: %v [SKIPPING]", u.Name, err)
			continue
		}
		// start the service
		err = d.fleetapi.SetUnitTargetState(unitName, u.DesiredState)
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

func (d *deployer) launchAll(wantedUnits, currentUnits map[string]*schema.Unit, zddUnits map[string]zddInfo) error {
	for _, u := range wantedUnits {
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
	return nil
}

func (d *deployer) isUpdatedUnit(newUnit *schema.Unit) (bool, error) {
	log.Printf("DEBUG Determining if unit [%s] is a an updated unit \n", newUnit.Name)
	currentUnit, err := d.fleetapi.Unit(newUnit.Name)
	if err != nil {
		return false, err
	}

	log.Printf("DEBUG CurrentUnit for name [%s]: %# v \n", newUnit.Name, pretty.Formatter(currentUnit))
	log.Printf("DEBUG NewUnit     for name [%s]: %# v \n", newUnit.Name, pretty.Formatter(newUnit))
	nuf := schema.MapSchemaUnitOptionsToUnitFile(newUnit.Options)
	cuf := schema.MapSchemaUnitOptionsToUnitFile(currentUnit.Options)

	log.Printf("DEBUG New Unitfile: %# v \n", pretty.Formatter(nuf))
	log.Printf("DEBUG Current Unitfile: %# v \n", pretty.Formatter(cuf))

	log.Printf("DEBUG New Unitfile hash: [%v] \n", nuf.Hash())
	log.Printf("DEBUG Current Unitfile hash: [%v] \n", cuf.Hash())
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
