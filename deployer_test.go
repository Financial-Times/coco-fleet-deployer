package main

import (
	"gopkg.in/yaml.v2"
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
  - name: annotations-api-sidekick@.service 
    version: latest`)

var goodServiceYaml = []byte(`---
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
    count: 1
  - name: mongodb-configurator.service
    version: latest`)

var goodServiceFileString = `[Unit]
Description=Deployer

[Service]
TimeoutStartSec=600
ExecStartPre=-/usr/bin/docker kill %p-%i
ExecStartPre=-/usr/bin/docker rm %p-%i
ExecStartPre=/usr/bin/docker pull coco/coco-fleet-deployer
ExecStart=/bin/bash -c "docker run --rm --name %p-%i --env=\"FLEET_ENDPOINT=http://$HOSTNAME:49153\" --env=\"SERVICE_FILES_URI=https://raw.githubusercontent.com/Financial-Times/fleet/master/service-files/\" --env=\"SERVICES_DEFINITION_FILE_URI=https://raw.githubusercontent.com/Financial-Times/fleet/master/services.yaml\" --env=\"INTERVAL_IN_SECONDS_BETWEEN_DEPLOYS=60\" --env=\"DESTROY=false\" coco/coco-fleet-deployer"
ExecStop=/usr/bin/docker stop -t 3 %p-%i`

type mockBadServiceDefinitionClient struct{}

func (msdc *mockBadServiceDefinitionClient) servicesDefinition() (services services) {
	yaml.Unmarshal(badServiceYaml, &services)
	return services
}

func (msdc *mockBadServiceDefinitionClient) renderedServiceFile(name string, context ...interface{}) (string, error) {
	return goodServiceFileString, nil
}

type mockGoodServiceDefinitionClient struct{}

func (msdc *mockGoodServiceDefinitionClient) servicesDefinition() (services services) {
	yaml.Unmarshal(goodServiceYaml, &services)
	return services
}

func (msdc *mockGoodServiceDefinitionClient) renderedServiceFile(name string, context ...interface{}) (string, error) {
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

	t.Logf("Passed with wanted units: %v", wantedUnits)
}
