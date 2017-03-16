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
	servicesDefinitionURI := fmt.Sprintf("%vservices.yaml?%v", hsdc.rootURI, time.Now().Format(time.RFC3339))
	resp, err := hsdc.httpClient.Get(servicesDefinitionURI)
	if err != nil {
		return services{}, fmt.Errorf("Failed to send request for services definition. The request URL is: %s  Error was: %s", servicesDefinitionURI, err.Error())
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return services{}, fmt.Errorf("Requesting services.yaml file returned %v HTTP status. The request URL is: %s \n", resp.Status, servicesDefinitionURI)
	}

	serviceYaml, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return services{}, fmt.Errorf("Failed to read services definition from response body. The request URL is: %s. Error was: %s", servicesDefinitionURI, err.Error())
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
		return nil, fmt.Errorf("Failed to send request for service file [service name is: %s]. The request URL is: %s  Error was: %s", service.Name, serviceFileURI, err.Error())
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Requesting service file %v returned %v HTTP status. The request URL is: %s \n", service.Name, resp.Status, serviceFileURI)
	}

	serviceTemplate, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("Failed to read service file [service name: %s] from response body. The request URL is: %s. Error was: %s", service.Name, serviceFileURI, err.Error())
	}
	return serviceTemplate, nil
}

func renderServiceDefinitionYaml(serviceYaml []byte) (s services, err error) {
	if err = yaml.Unmarshal(serviceYaml, &s); err != nil {
		return services{}, fmt.Errorf("Error while unmarshalling services definitiona yaml file. Error was: %s", err.Error())
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
