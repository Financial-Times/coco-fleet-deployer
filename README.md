#coco-fleet-deployer

An application that continuously checks the desired state of a cluster and deploys the new/changed services to the Fleet cluster.

Building
```
#Build go bin
go get
CGO_ENABLED=0 go build -a -installsuffix cgo -o coco-fleet-deployer .

#Build the docker image
docker build -t coco/coco-fleet-deployer .
```

Usage example:

```bash
coco-fleet-deployer -fleetEndpoint="http://1.2.3.4:49153" -rootURI="https://api.github.com/repos/Financial-Times/up-service-files/contents/" -branchRef="pre-prod" -destroy=true -token="githubToken"
```

Also supports socks5 proxies, primarily for ease of use via ssh tunnelling during testing:

```bash
#Run a tunnel
ssh -Nn -D2323 core@$FLEETCTL_TUNNEL &

#Execute the deployer
coco-fleet-deployer -fleetEndpoint="http://localhost:49153" -rootURI="https://api.github.com/repos/Financial-Times/up-service-files/contents/" -branchRef="pre-prod" -destroy=true -token="githubToken" -socksProxy="127.0.0.1:2323"
```
