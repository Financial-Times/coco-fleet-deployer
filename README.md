#coco-fleet-deployer

[![Circle CI](https://circleci.com/gh/Financial-Times/coco-fleet-deployer/tree/master.png?style=shield)](https://circleci.com/gh/Financial-Times/coco-fleet-deployer/tree/master)

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
coco-fleet-deployer -fleetEndpoint="http://1.2.3.4:49153" -rootURI="https://raw.githubusercontent.com/Financial-Times/fleet/master/service-files -destroy=true"
```

Also supports socks5 proxies, primarily for ease of use via ssh tunnelling during testing:

```bash
#Run a tunnel
ssh -Nn -D2323 core@$FLEETCTL_TUNNEL &

#Execute the deployer 
coco-fleet-deployer -fleetEndpoint="http://localhost:49153" -rootURI="https://raw.githubusercontent.com/Financial-Times/fleet/master/service-files/" -destroy=true -socksProxy="127.0.0.1:2323"
```

# Deployer workflow

- read services.yaml
- read service files - wanted services
- compare wanted services with actual services (cached state of the actual, taken when last changing services)
- identify and implement differences
- invalidate cache, on next run cache current state
- service state is cached to avoid overloading fleet by getting service state on every run

## What the deployer does

- applies changes in the services files to the cluster
- caches service state after each change and on every restart
- restarts (and clears cache) every time it has trouble connecting to GH or fleet

## What the deployer doesn't do

- identify changes made manually in the cluster and restore the wanted state - this used to be the case before the cache was introduced (don't think of the deployer as puppet)

# Features

## Debug logging

- disabled by default
- set the `/ft/config/deployer/is-debug` to `true` and restart the deployer - it should now show debug logs (see `Debug log enabled.` log on startup)
- to disable, delete the key or set it to false
- see the code for examples on how to add you own debug logs!

## Change in node count in services.yaml supported

- if you increase the node count in the services.yaml, deployer will create the extra nodes
- if you decrease the node count in the services.yaml, deployer will deleted the extra nodes, in reverse numerical order
- for the deleted nodes, you may have to manually delete the healthcheck entries from etcd `/ft/services/${SERVICE}/healthcheck` so they drop off the agg-hc


## Q & A

### Should I stop the deployer when doing manual changes in the cluster?

Yes, even if the deployer doesn't actively look for those changes, it might restart (connection error to GH for example) and pick up your changes.
