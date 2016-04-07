package main

import (
	"log"

	"github.com/coreos/fleet/client"
	"github.com/coreos/fleet/schema"
)

type loggingFleetAPI struct {
	client.API
}

type noDestroyFleetAPI struct {
	client.API
}

func (lapi loggingFleetAPI) CreateUnit(unit *schema.Unit) error {
	log.Printf("INFO Creating or updating unit %s\n", unit.Name)
	return lapi.API.CreateUnit(unit)
}

func (lapi loggingFleetAPI) DestroyUnit(unit string) error {
	log.Printf("INFO Destroying unit %s\n", unit)
	return lapi.API.DestroyUnit(unit)
}

func (lapi loggingFleetAPI) SetUnitTargetState(name, desiredState string) error {
	log.Printf("INFO Setting target state for %s to %s\n", name, desiredState)
	return lapi.API.SetUnitTargetState(name, desiredState)
}

func (api noDestroyFleetAPI) DestroyUnit(name string) error {
	log.Printf("INFO skipping destroying for unit %v\n", name)
	return nil
}
