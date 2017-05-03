package dockere2e

import (
	// basic imports
	"context"
	"fmt"
	"io/ioutil"
	"strings"
	"sync"
	"testing"
	"time"

	// testify
	"github.com/stretchr/testify/require"

	// http is used to test network endpoints
	"net/http"

	// docker api
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/swarm"
)

// test for Service Discovery in swarm tasks
func TestServiceDiscovery(t *testing.T) {
	name := "TestServiceDiscovery"
	testContext, _ := context.WithTimeout(context.Background(), 2*time.Minute)
	// create a client
	cli, err := GetClient()
	require.NoError(t, err, "Client creation failed")

	nwName := getUniqueName("TestServiceDiscoveryOverlay")
	nc := types.NetworkCreate{
		Driver:         "overlay",
		CheckDuplicate: true,
	}
	_, err = cli.NetworkCreate(testContext, nwName, nc)
	require.NoError(t, err, "Error creating overlay network %s", nwName)
	defer cli.NetworkRemove(testContext, nwName)

	var replicas uint64 = 3
	spec := CannedServiceSpec(cli, name, replicas, []string{"util", "test-service-discovery"}, []string{nwName})

	// create the service
	service, err := cli.ServiceCreate(testContext, spec, types.ServiceCreateOptions{})
	require.NoError(t, err, "Error creating service %s", name)
	defer func() {
		CleanTestServices(testContext, cli, name)
		// Wait for the tasks to be removed before deleting the network
		// TODO: covert to WaitForConverge for consistency
		time.Sleep(3 * time.Second)
	}()

	// make sure the service is up
	ctx, _ := context.WithTimeout(testContext, 60*time.Second)
	scaleCheck := ScaleCheck(service.ID, cli)
	err = WaitForConverge(ctx, 1*time.Second, scaleCheck(ctx, int(replicas)))
	require.NoError(t, err)

	endpoint, published, err := getNodeIPPort(cli, testContext, service.ID, 80)
	require.NoError(t, err)
	port := fmt.Sprintf(":%v", published)

	qName := "tasks." + spec.Annotations.Name
	ip, err := serviceLookup(endpoint, port, qName)
	require.NoError(t, err)
	require.Equal(t, int(replicas), len(ip), "incorrect number of task IPs in service-discovery response")

	full, _, err := cli.ServiceInspectWithRaw(testContext, service.ID, types.ServiceInspectOptions{})
	//scale up & scale down the service and verify the SD entries get updated
	replicas = 4
	full.Spec.Mode.Replicated.Replicas = &replicas
	version := full.Meta.Version
	_, err = cli.ServiceUpdate(testContext, service.ID, version, full.Spec, types.ServiceUpdateOptions{})
	require.NoError(t, err)
	err = WaitForConverge(ctx, 1*time.Second, scaleCheck(ctx, int(replicas)))
	require.NoError(t, err)

	full, _, err = cli.ServiceInspectWithRaw(testContext, service.ID, types.ServiceInspectOptions{})
	replicas = 2
	full.Spec.Mode.Replicated.Replicas = &replicas
	version = full.Meta.Version
	_, err = cli.ServiceUpdate(testContext, service.ID, version, full.Spec, types.ServiceUpdateOptions{})
	require.NoError(t, err)
	err = WaitForConverge(ctx, 1*time.Second, scaleCheck(ctx, int(replicas)))
	require.NoError(t, err)

	ip, err = serviceLookup(endpoint, port, qName)
	require.NoError(t, err)
	require.Equal(t, int(replicas), len(ip), "incorrect number of task IPs in service-discovery response")
}

