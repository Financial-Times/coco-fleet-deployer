package main

import (
	"flag"
	"fmt"
	fthealth "github.com/Financial-Times/go-fthealth/v1a"
	"github.com/coreos/fleet/schema"
	"github.com/gorilla/handlers"
	"github.com/gorilla/mux"
	"github.com/kr/pretty"
	"log"
	"net/http"
	"time"
)

//TODO add env var support
var (
	destroyFlag             = flag.Bool("destroy", false, "Destroy units not found in the definition")
	fleetEndpoint           = flag.String("fleetEndpoint", "", "Fleet API http endpoint: `http://host:port`")
	socksProxy              = flag.String("socksProxy", "", "address of socks proxy, e.g., 127.0.0.1:9050")
	destroyServiceBlacklist = map[string]struct{}{"deployer.service": struct{}{}, "deployer.timer": struct{}{}}
	rootURI                 = flag.String("rootURI", "", "Base uri to use when constructing service file URI. Only used if service file URI is relative.")
	isDebug                 = flag.Bool("isDebug", false, "Enable to show debug logs.")
	etcdURL                 = flag.String("etcd-url", "http://localhost:2379", "etcd URL")
	healthURLPrefix         = flag.String("health-url-prefix", "http://localhost:8080", "health URL prefix")
	healthEndpoint          = flag.String("health-endpoint", "__health", "health endpoint")
	serviceNamePrefix       = flag.String("service-name-prefix", "__", "service name prefix")
	healthBusinessImpact    = flag.String("business-impact", "Cannot retrieve service files", "business impact of health check")
	appPort                 = flag.String("app-port", "8080", "Port of the app")
)

const (
	serviceDescription = "Service that deploys other services into Coco cluster"
	serviceName        = "Deployer"
)

type services struct {
	Services []service `yaml:"services"`
}

type service struct {
	Name                 string `yaml:"name"`
	Version              string `yaml:"version"`
	Count                int    `yaml:"count"`
	URI                  string `yaml:"uri"`
	DesiredState         string `yaml:"desiredState"`
	SequentialDeployment bool   `yaml:"sequentialDeployment"`
}

type serviceDefinitionClient interface {
	servicesDefinition() (services, error)
	serviceFile(service service) ([]byte, error)
	checkServiceFilesRepoHealth() error
	getRootURI() string
}

type serviceGroup struct {
	serviceNodes []*schema.Unit
	sidekicks    []*schema.Unit
	isZDD        bool
}

type healthcheckResponse struct {
	Name   string
	Checks []struct {
		Name string
		OK   bool
	}
}

func main() {
	flag.Parse()
	if *fleetEndpoint == "" {
		log.Fatal("Fleet endpoint is required")
	}

	if *rootURI == "" {
		log.Fatal("Services definition file uri is required")
	}
	d, err := newDeployer()
	if err != nil {
		log.Printf("ERROR Error creating new deployer: [%s]", err.Error())
		panic(err)
	}

	if d.isDebug {
		log.Printf("DEBUG Using configuration: \n %# v \n", pretty.Formatter(d))
	}

	go setupHealthCheckHandler(d, *appPort)

	for {
		log.Print("Starting deploy run")
		if err := d.deployAll(); err != nil {
			log.Printf("Failed to run deploy : %v\n", err.Error())
		}
		log.Print("Finished deploy run")
		time.Sleep(1 * time.Minute)
	}
}

func setupHealthCheckHandler(d *deployer, appPort string) {
	r := mux.NewRouter()
	r.Path("/__health").Handler(handlers.MethodHandler{"GET": http.HandlerFunc(fthealth.Handler(serviceName, serviceDescription, d.servicesDefinitionClientHealthCheck()))})
	http.ListenAndServe(fmt.Sprintf(":%s", appPort), r)
	log.Printf("Started listening on port %s", appPort)
}
