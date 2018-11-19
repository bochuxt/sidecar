package discovery

import (
	"fmt"
	"strconv"
	"sync"
	"time"

	director "github.com/relistan/go-director"
	log "github.com/sirupsen/logrus"

	"github.com/Nitro/sidecar/service"
	"github.com/fsouza/go-dockerclient"
)

const (
	CacheDrainInterval = 10 * time.Minute // Drain the cache every 10 mins
)

type DockerClient interface {
	InspectContainer(id string) (*docker.Container, error)
	ListContainers(opts docker.ListContainersOptions) ([]docker.APIContainers, error)
	AddEventListener(listener chan<- *docker.APIEvents) error
	RemoveEventListener(listener chan *docker.APIEvents) error
	Ping() error
}

type DockerDiscovery struct {
	events         chan *docker.APIEvents       // Where events are announced to us
	endpoint       string                       // The Docker endpoint to talk to
	services       []*service.Service           // The list of services we know about
	ClientProvider func() (DockerClient, error) // Return the client we'll use to connect
	serviceNamer   ServiceNamer                 // The service namer implementation
	advertiseIp    string                       // The address we'll advertise for services
	containerCache *ContainerCache              // Stores full container data for fast lookups
	sleepInterval  time.Duration                // The sleep interval for event processing and reconnection
	sync.RWMutex                                // Reader/Writer lock
}

func NewDockerDiscovery(endpoint string, svcNamer ServiceNamer, ip string) *DockerDiscovery {
	discovery := DockerDiscovery{
		endpoint:       endpoint,
		events:         make(chan *docker.APIEvents),
		containerCache: NewContainerCache(),
		serviceNamer:   svcNamer,
		advertiseIp:    ip,
		sleepInterval:  DefaultSleepInterval,
	}

	// Default to our own method for returning this
	discovery.ClientProvider = discovery.getDockerClient

	return &discovery
}

func (d *DockerDiscovery) getDockerClient() (DockerClient, error) {
	if d.endpoint != "" {
		client, err := docker.NewClient(d.endpoint)
		if err != nil {
			return nil, err
		}

		return client, nil
	}

	client, err := docker.NewClientFromEnv()
	if err != nil {
		return nil, err
	}
	return client, nil
}

// HealthCheck looks up a health check using Docker container labels to
// pass the type of check and the arguments to pass to it.
func (d *DockerDiscovery) HealthCheck(svc *service.Service) (string, string) {
	container, err := d.inspectContainer(svc)
	if err != nil {
		return "", ""
	}

	return container.Config.Labels["HealthCheck"], container.Config.Labels["HealthCheckArgs"]
}

func (d *DockerDiscovery) inspectContainer(svc *service.Service) (*docker.Container, error) {
	// If we have it cached, return it!
	container := d.containerCache.Get(svc.ID)
	if container != nil {
		return container, nil
	}

	// New connection every time
	client, err := d.ClientProvider()
	if err != nil {
		log.Errorf("Error when creating Docker client: %s\n", err.Error())
		return nil, err
	}

	container, err = client.InspectContainer(svc.ID)
	if err != nil {
		log.Errorf("Error inspecting container : %v\n", svc.ID)
		return nil, err
	}

	// Cache it for next time
	d.containerCache.Set(svc, container)

	return container, nil
}

// The main loop, poll for containers continuously.
func (d *DockerDiscovery) Run(looper director.Looper) {
	connQuitChan := make(chan bool)

	go d.manageConnection(connQuitChan)

	go func() {
		// Loop around, process any events which came in, and
		// periodically fetch the whole container list
		looper.Loop(func() error {
			select {
			case event := <-d.events:
				if event == nil {
					// This usually happens because of a Docker restart.
					// Sleep, let us reconnect in the background, then loop.
					return nil
				}
				log.Debugf("Event: %#v\n", event)
				d.handleEvent(*event)
			case <-time.After(d.sleepInterval):
				d.getContainers()
			case <-time.After(CacheDrainInterval):
				d.containerCache.Drain(len(d.services))
			}

			return nil
		})

		// Propagate quit channel message
		close(connQuitChan)
	}()
}

// Services returns the slice of services we found running
func (d *DockerDiscovery) Services() []service.Service {
	d.RLock()
	defer d.RUnlock()

	svcList := make([]service.Service, len(d.services))

	for i, svc := range d.services {
		svcList[i] = *svc
	}

	return svcList
}

// Listeners returns any containers we found that had the
// SidecarListener label set to a valid ServicePort.
func (d *DockerDiscovery) Listeners() []ChangeListener {
	var listeners []ChangeListener

	for _, cntnr := range d.services {
		container, err := d.inspectContainer(cntnr)
		if err != nil {
			continue
		}

		listener := d.listenerForContainer(container)
		if listener != nil {
			listeners = append(listeners, *listener)
		}
	}

	return listeners
}

func (d *DockerDiscovery) findServiceByID(id string) *service.Service {
	for _, svc := range d.services {
		if svc.ID == id {
			return svc
		}
	}

	return nil
}

