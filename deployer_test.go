package main

import (
	"gopkg.in/yaml.v2"
	"net/http"
	"testing"
)

func mockServiceDefinitionGetter(httpClient *http.Client) (services services) {
	serviceYaml := []byte(`---
services:
  - name: mongodb@.service
    version: latest
    count: 3
  - name: mongodb-sidekick@.service
    version: latest
    count: 3
  - name: mongodb-configurator.service
    version: latest
  - name: annotations-api@.service 
    version: latest
    count: 1
  - name: annotations-api-sidekick@.service 
    version: latest
    count: 1`)
	yaml.Unmarshal(serviceYaml, &services)
	return services

}

type sfr struct{}

func (sfr *sfr) renderServiceFile(name string, context ...interface{}) (string, error) {
	return `[Unit]
Description=Deployer

[Service]
TimeoutStartSec=600
ExecStartPre=-/usr/bin/docker kill %p-%i
ExecStartPre=-/usr/bin/docker rm %p-%i
ExecStartPre=/usr/bin/docker pull coco/coco-fleet-deployer
ExecStart=/bin/bash -c "docker run --rm --name %p-%i --env=\"FLEET_ENDPOINT=http://$HOSTNAME:49153\" --env=\"SERVICE_FILES_URI=https://raw.githubusercontent.com/Financial-Times/fleet/master/service-files/\" --env=\"SERVICES_DEFINITION_FILE_URI=https://raw.githubusercontent.com/Financial-Times/fleet/master/services.yaml\" --env=\"INTERVAL_IN_SECONDS_BETWEEN_DEPLOYS=60\" --env=\"DESTROY=false\" coco/coco-fleet-deployer"
ExecStop=/usr/bin/docker stop -t 3 %p-%i
`, nil
}

func TestBuildWantedUnits(t *testing.T) {
	sfr := &sfr{}
	d, err := newDeployer(mockServiceDefinitionGetter)
	wantedUnits, err := d.buildWantedUnits(sfr)
	if err != nil {
		t.Errorf("wanted units threw an error: %v", err)
	}
	t.Logf("Passed with wanted units: %v", wantedUnits)
}
