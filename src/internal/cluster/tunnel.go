// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: 2021-Present The Zarf Authors

// Package cluster contains zarf-specific cluster management functions
package cluster

// Forked from https://github.com/gruntwork-io/terratest/blob/v0.38.8/modules/k8s/tunnel.go

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/defenseunicorns/zarf/src/config/lang"
	"github.com/defenseunicorns/zarf/src/types"

	"github.com/defenseunicorns/zarf/src/config"
	"github.com/defenseunicorns/zarf/src/pkg/k8s"
	"github.com/defenseunicorns/zarf/src/pkg/message"
	"github.com/defenseunicorns/zarf/src/pkg/utils"
	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

// Global lock to synchronize port selections
var globalMutex sync.Mutex

const (
	PodResource  = "pod"
	SvcResource  = "svc"
	ZarfRegistry = "REGISTRY"
	ZarfLogging  = "LOGGING"
	ZarfGit      = "GIT"
	ZarfInjector = "INJECTOR"

	// See https://regex101.com/r/OWVfAO/1
	serviceURLPattern = `^(?P<name>[^\.]+)\.(?P<namespace>[^\.]+)\.svc\.cluster\.local$`
)

// Tunnel is the main struct that configures and manages port forwading tunnels to Kubernetes resources.
type Tunnel struct {
	kube         *k8s.K8s
	out          io.Writer
	autoOpen     bool
	localPort    int
	remotePort   int
	namespace    string
	resourceType string
	resourceName string
	urlSuffix    string
	attempt      int
	stopChan     chan struct{}
	readyChan    chan struct{}
	spinner      *message.Spinner
}

// GenerateConnectionTable will print a table of all zarf connect matches found in the cluster
func (c *Cluster) PrintConnectTable() error {
	list, err := c.Kube.GetServicesByLabelExists(v1.NamespaceAll, config.ZarfConnectLabelName)
	if err != nil {
		return err
	}

	connections := make(types.ConnectStrings)

	for _, svc := range list.Items {
		name := svc.Labels[config.ZarfConnectLabelName]

		// Add the connectstring for processing later in the deployment
		connections[name] = types.ConnectString{
			Description: svc.Annotations[config.ZarfConnectAnnotationDescription],
			Url:         svc.Annotations[config.ZarfConnectAnnotationUrl],
		}
	}

	message.PrintConnectStringTable(connections)

	return nil
}

// IsServiceURL will check if the provided string is a valid serviceURL based on if it properly matches a validating regexp
func IsServiceURL(serviceURL string) bool {
	parsedURL, err := url.Parse(serviceURL)
	if err != nil {
		return false
	}

	// Match hostname against local cluster service format
	pattern := regexp.MustCompile(serviceURLPattern)
	matches := pattern.FindStringSubmatch(parsedURL.Hostname())

	// If incomplete match, return an error
	return len(matches) == 3
}

// NewTunnelFromServiceURL takes a serviceURL and parses it to create a tunnel to the cluster. The string is expected to follow the following format:
// Example serviceURL: http://{SERVICE_NAME}.{NAMESPACE}.svc.cluster.local:{PORT}
func NewTunnelFromServiceURL(serviceURL string) (*Tunnel, error) {
	parsedURL, err := url.Parse(serviceURL)
	if err != nil {
		return nil, err
	}

	// Get the remote port from the serviceURL
	remotePort, err := strconv.Atoi(parsedURL.Port())
	if err != nil {
		return nil, err
	}

	// Match hostname against local cluster service format
	pattern := regexp.MustCompile(serviceURLPattern)
	matches := pattern.FindStringSubmatch(parsedURL.Hostname())

	// If incomplete match, return an error
	if len(matches) != 3 {
		return nil, lang.ErrNotAServiceURL
	}

	// Use the matched values to create a new tunnel
	name := matches[pattern.SubexpIndex("name")]
	namespace := matches[pattern.SubexpIndex("namespace")]

	return NewTunnel(namespace, SvcResource, name, 0, remotePort)
}

