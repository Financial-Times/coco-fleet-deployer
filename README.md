#coco-fleet-deployer

An application that continously checks the desired state of a cluster and deploys the new/changed services to the Fleet cluster.

Installation:

```bash
go get github.com/Financial-Times/coco-fleet-deployer
```

Usage example:

```bash
coco-fleet-deployer -fleetEndpoint="http://1.2.3.4:49153" -serviceFilesUri="https://raw.githubusercontent.com/Financial-Times/fleet/master/service-files/" -servicesDefinitionFileUri="https://raw.githubusercontent.com/Financial-Times/fleet/master/services.yaml"
```

Also supports socks5 proxies, primarily for ease of use via ssh tunnelling during testing:

```bash
#Run a tunnel
ssh -Nn -D2323 core@$FLEETCTL_TUNNEL &

#Execute the deployer
coco-fleet-deployer -fleetEndpoint="http://localhost:49153" -serviceFilesUri="https://raw.githubusercontent.com/Financial-Times/fleet/master/service-files/" -servicesDefinitionFileUri="https://raw.githubusercontent.com/Financial-Times/fleet/master/services.yaml" -intervalInSecondsBetweenDeploys=60 -destroy=false -socksProxy="127.0.0.1:2323"
```
