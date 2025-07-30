package discovery

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus-multi-tenant-proxy/internal/config"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
)

// Target represents a discovered Prometheus target
type Target struct {
	// URL of the Prometheus instance
	URL string
	
	// Labels associated with this target
	Labels map[string]string
	
	// Health status of the target
	Healthy bool
	
	// Last time this target was seen
	LastSeen time.Time
}

// Discovery interface defines the contract for service discovery
type Discovery interface {
	// Start begins the discovery process
	Start(ctx context.Context) error
	
	// GetTargets returns the current list of discovered targets
	GetTargets() []Target
	
	// Subscribe returns a channel that receives target updates
	Subscribe() <-chan []Target
}

// kubernetesDiscovery implements Discovery for Kubernetes environments
type kubernetesDiscovery struct {
	client    kubernetes.Interface
	config    config.KubernetesDiscoveryConfig
	refresh   time.Duration
	
	// Current targets
	mu      sync.RWMutex
	targets []Target
	
	// Subscribers for target updates
	subscribers []chan []Target
	subMu       sync.RWMutex
}

// NewKubernetesDiscovery creates a new Kubernetes service discovery instance
func NewKubernetesDiscovery(client kubernetes.Interface, cfg config.DiscoveryConfig) (Discovery, error) {
	return &kubernetesDiscovery{
		client:      client,
		config:      cfg.Kubernetes,
		refresh:     cfg.RefreshInterval,
		targets:     make([]Target, 0),
		subscribers: make([]chan []Target, 0),
	}, nil
}

// Start begins the discovery process
func (kd *kubernetesDiscovery) Start(ctx context.Context) error {
	logrus.Info("Starting Kubernetes service discovery")
	
	// Initial discovery
	if err := kd.discover(ctx); err != nil {
		logrus.Errorf("Initial discovery failed: %v", err)
	}
	
	// Start periodic discovery
	ticker := time.NewTicker(kd.refresh)
	defer ticker.Stop()
	
	for {
		select {
		case <-ctx.Done():
			logrus.Info("Stopping Kubernetes service discovery")
			return ctx.Err()
		case <-ticker.C:
			if err := kd.discover(ctx); err != nil {
				logrus.Errorf("Discovery refresh failed: %v", err)
			}
		}
	}
}

// GetTargets returns the current list of discovered targets
func (kd *kubernetesDiscovery) GetTargets() []Target {
	kd.mu.RLock()
	defer kd.mu.RUnlock()
	
	// Return a copy to avoid race conditions
	targets := make([]Target, len(kd.targets))
	copy(targets, kd.targets)
	return targets
}

// Subscribe returns a channel that receives target updates
func (kd *kubernetesDiscovery) Subscribe() <-chan []Target {
	kd.subMu.Lock()
	defer kd.subMu.Unlock()
	
	ch := make(chan []Target, 1)
	kd.subscribers = append(kd.subscribers, ch)
	
	// Send current targets immediately
	go func() {
		ch <- kd.GetTargets()
	}()
	
	return ch
}

// discover performs the actual service discovery
func (kd *kubernetesDiscovery) discover(ctx context.Context) error {
	var allTargets []Target
	
	for _, resourceType := range kd.config.ResourceTypes {
		targets, err := kd.discoverResourceType(ctx, resourceType)
		if err != nil {
			logrus.Errorf("Failed to discover %s resources: %v", resourceType, err)
			continue
		}
		allTargets = append(allTargets, targets...)
	}
	
	// Update targets
	kd.updateTargets(allTargets)
	
	logrus.Debugf("Discovered %d targets", len(allTargets))
	return nil
}

// discoverResourceType discovers targets for a specific resource type
func (kd *kubernetesDiscovery) discoverResourceType(ctx context.Context, resourceType string) ([]Target, error) {
	switch resourceType {
	case "Service":
		return kd.discoverServices(ctx)
	case "Pod":
		return kd.discoverPods(ctx)
	case "Endpoints":
		return kd.discoverEndpoints(ctx)
	default:
		return nil, fmt.Errorf("unsupported resource type: %s", resourceType)
	}
}

