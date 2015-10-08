package main

import (
	"gopkg.in/yaml.v2"
	"strings"
	"testing"
)

var badServiceYaml = []byte(`---
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
    count: 0
  - name: bad-syntax.service 
    uri: bad-syntax.service
    version: latest
  - name: annotations-api-sidekick@.service 
    version: latest`)

var goodServiceYaml = []byte(`---
rootUri: https://raw.githubusercontent.com/Financial-Times/fleet/master/service-files
services:
  - name: mongodb@.service
    uri: https://raw.githubusercontent.com/Financial-Times/fleet/master/service-files/mongodb@.service
    version: latest
    count: 3
  - name: mongodb-sidekick@.service
    uri: https://raw.githubusercontent.com/Financial-Times/fleet/master/service-files/mongodb-sidekick@.service
    version: latest
    count: 3
  - name: mongodb-configurator.service
    uri: https://raw.githubusercontent.com/Financial-Times/fleet/master/service-files/mongodb-configurator.service
  - name: annotations-api@.service 
    uri: https://raw.githubusercontent.com/Financial-Times/fleet/master/service-files/annotations-api@.service
    version: latest
    count: 1
  - name: annotations-api-sidekick@.service 
    uri: annotations-api-sidekick@.service
    version: latest
    count: 1`)

var goodServiceFileString = []byte(`[Unit]
Description=Deployer

[Service]
Environment="DOCKER_APP_VERSION=latest"
TimeoutStartSec=600
ExecStartPre=-/usr/bin/docker kill %p-%i
ExecStartPre=-/usr/bin/docker rm %p-%i
ExecStartPre=/usr/bin/docker pull coco/coco-fleet-deployer:$DOCKER_APP_VESRION
ExecStart=/bin/bash -c "docker run --rm --name %p-%i --env=\"FLEET_ENDPOINT=http://$HOSTNAME:49153\" --env=\"SERVICE_FILES_URI=https://raw.githubusercontent.com/Financial-Times/fleet/master/service-files/\" --env=\"SERVICES_DEFINITION_FILE_URI=https://raw.githubusercontent.com/Financial-Times/fleet/master/services.yaml\" --env=\"INTERVAL_IN_SECONDS_BETWEEN_DEPLOYS=60\" --env=\"DESTROY=false\" coco/coco-fleet-deployer:$DOCKER_APP_VESRION"
ExecStop=/usr/bin/docker stop -t 3 %p-%i`)

var badServiceFileString = []byte(`[Unit]
Description=Deployer

[Service]
Environment="DOCKER_APP_VERSION=latest"
TimeoutStartSec=600
ExecStartPre=-/usr/bin/docker kill %p-%i
ExecStartPre=-/usr/bin/docker rm %p-%i
<<<<<<<<<
ExecStartPre=/usr/bin/docker pull coco/coco-fleet-deployer:$DOCKER_APP_VESRION
ExecStart=/bin/bash -c "docker run --rm --name %p-%i --env=\"FLEET_ENDPOINT=http://$HOSTNAME:49153\" --env=\"SERVICE_FILES_URI=https://raw.githubusercontent.com/Financial-Times/fleet/master/service-files/\" --env=\"SERVICES_DEFINITION_FILE_URI=https://raw.githubusercontent.com/Financial-Times/fleet/master/services.yaml\" --env=\"INTERVAL_IN_SECONDS_BETWEEN_DEPLOYS=60\" --env=\"DESTROY=false\" coco/coco-fleet-deployer:$DOCKER_APP_VESRION"
ExecStop=/usr/bin/docker stop -t 3 %p-%i`)

type mockBadServiceDefinitionClient struct{}

func (msdc *mockBadServiceDefinitionClient) servicesDefinition() (services services, err error) {
	err = yaml.Unmarshal(badServiceYaml, &services)
	return
}

func (msdc *mockBadServiceDefinitionClient) serviceFile(serviceFileURI string) ([]byte, error) {
	if serviceFileURI == "bad-syntax.service" {
		return badServiceFileString, nil
	}
	return goodServiceFileString, nil
}

type mockGoodServiceDefinitionClient struct{}

func (msdc *mockGoodServiceDefinitionClient) servicesDefinition() (services services, err error) {
	err = yaml.Unmarshal(goodServiceYaml, &services)
	return
}

func (msdc *mockGoodServiceDefinitionClient) serviceFile(serviceFileURI string) ([]byte, error) {
	return goodServiceFileString, nil
}

func TestBuildWantedUnitsBad(t *testing.T) {
	mockServiceDefinitionClient := &mockBadServiceDefinitionClient{}
	d := &deployer{serviceDefinitionClient: mockServiceDefinitionClient}
	wantedUnits, err := d.buildWantedUnits()
	if err != nil {
		t.Errorf("wanted units threw an error: %v", err)
	}
	if wantedUnits["annotations-api@.service"] != nil {
		t.Fatalf("Scheduled a '@' unit with 0 count")
	}
	if wantedUnits["annotations-api-sidekick@.service"] != nil {
		t.Fatalf("Scheduled a '@' unit without a count")
	}
	if len(wantedUnits) != 0 {
		t.Fatalf("No services should've been loaded, loaded: %d, %v", len(wantedUnits), wantedUnits)
	}

	t.Logf("Passed with wanted units: %v", wantedUnits)
}

func TestBuildWantedUnitsGood(t *testing.T) {
	mockServiceDefinitionClient := &mockGoodServiceDefinitionClient{}
	d := &deployer{serviceDefinitionClient: mockServiceDefinitionClient}
	wantedUnits, err := d.buildWantedUnits()
	if err != nil {
		t.Errorf("wanted units threw an error: %v", err)
	}
	if wantedUnits["mongodb-configurator.service"] == nil {
		t.Fatalf("Didn't load a service without a count")
	}
	if len(wantedUnits) != 9 {
		t.Fatalf("Didn't load all services, loaded: %d", len(wantedUnits))
	}

	t.Logf("Passed with wanted units: %v", wantedUnits)
}

func TestRenderServiceFile(t *testing.T) {
	vars := make(map[string]interface{})
	version := "v1_asdasdasd"
	vars["version"] = version
	serviceFile, err := renderedServiceFile(goodServiceFileString, vars)
	if err != nil {
		t.Errorf("failed rendering with error %v", err)
	}
	if !strings.Contains(serviceFile, vars["version"].(string)) {
		t.Errorf("Service file didn't render properly\n%s", serviceFile)
	}
}