// test for unmanaged container creation on an attached network and SD for unmanaged
// containers from service tasks.
func TestAttachableNetwork(t *testing.T) {
	name := "TestAttachableNetwork"
	testContext, _ := context.WithTimeout(context.Background(), 2*time.Minute)
	// create a client
	cli, err := GetClient()
	require.NoError(t, err, "Client creation failed")

	nwName := getUniqueName("TestAttachableNetwork")
	nc := types.NetworkCreate{
		Driver:         "overlay",
		CheckDuplicate: true,
		Attachable:     true,
	}
	_, err = cli.NetworkCreate(testContext, nwName, nc)
	require.NoError(t, err, "Error creating overlay network %s", nwName)
	defer cli.NetworkRemove(testContext, nwName)

	image := GetSelfImage(cli)
	if _, _, err := cli.ImageInspectWithRaw(context.TODO(), image); err != nil {
		ticker := time.NewTicker(2 * time.Minute)
		ch := make(chan bool)
		go func(ch chan bool) {
			r, err := cli.ImagePull(context.TODO(), image, types.ImagePullOptions{})
			require.NoError(t, err, "Error pulling the image, %s", image)
			_, err = ioutil.ReadAll(r)
			r.Close()
			require.NoError(t, err, "Error reading pull response")
			ch <- true
		}(ch)
		select {
		case <-ticker.C:
			require.Error(t, fmt.Errorf("Image %s pull timed out", image))
		case <-ch:
		}
	}
	config := &container.Config{
		Image: image,
		Cmd:   []string{"util", "test-server"},
	}
	hostConfig := &container.HostConfig{
		AutoRemove:  true,
		NetworkMode: container.NetworkMode(nwName),
	}
	resp, err := cli.ContainerCreate(testContext, config, hostConfig, nil, "test-container")
	require.NoError(t, err)
	err = cli.ContainerStart(testContext, resp.ID, types.ContainerStartOptions{})
	require.NoError(t, err)

	spec := CannedServiceSpec(cli, name, 1, []string{"util", "test-service-discovery"}, []string{nwName})
	// create the service
	service, err := cli.ServiceCreate(testContext, spec, types.ServiceCreateOptions{})
	require.NoError(t, err, "Error creating service %s", name)
	defer func() {
		CleanTestServices(testContext, cli, name)
		// Wait for the tasks to be removed before deleting the network
		// TODO: covert to WaitForConverge for consistency
		time.Sleep(3 * time.Second)
	}()

	// make sure the service is up
	ctx, _ := context.WithTimeout(testContext, 60*time.Second)
	scaleCheck := ScaleCheck(service.ID, cli)
	err = WaitForConverge(ctx, 1*time.Second, scaleCheck(ctx, 1))
	require.NoError(t, err)

	endpoint, published, err := getNodeIPPort(cli, testContext, service.ID, 80)
	require.NoError(t, err)
	port := fmt.Sprintf(":%v", published)

	ip, err := serviceLookup(endpoint, port, "test-container")
	require.NoError(t, err)
	require.Equal(t, 1, len(ip), "incorrect number of task IPs in service-discovery response")

	err = cli.ContainerRemove(testContext, resp.ID, types.ContainerRemoveOptions{Force: true})
	require.NoError(t, err)
}