// listenerForContainer returns a ChangeListener for a container if one
// is configured.
func (d *DockerDiscovery) listenerForContainer(cntnr *docker.Container) *ChangeListener {
	// See if the container has the SidecarListener label, which
	// will tell us the ServicePort of the port that should be
	// subscribed to Sidecar events.
	svcPortStr, ok := cntnr.Config.Labels["SidecarListener"]
	if !ok {
		return nil
	}

	// Be careful about ID matching
	id := cntnr.ID
	if len(id) > 12 {
		id = id[:12]
	}

	svc := d.findServiceByID(id)
	if svc == nil {
		return nil
	}

	listenPort := portForServicePort(svc, svcPortStr, "tcp") // We only do HTTP (TCP)
	// -1 is returned when there is no match
	if listenPort == nil {
		log.Warnf(
			"SidecarListener label found on %s, but no matching ServicePort! '%s'",
			svc.ID, svcPortStr,
		)
		return nil
	}

	return &ChangeListener{
		Name: svc.ListenerName(),
		Url:  fmt.Sprintf("http://%s:%d/sidecar/update", listenPort.IP, listenPort.Port),
	}
}

// portForServicePort is similar to service.PortForServicePort, but takes a string
// and returns a full service.Port, not just the integer.
func portForServicePort(svc *service.Service, portStr string, pType string) *service.Port {
	// Look up the ServicePort and translate to Docker port
	svcPort, err := strconv.ParseInt(portStr, 10, 64)
	if err != nil {
		log.Warnf(
			"SidecarListener label found on %s, can't decode port '%s'",
			svc.ID, portStr,
		)
		return nil
	}

	for _, port := range svc.Ports {
		if port.ServicePort == svcPort && port.Type == pType {
			return &port
		}
	}

	return nil
}

func (d *DockerDiscovery) getContainers() {
	// New connection every time
	client, err := d.ClientProvider()
	if err != nil {
		log.Errorf("Error when creating Docker client: %s\n", err.Error())
		return
	}

	containers, err := client.ListContainers(docker.ListContainersOptions{All: false})
	if err != nil {
		return
	}

	d.Lock()
	defer d.Unlock()

	// Temporary set to track if we have seen a container (for cache pruning)
	containerMap := make(map[string]interface{})

	// Build up the service list, and prepare to prune the containerCache
	d.services = make([]*service.Service, 0, len(containers))
	for _, container := range containers {
		// Skip services that are purposely excluded from discovery.
		if container.Labels["SidecarDiscover"] == "false" {
			continue
		}

		svc := service.ToService(&container, d.advertiseIp)
		svc.Name = d.serviceNamer.ServiceName(&container)
		d.services = append(d.services, &svc)
		containerMap[svc.ID] = true
	}

	d.containerCache.Prune(containerMap)
}

func (d *DockerDiscovery) configureDockerConnection() DockerClient {
	client, err := d.ClientProvider()
	if err != nil {
		log.Errorf("Error creating Docker client: %s", err)
		return nil
	}

	err = client.AddEventListener(d.events)
	if err != nil {
		log.Errorf("Error adding Docker client event listener: %s", err)
		return nil
	}

	return client
}

func (d *DockerDiscovery) manageConnection(quit chan bool) {
	client := d.configureDockerConnection()

	// Health check the connection and set it back up when it goes away.
	for {
		// Is the client connected?
		if client == nil || client.Ping() != nil {
			log.Warn("Lost connection to Docker, re-connecting")
			if client != nil {
				// Swallow errors since we're overwriting the client anyway
				_ = client.RemoveEventListener(d.events)
			}
			d.events = make(chan *docker.APIEvents) // RemoveEventListener closes it

			client = d.configureDockerConnection()
		}

		select {
		case <-quit:
			return
		default:
		}

		// Sleep a bit before attempting to reconnect
		time.Sleep(d.sleepInterval)
	}
}

func (d *DockerDiscovery) handleEvent(event docker.APIEvents) {
	// We're only worried about stopping containers
	if event.Status == "die" || event.Status == "stop" {
		d.Lock()
		defer d.Unlock()

		for i, service := range d.services {
			if len(event.ID) < 12 {
				continue
			}
			if event.ID[:12] == service.ID {
				log.Printf("Deleting %s based on Docker '%s' event\n", service.ID, event.Status)
				// Delete the entry in the slice
				d.services[i] = nil
				d.services = append(d.services[:i], d.services[i+1:]...)
				// Once we found a match, return
				return
			}
		}
	}
}

// A ContainerCache keeps a history of the containers we've inspected
// in order to do fast lookups of container info when needed.
type ContainerCache struct {
	cache map[string]*docker.Container // Cache of inspected containers
	sync.RWMutex
}

func NewContainerCache() *ContainerCache {
	return &ContainerCache{
		cache: make(map[string]*docker.Container),
	}
}

// On a timed basis, drain the containerCache
func (c *ContainerCache) Drain(newSize int) {
	c.Lock()
	defer c.Unlock()
	// Make a new one, leave the old one for GC
	c.cache = make(map[string]*docker.Container, newSize)
}

// Loop through the current cache and remove anything that has disappeared
func (c *ContainerCache) Prune(liveContainers map[string]interface{}) {
	c.Lock()
	defer c.Unlock()

	for id := range c.cache {
		if _, ok := liveContainers[id]; !ok {
			delete(c.cache, id)
		}
	}
}

// Get locks the cache, try to get a service if we have it
func (c *ContainerCache) Get(svcID string) *docker.Container {
	c.RLock()
	defer c.RUnlock()

	if container, ok := c.cache[svcID]; ok {
		return container
	}

	return nil
}

func (c *ContainerCache) Set(svc *service.Service, container *docker.Container) {
	c.Lock()
	defer c.Unlock()
	c.cache[svc.ID] = container
}

func (c *ContainerCache) Len() int {
	c.RLock()
	defer c.RUnlock()
	return len(c.cache)
}
