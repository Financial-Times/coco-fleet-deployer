##coco-fleet-deployer

###An application that continously checks the desired state of a cluster and deploys the new/changed services to the Fleet cluster.

Installation:

```
go get github.com/Financial-Times/coco-fleet-deployer
```

Usage example:

```
coco-fleet-deployer -fleetEndpoint="http://1.2.3.4:49153" -serviceFilesUri="https://raw.githubusercontent.com/Financial-Times/fleet/master/service-files/" -servicesDefinitionFileUri="https://raw.githubusercontent.com/Financial-Times/fleet/master/services.yaml"
```

Also supports socks5 proxies, primarily for ease of use via ssh tunnelling during testing:

```
ssh -D2323 $FLEETCTL_TUNNEL
```

remain logged in and do

```
coco-fleet-deployer -fleetEndpoint="http://ip-172-31-19-102.eu-west-1.compute.internal:49153" -serviceFilesUri="https://raw.githubusercontent.com/Financial-Times/fleet/master/service-files/" -servicesDefinitionFileUri="https://raw.githubusercontent.com/Financial-Times/fleet/master/services.yaml" -intervalInSecondsBetweenDeploys=60 -destroy=false -socksProxy="127.0.0.1:2323"

```
