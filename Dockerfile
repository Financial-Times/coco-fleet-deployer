FROM gliderlabs/alpine:3.2
ADD coco-fleet-deployer /coco-fleet-deployer
CMD /coco-fleet-deployer -fleetEndpoint=$FLEET_ENDPOINT -servicesDefinitionFileUri=$SERVICES_DEFINITION_FILE_URI -destroy=$DESTROY