// NewTunnel will create a new Tunnel struct
// Note that if you use 0 for the local port, an open port on the host system
// will be selected automatically, and the Tunnel struct will be updated with the selected port.
func NewTunnel(namespace, resourceType, resourceName string, local, remote int) (*Tunnel, error) {
	message.Debugf("tunnel.NewTunnel(%s, %s, %s, %d, %d)", namespace, resourceType, resourceName, local, remote)

	kube, err := k8s.NewWithWait(message.Debugf, labels, defaultTimeout)
	if err != nil {
		return &Tunnel{}, err
	}

	return &Tunnel{
		out:          io.Discard,
		localPort:    local,
		remotePort:   remote,
		namespace:    namespace,
		resourceType: resourceType,
		resourceName: resourceName,
		stopChan:     make(chan struct{}, 1),
		readyChan:    make(chan struct{}, 1),
		kube:         kube,
	}, nil
}

func NewZarfTunnel() (*Tunnel, error) {
	return NewTunnel(ZarfNamespace, SvcResource, "", 0, 0)
}

func (tunnel *Tunnel) EnableAutoOpen() {
	tunnel.autoOpen = true
}

func (tunnel *Tunnel) AddSpinner(spinner *message.Spinner) {
	tunnel.spinner = spinner
}

func (tunnel *Tunnel) Connect(target string, blocking bool) error {
	message.Debugf("tunnel.Connect(%s, %#v)", target, blocking)

	switch strings.ToUpper(target) {
	case ZarfRegistry:
		tunnel.resourceName = "zarf-docker-registry"
		tunnel.remotePort = 5000
		tunnel.urlSuffix = `/v2/_catalog`

	case ZarfLogging:
		tunnel.resourceName = "zarf-loki-stack-grafana"
		tunnel.remotePort = 3000
		// Start the logs with something useful
		tunnel.urlSuffix = `/monitor/explore?orgId=1&left=%5B"now-12h","now","Loki",%7B"refId":"Zarf%20Logs","expr":"%7Bnamespace%3D%5C"zarf%5C"%7D"%7D%5D`

	case ZarfGit:
		tunnel.resourceName = "zarf-gitea-http"
		tunnel.remotePort = 3000

	case ZarfInjector:
		tunnel.resourceName = "zarf-injector"
		tunnel.remotePort = 5000

	default:
		if target != "" {
			if err := tunnel.checkForZarfConnectLabel(target); err != nil {
				message.Errorf(err, "Problem looking for a zarf connect label in the cluster")
			}
		}

		if tunnel.resourceName == "" {
			return fmt.Errorf("missing resource name")
		}
		if tunnel.remotePort < 1 {
			return fmt.Errorf("missing remote port")
		}
	}

	url, err := tunnel.establish()

	// Try to etablish the tunnel up to 3 times
	if err != nil {
		tunnel.attempt++
		// If we have exceeded the number of attempts, exit with an error
		if tunnel.attempt > 3 {
			return fmt.Errorf("unable to establish tunnel after 3 attempts: %w", err)
		} else {
			// Otherwise, retry the connection but delay increasing intervals between attempts
			delay := tunnel.attempt * 10
			message.Debug(err)
			message.Infof("Delay creating tunnel, waiting %d seconds...", delay)
			time.Sleep(time.Duration(delay) * time.Second)
			tunnel.Connect(target, blocking)
		}
	}

	if blocking {
		// Otherwise, if this is blocking it is coming from a user request so try to open the URL, but ignore errors
		if tunnel.autoOpen {
			if err := utils.ExecLaunchURL(url); err != nil {
				message.Debug(err)
			}
		}

		// Dump the tunnel URL to the console for other tools to use
		fmt.Print(url)

		// Since this blocking, set the defer now so it closes properly on sigterm
		defer tunnel.Close()

		// Keep this open until an interrupt signal is received
		c := make(chan os.Signal)
		signal.Notify(c, os.Interrupt, syscall.SIGTERM)
		go func() {
			<-c
			os.Exit(0)
		}()

		for {
			runtime.Gosched()
		}
	}

	return nil
}

