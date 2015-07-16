from golang

RUN go get github.com/Financial-Times/coco-fleet-deployer
CMD $GOPATH/bin/coco-fleet-deployer -fleetEndpoint=$FLEET_ENDPOINT -serviceFilesUri=$SERVICE_FILES_URI -servicesDefinitionFileUri=$SERVICES_DEFINITION_FILE_URI -destroy=$DESTROY

