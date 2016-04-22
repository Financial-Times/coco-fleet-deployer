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
	err := lapi.API.CreateUnit(unit)
	if err != nil {
		log.Printf("WARN Failed to create unit %s: %v", unit.Name, err)
	}
	return err
}

func (lapi loggingFleetAPI) DestroyUnit(unit string) error {
	log.Printf("INFO Destroying unit %s\n", unit)
	err := lapi.API.DestroyUnit(unit)
	if err != nil {
		log.Printf("WARN Failed to destroy unit %s: %v", unit, err)
	}
	return err
}

func (lapi loggingFleetAPI) SetUnitTargetState(name, desiredState string) error {
	log.Printf("INFO Setting target state for %s to %s\n", name, desiredState)
	err := lapi.API.SetUnitTargetState(name, desiredState)
	if err != nil {
		log.Printf("ERROR Could not set desired state [%s] for unit [%s]", desiredState, name)
	}
	return err
}

func (api noDestroyFleetAPI) DestroyUnit(name string) error {
	log.Printf("INFO skipping destroying for unit %v\n", name)
	return nil
}