// Endpoint returns the tunnel endpoint
func (tunnel *Tunnel) Endpoint() string {
	message.Debug("tunnel.Endpoint()")
	return fmt.Sprintf("127.0.0.1:%d", tunnel.localPort)
}

// HttpEndpoint returns the tunnel endpoint as a HTTP URL string
func (tunnel *Tunnel) HttpEndpoint() string {
	message.Debug("tunnel.HttpEndpoint()")
	return fmt.Sprintf("http://%s", tunnel.Endpoint())
}

// Close disconnects a tunnel connection by closing the StopChan, thereby stopping the goroutine.
func (tunnel *Tunnel) Close() {
	message.Debug("tunnel.Close()")
	close(tunnel.stopChan)
}

func (tunnel *Tunnel) checkForZarfConnectLabel(name string) error {
	message.Debugf("tunnel.checkForZarfConnectLabel(%s)", name)
	var spinner *message.Spinner
	var err error

	spinnerMessage := "Looking for a Zarf Connect Label in the cluster"
	if tunnel.spinner != nil {
		spinner = tunnel.spinner
		spinner.Updatef(spinnerMessage)
	} else {
		spinner = message.NewProgressSpinner(spinnerMessage)
		defer spinner.Stop()
	}

	matches, err := tunnel.kube.GetServicesByLabel("", config.ZarfConnectLabelName, name)
	if err != nil {
		return fmt.Errorf("unable to lookup the service: %w", err)
	}

	if len(matches.Items) > 0 {
		// If there is a match, use the first one as these are supposed to be unique
		svc := matches.Items[0]

		// Reset based on the matched params
		tunnel.resourceType = SvcResource
		tunnel.resourceName = svc.Name
		tunnel.namespace = svc.Namespace
		// Only support a service with a single port
		tunnel.remotePort = svc.Spec.Ports[0].TargetPort.IntValue()

		// Add the url suffix too
		tunnel.urlSuffix = svc.Annotations[config.ZarfConnectAnnotationUrl]

		message.Debugf("tunnel connection match: %s/%s on port %d", svc.Namespace, svc.Name, tunnel.remotePort)
	}

	return nil
}