// discoverServices discovers Prometheus targets from Kubernetes services
func (kd *kubernetesDiscovery) discoverServices(ctx context.Context) ([]Target, error) {
	var targets []Target
	
	namespaces := kd.getNamespaces()
	if len(namespaces) == 0 {
		namespaces = []string{metav1.NamespaceAll}
	}
	
	for _, namespace := range namespaces {
		services, err := kd.client.CoreV1().Services(namespace).List(ctx, metav1.ListOptions{
			LabelSelector: kd.buildLabelSelector(),
		})
		if err != nil {
			return nil, fmt.Errorf("failed to list services in namespace %s: %w", namespace, err)
		}
		
		for _, service := range services.Items {
			if kd.matchesAnnotations(&service.ObjectMeta) {
				serviceTargets := kd.serviceToTargets(&service)
				targets = append(targets, serviceTargets...)
			}
		}
	}
	
	return targets, nil
}

// discoverPods discovers Prometheus targets from Kubernetes pods
func (kd *kubernetesDiscovery) discoverPods(ctx context.Context) ([]Target, error) {
	var targets []Target
	
	namespaces := kd.getNamespaces()
	if len(namespaces) == 0 {
		namespaces = []string{metav1.NamespaceAll}
	}
	
	for _, namespace := range namespaces {
		pods, err := kd.client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
			LabelSelector: kd.buildLabelSelector(),
		})
		if err != nil {
			return nil, fmt.Errorf("failed to list pods in namespace %s: %w", namespace, err)
		}
		
		for _, pod := range pods.Items {
			if pod.Status.Phase == corev1.PodRunning && kd.matchesAnnotations(&pod.ObjectMeta) {
				podTargets := kd.podToTargets(&pod)
				targets = append(targets, podTargets...)
			}
		}
	}
	
	return targets, nil
}

// discoverEndpoints discovers Prometheus targets from Kubernetes endpoints
func (kd *kubernetesDiscovery) discoverEndpoints(ctx context.Context) ([]Target, error) {
	var targets []Target
	
	namespaces := kd.getNamespaces()
	if len(namespaces) == 0 {
		namespaces = []string{metav1.NamespaceAll}
	}
	
	for _, namespace := range namespaces {
		endpoints, err := kd.client.CoreV1().Endpoints(namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to list endpoints in namespace %s: %w", namespace, err)
		}
		
		for _, endpoint := range endpoints.Items {
			if kd.matchesAnnotations(&endpoint.ObjectMeta) {
				endpointTargets := kd.endpointsToTargets(&endpoint)
				targets = append(targets, endpointTargets...)
			}
		}
	}
	
	return targets, nil
}

// serviceToTargets converts a Kubernetes service to Prometheus targets
func (kd *kubernetesDiscovery) serviceToTargets(service *corev1.Service) []Target {
	var targets []Target
	
	port := kd.findServicePort(service)
	if port == nil {
		return targets
	}
	
	// Create target URL
	targetURL := fmt.Sprintf("http://%s.%s.svc.cluster.local:%d",
		service.Name, service.Namespace, port.Port)
	
	target := Target{
		URL:      targetURL,
		Labels:   kd.buildTargetLabels(service.Labels, service.Annotations, service.Namespace),
		Healthy:  true,
		LastSeen: time.Now(),
	}
	
	targets = append(targets, target)
	return targets
}

// podToTargets converts a Kubernetes pod to Prometheus targets
func (kd *kubernetesDiscovery) podToTargets(pod *corev1.Pod) []Target {
	var targets []Target
	
	if pod.Status.PodIP == "" {
		return targets
	}
	
	port := kd.findPodPort(pod)
	if port == 0 {
		return targets
	}
	
	// Create target URL
	var targetURL string
	
	// Try direct IP access first - this is most reliable
		targetURL = fmt.Sprintf("http://%s:%d", pod.Status.PodIP, port)
	
	target := Target{
		URL:      targetURL,
		Labels:   kd.buildTargetLabels(pod.Labels, pod.Annotations, pod.Namespace),
		Healthy:  true,
		LastSeen: time.Now(),
	}
	
	targets = append(targets, target)
	return targets
}

