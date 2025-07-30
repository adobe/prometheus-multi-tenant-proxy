package proxy

import (
	"context"
	"net/http"
	"time"

	"github.com/sirupsen/logrus"
)

// HealthChecker performs health checks on targets
type HealthChecker struct {
	loadBalancer *LoadBalancer
	client       *http.Client
	interval     time.Duration
	timeout      time.Duration
}

// NewHealthChecker creates a new health checker
func NewHealthChecker(lb *LoadBalancer) *HealthChecker {
	return &HealthChecker{
		loadBalancer: lb,
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
		interval: 30 * time.Second,
		timeout:  10 * time.Second,
	}
}

// Start begins health checking
func (hc *HealthChecker) Start() {
	ticker := time.NewTicker(hc.interval)
	defer ticker.Stop()
	
	for {
		select {
		case <-ticker.C:
			hc.checkAllTargets()
		}
	}
}

// checkAllTargets checks the health of all targets
func (hc *HealthChecker) checkAllTargets() {
	targets := hc.loadBalancer.GetAllTargets()
	
	for _, target := range targets {
		go hc.checkTarget(target.URL)
	}
}

// checkTarget checks the health of a single target
func (hc *HealthChecker) checkTarget(targetURL string) {
	ctx, cancel := context.WithTimeout(context.Background(), hc.timeout)
	defer cancel()
	
	// Create health check request
	req, err := http.NewRequestWithContext(ctx, "GET", targetURL+"/-/healthy", nil)
	if err != nil {
		logrus.Errorf("Failed to create health check request for %s: %v", targetURL, err)
		hc.loadBalancer.UpdateTargetHealth(targetURL, false)
		return
	}
	
	// Perform health check
	resp, err := hc.client.Do(req)
	if err != nil {
		logrus.Debugf("Health check failed for %s: %v", targetURL, err)
		hc.loadBalancer.UpdateTargetHealth(targetURL, false)
		return
	}
	defer resp.Body.Close()
	
	// Check response status
	healthy := resp.StatusCode == http.StatusOK
	hc.loadBalancer.UpdateTargetHealth(targetURL, healthy)
	
	if !healthy {
		logrus.Debugf("Health check failed for %s: status %d", targetURL, resp.StatusCode)
	}
} 