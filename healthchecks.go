package main

import (
	"fmt"
	fthealth "github.com/Financial-Times/go-fthealth/v1a"
	"log"
)

func (d *deployer) servicesDefinitionClientHealthCheck() fthealth.Check {
	return fthealth.Check{
		BusinessImpact:   d.healthBusinessImpact,
		Name:             "Check connectivity to services definition location: %s",
		PanicGuide:       "TO DO",
		Severity:         2,
		TechnicalSummary: fmt.Sprintf("Checks that services definition location is reachable. Services definition URI: %s", d.serviceDefinitionClient.getRootURI()),
		Checker: func() (string, error) {
			return "", d.serviceDefinitionClient.checkServiceFilesRepoHealth()
		},
	}
}
