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
	resp, err := hsdc.httpClient.Get(fmt.Sprintf("%vservices.yaml?%v", hsdc.rootURI, time.Now().Format(time.RFC3339)))
	if err != nil {
		return services{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return services{}, fmt.Errorf("Requesting services.yaml file returned %v HTTP status\n", resp.Status)
	}

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

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Requesting service file %v returned %v HTTP status\n", service.Name, resp.Status)
	}

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
		return fmt.Sprintf("%v?%v", service.URI, time.Now().Format(time.RFC3339)), nil
	}
	if rootURI == "" {
		return "", errors.New("WARNING Service uri isn't absolute and rootURI not specified")
	}
	uri := fmt.Sprintf("%s%s?%v", rootURI, service.Name, time.Now().Format(time.RFC3339))
	return uri, nil
}