// tests the load balancer for services with public endpoints
func TestNetworkExternalLb(t *testing.T) {
	// TODO(dperny): there are debugging statements commented out. remove them.
	t.Parallel()
	name := "TestNetworkExternalLb"
	testContext, _ := context.WithTimeout(context.Background(), 2*time.Minute)
	// create a client
	cli, err := GetClient()
	require.NoError(t, err, "Client creation failed")

	replicas := 3
	spec := CannedServiceSpec(cli, name, uint64(replicas), nil, nil)
	// expose a port
	spec.EndpointSpec = &swarm.EndpointSpec{
		Mode: swarm.ResolutionModeVIP,
		Ports: []swarm.PortConfig{
			{
				Protocol:   swarm.PortConfigProtocolTCP,
				TargetPort: 80,
			},
		},
	}

	// create the service
	service, err := cli.ServiceCreate(testContext, spec, types.ServiceCreateOptions{})
	require.NoError(t, err, "Error creating service")
	require.NotNil(t, service, "Resp is nil for some reason")
	require.NotZero(t, service.ID, "serviceonse ID is zero, something is amiss")
	defer CleanTestServices(testContext, cli, name)

	// now make sure the service comes up
	ctx, _ := context.WithTimeout(testContext, 60*time.Second)
	scaleCheck := ScaleCheck(service.ID, cli)
	err = WaitForConverge(ctx, 1*time.Second, scaleCheck(ctx, 3))
	require.NoError(t, err)

	var published uint32
	full, _, err := cli.ServiceInspectWithRaw(testContext, service.ID, types.ServiceInspectOptions{})
	require.NoError(t, err, "Error getting newly created service")
	for _, port := range full.Endpoint.Ports {
		if port.TargetPort == 80 {
			published = port.PublishedPort
			break
		}
	}
	port := fmt.Sprintf(":%v", published)

	// create a context, and also grab the cancelfunc
	ctx, cancel := context.WithTimeout(testContext, 60*time.Second)

	// alright now comes the tricky part. we're gonna hit the endpoint
	// repeatedly until we get 3 different container ids, twice each.
	// if we hit twice each, we know that we've been LB'd around to each
	// instance. why twice? seems like a good number, idk. when i test LB
	// manually i just hit the endpoint a few times until i've seen each
	// container a couple of times

	// create a map to store all the containers we've seen
	containers := make(map[string]int)
	// create a mutex to synchronize access to this map
	mu := new(sync.Mutex)

	// select the network endpoint we're going to hit
	// list the nodes
	ips, err := GetNodeIps(cli)
	require.NoError(t, err, "error listing nodes to get IP")
	require.NotZero(t, ips, "no node ip addresses were returned")
	// take the first node
	endpoint := ips[0]

	// first we need a function to poll containers, and let it run
	go func() {
		for {
			select {
			case <-ctx.Done():
				// stop polling when ctx is done
				return
			default:
				// anonymous func to leverage defers
				func() {
					// TODO(dperny) consider breaking out into separate function
					// lock the mutex to synchronize access to the map
					mu.Lock()
					defer mu.Unlock()
					tr := &http.Transport{}
					client := &http.Client{Transport: tr, Timeout: time.Duration(5 * time.Second)}

					// poll the endpoint
					// TODO(dperny): this string concat is probably Bad
					resp, err := client.Get("http://" + endpoint + port)
					if err != nil {
						// TODO(dperny) properly handle error
						// fmt.Printf("error: %v\n", err)
						return
					}
					defer resp.Body.Close()
					name := resp.Header.Get("Host")

					if name == "" {
						// body text should just be the container id
						namebytes, err := ioutil.ReadAll(resp.Body)
						// docs say we have to close the body. defer doing so
						if err != nil {
							// TODO(dperny) properly handle error
							return
						}
						name = strings.TrimSpace(string(namebytes))
					}
					// fmt.Printf("saw %v\n", name)

					// if the container has already been seen, increment its count
					if count, ok := containers[name]; ok {
						containers[name] = count + 1
						// if not, add it as a new record with count 1
					} else {
						containers[name] = 1
					}
				}()
				// if we don't sleep, we'll starve the check function. we stop
				// just long enough for the system to schedule the check function
				// TODO(dperny): figure out a cleaner way to do this.
				time.Sleep(5 * time.Millisecond)
			}
		}
	}()

	// function to check if we've been LB'd to all containers
	checkComplete := func() error {
		mu.Lock()
		defer mu.Unlock()
		c := len(containers)
		// check if we have too many containers (unlikely but possible)
		if c > replicas {
			// cancel the context, we have overshot and will never converge
			cancel()
			return fmt.Errorf("expected %v different container IDs, got %v", replicas, c)
		}
		// now check if we have too few
		if c < replicas {
			return fmt.Errorf("haven't seen enough different containers, expected %v got %v", replicas, c)
		}
		// now check that we've hit each container at least 2 times
		for name, count := range containers {
			if count < 2 {
				return fmt.Errorf("haven't seen container %v twice", name)
			}
		}
		// if everything so far passes, we're golden
		return nil
	}

	err = WaitForConverge(ctx, time.Second, checkComplete)
	// cancel the context to stop polling
	cancel()

	require.NoError(t, err)

}
