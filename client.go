package main

import (
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"strings"
	"time"

	"gopkg.in/yaml.v2"
)

type httpServiceDefinitionClient struct {
	httpClient *http.Client
	rootURI    string
}

func (hsdc *httpServiceDefinitionClient) servicesDefinition() (services, error) {
	resp, err := sendServicesDefinitionRequest(hsdc)

	if err != nil {
		return services{}, err
	}

	defer resp.Body.Close()
	serviceYaml, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return services{}, err
	}
	return renderServiceDefinitionYaml(serviceYaml)
}

func (hsdc *httpServiceDefinitionClient) serviceFile(service service) ([]byte, error) {
	serviceFileURI, err := buildServiceFileURI(service, hsdc.rootURI)
	if err != nil {
		return nil, err
	}
	resp, err := hsdc.httpClient.Get(serviceFileURI)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Requesting service file %v returned %v HTTP status. The request URL is: %s\n", service.Name, resp.Status, serviceFileURI)
	}

	serviceTemplate, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return serviceTemplate, nil
}

func (hsdc *httpServiceDefinitionClient) checkServiceFilesRepoHealth() error {
	_, err := sendServicesDefinitionRequest(hsdc)
	return err
}

func (hsdc *httpServiceDefinitionClient) getRootURI() string {
	return hsdc.rootURI
}

func renderServiceDefinitionYaml(serviceYaml []byte) (services services, err error) {
	if err = yaml.Unmarshal(serviceYaml, &services); err != nil {
		panic(err)
	}
	return
}

func buildServiceFileURI(service service, rootURI string) (string, error) {
	if strings.HasPrefix(service.URI, "http") == true {
		return fmt.Sprintf("%v?%v", service.URI, time.Now().Format(time.RFC3339)), nil
	}
	if rootURI == "" {
		return "", errors.New("WARNING Service uri isn't absolute and rootURI not specified")
	}
	uri := fmt.Sprintf("%s%s?%v", rootURI, service.Name, time.Now().Format(time.RFC3339))
	return uri, nil
}

func sendServicesDefinitionRequest(hsdc *httpServiceDefinitionClient) (*http.Response, error) {
	servicesDefinitionUri := fmt.Sprintf("%vservices.yaml?%v", hsdc.rootURI, time.Now().Format(time.RFC3339))
	resp, err := hsdc.httpClient.Get(servicesDefinitionUri)
	if err != nil {
		return &http.Response{}, err
	}

	if resp.StatusCode != http.StatusOK {
		return &http.Response{}, fmt.Errorf("Requesting services.yaml file returned %v HTTP status. The request URL is: %s\n", resp.Status, servicesDefinitionUri)
	}

	return resp, nil
}
