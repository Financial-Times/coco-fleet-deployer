FROM gliderlabs/alpine:3.2

RUN apk --update add go git\
  && export GOPATH=/.gopath \
  && go get github.com/Financial-Times/coco-fleet-deployer \
  && go build github.com/Financial-Times/coco-fleet-deployer \
  && apk del go git \
  && rm -rf $GOPATH /var/cache/apk/*

CMD /coco-fleet-deployer -fleetEndpoint=$FLEET_ENDPOINT -servicesDefinitionFileUri=$SERVICES_DEFINITION_FILE_URI -destroy=$DESTROY