// establish opens a tunnel to a kubernetes resource, as specified by the provided tunnel struct.
func (tunnel *Tunnel) establish() (string, error) {
	message.Debug("tunnel.Establish()")

	var err error
	var spinner *message.Spinner

	// Track this locally as we may need to retry if the tunnel fails
	localPort := tunnel.localPort

	// If the local-port is 0, get an available port before continuing. We do this here instead of relying on the
	// underlying port-forwarder library, because the port-forwarder library does not expose the selected local port in a
	// machine-readable manner.
	// Synchronize on the global lock to avoid race conditions with concurrently selecting the same available port,
	// since there is a brief moment between `GetAvailablePort` and `forwarder.ForwardPorts` where the selected port
	// is available for selection again.
	if localPort == 0 {
		message.Debugf("Requested local port is 0. Selecting an open port on host system")
		localPort, err = utils.GetAvailablePort()
		if err != nil {
			return "", fmt.Errorf("unable to find an available port: %w", err)
		}
		message.Debugf("Selected port %d", localPort)
		globalMutex.Lock()
		defer globalMutex.Unlock()
	}

	spinnerMessage := fmt.Sprintf("Opening tunnel %d -> %d for %s/%s in namespace %s",
		localPort,
		tunnel.remotePort,
		tunnel.resourceType,
		tunnel.resourceName,
		tunnel.namespace,
	)

	if tunnel.spinner != nil {
		spinner = tunnel.spinner
		spinner.Updatef(spinnerMessage)
	} else {
		spinner = message.NewProgressSpinner(spinnerMessage)
		defer spinner.Stop()
	}

	kube, err := k8s.NewWithWait(spinner.Debugf, labels, defaultTimeout)
	if err != nil {
		return "", fmt.Errorf("unable to connect to the cluster: %w", err)
	}

	// Find the pod to port forward to
	podName, err := tunnel.getAttachablePodForResource()
	if err != nil {
		return "", fmt.Errorf("unable to find pod attached to given resource: %w", err)
	}
	spinner.Debugf("Selected pod %s to open port forward to", podName)

	// Build url to the port forward endpoint
	// example: http://localhost:8080/api/v1/namespaces/helm/pods/tiller-deploy-9itlq/portforward
	postEndpoint := tunnel.kube.Clientset.CoreV1().RESTClient().Post()
	namespace := tunnel.namespace
	portForwardCreateURL := postEndpoint.
		Resource("pods").
		Namespace(namespace).
		Name(podName).
		SubResource("portforward").
		URL()

	spinner.Debugf("Using URL %s to create portforward", portForwardCreateURL)

	// Construct the spdy client required by the client-go portforward library
	transport, upgrader, err := spdy.RoundTripperFor(kube.RestConfig)
	if err != nil {
		return "", fmt.Errorf("unable to create the spdy client %w", err)
	}
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, "POST", portForwardCreateURL)

	// Construct a new PortForwarder struct that manages the instructed port forward tunnel
	ports := []string{fmt.Sprintf("%d:%d", localPort, tunnel.remotePort)}
	portforwarder, err := portforward.New(dialer, ports, tunnel.stopChan, tunnel.readyChan, tunnel.out, tunnel.out)
	if err != nil {
		return "", fmt.Errorf("unable to create the port forward: %w", err)
	}

	// Open the tunnel in a goroutine so that it is available in the background. Report errors to the main goroutine via
	// a new channel.
	errChan := make(chan error)
	go func() {
		errChan <- portforwarder.ForwardPorts()
	}()

	// Wait for an error or the tunnel to be ready
	select {
	case err = <-errChan:
		if tunnel.spinner == nil {
			spinner.Stop()
		}
		return "", fmt.Errorf("unable to start the tunnel: %w", err)
	case <-portforwarder.Ready:
		// Store for endpoint output
		tunnel.localPort = localPort
		url := fmt.Sprintf("http://%s:%d%s", config.IPV4Localhost, localPort, tunnel.urlSuffix)
		msg := fmt.Sprintf("Creating port forwarding tunnel at %s", url)
		if tunnel.spinner == nil {
			spinner.Successf(msg)
		} else {
			spinner.Updatef(msg)
		}
		return url, nil
	}
}

// getAttachablePodForResource will find a pod that can be port forwarded to the provided resource type and return
// the name.
func (tunnel *Tunnel) getAttachablePodForResource() (string, error) {
	message.Debug("tunnel.GettAttachablePodForResource()")
	switch tunnel.resourceType {
	case PodResource:
		return tunnel.resourceName, nil
	case SvcResource:
		return tunnel.getAttachablePodForService()
	default:
		return "", fmt.Errorf("unknown resource type: %s", tunnel.resourceType)
	}
}

// getAttachablePodForServiceE will find an active pod associated with the Service and return the pod name.
func (tunnel *Tunnel) getAttachablePodForService() (string, error) {
	message.Debug("tunnel.getAttachablePodForService()")
	service, err := tunnel.kube.GetService(tunnel.namespace, tunnel.resourceName)
	if err != nil {
		return "", fmt.Errorf("unable to find the service: %w", err)
	}
	selectorLabelsOfPods := makeLabels(service.Spec.Selector)

	servicePods := tunnel.kube.WaitForPodsAndContainers(k8s.PodLookup{
		Namespace: tunnel.namespace,
		Selector:  selectorLabelsOfPods,
	}, nil)

	return servicePods[0], nil
}

// makeLabels is a helper to format a map of label key and value pairs into a single string for use as a selector.
func makeLabels(labels map[string]string) string {
	var out []string
	for key, value := range labels {
		out = append(out, fmt.Sprintf("%s=%s", key, value))
	}
	return strings.Join(out, ",")
}
