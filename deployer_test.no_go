package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"gopkg.in/yaml.v2"
)

var badServiceYaml = []byte(`---
services:
  - name: annotations-api@.service 
    version: latest
    count: 0
  - name: bad-syntax.service 
    uri: bad-syntax.service
    version: latest
  - name: annotations-api-sidekick@.service 
    version: latest`)

var goodServiceYaml = []byte(`---
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
    count: 1
  - name: mongo-backup.service
    uri: https://raw.githubusercontent.com/Financial-Times/fleet/pre-prod/service-files/mongo-backup.service
    desiredState: loaded
  - name: mongo-backup.timer
    uri: https://raw.githubusercontent.com/Financial-Times/fleet/pre-prod/service-files/mongo-backup.timer
    desiredState: inactive
`)

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

func (msdc *mockBadServiceDefinitionClient) serviceFile(service service) ([]byte, error) {
	if service.URI == "bad-syntax.service" {
		return badServiceFileString, nil
	}
	return goodServiceFileString, nil
}

type mockGoodServiceDefinitionClient struct{}

func (msdc *mockGoodServiceDefinitionClient) servicesDefinition() (services services, err error) {
	err = yaml.Unmarshal(goodServiceYaml, &services)
	return
}

func (msdc *mockGoodServiceDefinitionClient) serviceFile(service service) ([]byte, error) {
	return goodServiceFileString, nil
}

func TestBuildWantedUnitsBad(t *testing.T) {
	mockServiceDefinitionClient := &mockBadServiceDefinitionClient{}
	d := &deployer{serviceDefinitionClient: mockServiceDefinitionClient}
	wantedUnits, _, err := d.buildWantedUnits()
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
	wantedUnits, _, err := d.buildWantedUnits()
	if err != nil {
		t.Errorf("wanted units threw an error: %v", err)
	}
	if wantedUnits["mongodb-configurator.service"] == nil {
		t.Fatalf("Didn't load a service without a count")
	}
	if wantedUnits["mongodb-configurator.service"] == nil {
		t.Fatalf("Didn't load a service without a desiredState")
	}
	if len(wantedUnits) != 11 {
		t.Fatalf("Didn't load all services, loaded: %d", len(wantedUnits))
	}

	t.Logf("Passed with wanted units: %v", wantedUnits)
}

func TestBuildDesiredStateHandling(t *testing.T) {
	mockServiceDefinitionClient := &mockGoodServiceDefinitionClient{}
	d := &deployer{serviceDefinitionClient: mockServiceDefinitionClient}
	wantedUnits, _, _ := d.buildWantedUnits()
	//TODO would be nice to look at whats being passed to fleet here and assert on that
	//d.launchAll(wantedUnits)

	withoutState := wantedUnits["annotations-api-sidekick@1.service"]
	if withoutState.DesiredState != "" {
		t.Fatalf("Set value %s for desiredState", withoutState.DesiredState)
	}

	withHandledState := wantedUnits["mongo-backup.service"]
	if withHandledState.DesiredState != "loaded" {
		t.Fatalf("Didn't set DesiredState to provided value")
	}

	withUnhandledState := wantedUnits["mongo-backup.timer"]
	if withUnhandledState.DesiredState != "inactive" {
		t.Fatalf("Didn't set DesiredState to value provided")
	}
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
func TestServicesDefinition(t *testing.T) {

	// Test server that always responds with 200 code, and specific payload
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Header().Set("Content-Type", "text/plain")
		w.Write(goodServiceYaml)
	}))
	defer server.Close()

	// Make a transport that reroutes all traffic to the example server
	transport := &http.Transport{
		Proxy: func(req *http.Request) (*url.URL, error) {
			if req.URL.String() == "http://raw.githubusercontent.com/Financial-Times/fleet/master/service-files/services.yaml" {
				return url.Parse(server.URL)
			}
			return nil, errors.New("Unexpected url. Failing.")
		},
	}

	// Make a http.Client with the transport
	httpClient := &http.Client{Transport: transport}

	serviceDefinitionClient := &httpServiceDefinitionClient{httpClient: httpClient, rootURI: "http://raw.githubusercontent.com/Financial-Times/fleet/master/service-files/"}
	services, err := serviceDefinitionClient.servicesDefinition()
	if len(services.Services) != 7 || err != nil {
		t.Fatalf("Didn't retrieve services definition: %s", err.Error())
	}
}

func TestServiceFileForMissingUri(t *testing.T) {

	// Test server that always responds with 200 code, and specific payload
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Header().Set("Content-Type", "text/plain")
		w.Write(goodServiceFileString)
	}))
	defer server.Close()

	// Make a transport that reroutes all traffic to the example server
	transport := &http.Transport{
		Proxy: func(req *http.Request) (*url.URL, error) {
			if req.URL.String() == "http://raw.githubusercontent.com/Financial-Times/fleet/master/service-files/deployer.service" {
				return url.Parse(server.URL)
			}
			return nil, errors.New("Unexpected url. Failing.")
		},
	}

	// Make a http.Client with the transport
	httpClient := &http.Client{Transport: transport}

	serviceDefinitionClient := &httpServiceDefinitionClient{httpClient: httpClient, rootURI: "http://raw.githubusercontent.com/Financial-Times/fleet/master/service-files/"}
	_, err := serviceDefinitionClient.serviceFile(service{Name: "deployer.service"})
	if err != nil {
		t.Fatalf("Didn't retrieve service definition: %s", err.Error())
	}
}

func TestServiceFileForAbsoluteUri(t *testing.T) {

	// Test server that always responds with 200 code, and specific payload
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Header().Set("Content-Type", "text/plain")
		w.Write(goodServiceFileString)
	}))
	defer server.Close()

	// Make a transport that reroutes all traffic to the example server
	transport := &http.Transport{
		Proxy: func(req *http.Request) (*url.URL, error) {
			if req.URL.String() == "http://mydeployer.com/deployer.service" {
				return url.Parse(server.URL)
			}
			return nil, errors.New("Unexpected url. Failing.")
		},
	}

	// Make a http.Client with the transport
	httpClient := &http.Client{Transport: transport}

	serviceDefinitionClient := &httpServiceDefinitionClient{httpClient: httpClient, rootURI: "http://raw.githubusercontent.com/Financial-Times/fleet/master/service-files/"}
	_, err := serviceDefinitionClient.serviceFile(service{Name: "deployer.service", URI: "http://mydeployer.com/deployer.service"})
	if err != nil {
		t.Fatalf("Didn't retrieve service definition: %s", err.Error())
	}

}
