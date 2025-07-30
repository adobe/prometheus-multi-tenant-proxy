package proxy

import (
	"fmt"
	"math/rand"
	"sync"
	"time"

	"github.com/prometheus-multi-tenant-proxy/internal/discovery"
	"github.com/sirupsen/logrus"
)

// LoadBalancer handles load balancing across discovered targets
type LoadBalancer struct {
	discovery discovery.Discovery
	
	// Current targets
	mu      sync.RWMutex
	targets []discovery.Target
	
	// Round-robin counter
	counter int
	
	// Health checking
	healthChecker *HealthChecker
}

// NewLoadBalancer creates a new load balancer
func NewLoadBalancer(disc discovery.Discovery) *LoadBalancer {
	lb := &LoadBalancer{
		discovery: disc,
		targets:   []discovery.Target{},
	}
	
	// Initialize health checker
	lb.healthChecker = NewHealthChecker(lb)
	
	// Subscribe to target updates
	go lb.watchTargets()
	
	// Start health checking
	go lb.healthChecker.Start()
	
	return lb
}

// GetTarget returns a healthy target using round-robin load balancing
func (lb *LoadBalancer) GetTarget() (*discovery.Target, error) {
	lb.mu.RLock()
	defer lb.mu.RUnlock()
	
	healthyTargets := lb.getHealthyTargets()
	if len(healthyTargets) == 0 {
		return nil, fmt.Errorf("no healthy targets available")
	}
	
	// Round-robin selection
	target := &healthyTargets[lb.counter%len(healthyTargets)]
	lb.counter++
	
	return target, nil
}

// GetRandomTarget returns a random healthy target
func (lb *LoadBalancer) GetRandomTarget() (*discovery.Target, error) {
	lb.mu.RLock()
	defer lb.mu.RUnlock()
	
	healthyTargets := lb.getHealthyTargets()
	if len(healthyTargets) == 0 {
		return nil, fmt.Errorf("no healthy targets available")
	}
	
	// Random selection
	index := rand.Intn(len(healthyTargets))
	return &healthyTargets[index], nil
}

// GetAllTargets returns all current targets
func (lb *LoadBalancer) GetAllTargets() []discovery.Target {
	lb.mu.RLock()
	defer lb.mu.RUnlock()
	
	// Return a copy
	targets := make([]discovery.Target, len(lb.targets))
	copy(targets, lb.targets)
	return targets
}

// UpdateTargetHealth updates the health status of a target
func (lb *LoadBalancer) UpdateTargetHealth(targetURL string, healthy bool) {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	
	for i := range lb.targets {
		if lb.targets[i].URL == targetURL {
			lb.targets[i].Healthy = healthy
			lb.targets[i].LastSeen = time.Now()
			
			logrus.WithFields(logrus.Fields{
				"target":  targetURL,
				"healthy": healthy,
			}).Debug("Updated target health status")
			
			break
		}
	}
}

// getHealthyTargets returns only healthy targets (must be called with lock held)
func (lb *LoadBalancer) getHealthyTargets() []discovery.Target {
	var healthyTargets []discovery.Target
	
	for _, target := range lb.targets {
		if target.Healthy {
			healthyTargets = append(healthyTargets, target)
		}
	}
	
	return healthyTargets
}

// watchTargets watches for target updates from service discovery
func (lb *LoadBalancer) watchTargets() {
	updates := lb.discovery.Subscribe()
	
	for targets := range updates {
		lb.mu.Lock()
		lb.targets = targets
		lb.mu.Unlock()
		
		logrus.Infof("Updated load balancer with %d targets", len(targets))
	}
} 