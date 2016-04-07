package main

import (
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"strings"

	"gopkg.in/yaml.v2"
)

type httpServiceDefinitionClient struct {
	httpClient *http.Client
	rootURI    string
}

func (hsdc *httpServiceDefinitionClient) servicesDefinition() (services, error) {
	resp, err := hsdc.httpClient.Get(hsdc.rootURI + "services.yaml")
	if err != nil {
		return services{}, err
	}
	defer resp.Body.Close()

	serviceYaml, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		panic(err)
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

	serviceTemplate, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return serviceTemplate, nil
}

func renderServiceDefinitionYaml(serviceYaml []byte) (services services, err error) {
	if err = yaml.Unmarshal(serviceYaml, &services); err != nil {
		panic(err)
	}
	return
}

func buildServiceFileURI(service service, rootURI string) (string, error) {
	if strings.HasPrefix(service.URI, "http") == true {
		return service.URI, nil
	}
	if rootURI == "" {
		return "", errors.New("WARNING Service uri isn't absolute and rootURI not specified")
	}
	uri := fmt.Sprintf("%s%s", rootURI, service.Name)
	return uri, nil
}