// endpointsToTargets converts Kubernetes endpoints to Prometheus targets
func (kd *kubernetesDiscovery) endpointsToTargets(endpoints *corev1.Endpoints) []Target {
	var targets []Target
	
	for _, subset := range endpoints.Subsets {
		port := kd.findEndpointPort(&subset)
		if port == 0 {
			continue
		}
		
		for _, address := range subset.Addresses {
			targetURL := fmt.Sprintf("http://%s:%d", address.IP, port)
			
			target := Target{
				URL:      targetURL,
				Labels:   kd.buildTargetLabels(endpoints.Labels, endpoints.Annotations, endpoints.Namespace),
				Healthy:  true,
				LastSeen: time.Now(),
			}
			
			targets = append(targets, target)
		}
	}
	
	return targets
}

// Helper functions

func (kd *kubernetesDiscovery) getNamespaces() []string {
	if len(kd.config.Namespaces) > 0 {
		return kd.config.Namespaces
	}
	return []string{}
}

func (kd *kubernetesDiscovery) buildLabelSelector() string {
	selector := labels.Set(kd.config.LabelSelectors)
	return selector.String()
}

func (kd *kubernetesDiscovery) matchesAnnotations(meta *metav1.ObjectMeta) bool {
	if len(kd.config.AnnotationSelectors) == 0 {
		return true
	}
	
	for key, value := range kd.config.AnnotationSelectors {
		if meta.Annotations[key] != value {
			return false
		}
	}
	
	return true
}

func (kd *kubernetesDiscovery) findServicePort(service *corev1.Service) *corev1.ServicePort {
	for _, port := range service.Spec.Ports {
		if port.Name == kd.config.Port || strconv.Itoa(int(port.Port)) == kd.config.Port {
			return &port
		}
	}
	
	// If no specific port found, return the first port
	if len(service.Spec.Ports) > 0 {
		return &service.Spec.Ports[0]
	}
	
	return nil
}

func (kd *kubernetesDiscovery) findPodPort(pod *corev1.Pod) int32 {
	for _, container := range pod.Spec.Containers {
		for _, port := range container.Ports {
			if port.Name == kd.config.Port || strconv.Itoa(int(port.ContainerPort)) == kd.config.Port {
				return port.ContainerPort
			}
		}
	}
	
	// Default to 9090 if no specific port found
	if portNum, err := strconv.Atoi(kd.config.Port); err == nil {
		return int32(portNum)
	}
	
	return 9090
}

func (kd *kubernetesDiscovery) findEndpointPort(subset *corev1.EndpointSubset) int32 {
	for _, port := range subset.Ports {
		if port.Name == kd.config.Port || strconv.Itoa(int(port.Port)) == kd.config.Port {
			return port.Port
		}
	}
	
	// Default to 9090 if no specific port found
	if portNum, err := strconv.Atoi(kd.config.Port); err == nil {
		return int32(portNum)
	}
	
	return 9090
}

func (kd *kubernetesDiscovery) buildTargetLabels(labels, annotations map[string]string, namespace string) map[string]string {
	targetLabels := make(map[string]string)
	
	// Add original labels
	for k, v := range labels {
		targetLabels[k] = v
	}
	
	// Add namespace
	targetLabels["__meta_kubernetes_namespace"] = namespace
	
	// Add selected annotations as labels
	for k, v := range annotations {
		targetLabels["__meta_kubernetes_annotation_"+k] = v
	}
	
	return targetLabels
}

func (kd *kubernetesDiscovery) updateTargets(newTargets []Target) {
	kd.mu.Lock()
	kd.targets = newTargets
	kd.mu.Unlock()
	
	// Notify subscribers
	kd.notifySubscribers(newTargets)
}

func (kd *kubernetesDiscovery) notifySubscribers(targets []Target) {
	kd.subMu.RLock()
	defer kd.subMu.RUnlock()
	
	for _, ch := range kd.subscribers {
		select {
		case ch <- targets:
		default:
			// Channel is full, skip this update
		}
	}
} 