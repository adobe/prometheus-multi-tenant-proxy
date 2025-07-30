package remote_write

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/common/model"
	"github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/prometheus-multi-tenant-proxy/api/v1alpha1"
	"github.com/prometheus-multi-tenant-proxy/internal/config"
	"github.com/prometheus-multi-tenant-proxy/internal/discovery"
)

// PrometheusResponse represents the response from a Prometheus query
type PrometheusResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Value  []interface{}     `json:"value"`
		} `json:"result"`
	} `json:"data"`
}

// Controller manages metric remote write for tenants
type Controller struct {
	client           client.Client
	config           config.RemoteWriteConfig
	serviceDiscovery discovery.Discovery

	// Active remote write jobs
	mu   sync.RWMutex
	jobs map[string]*RemoteWriteJob // namespace/name -> job

	// In-memory store for latest collected metrics per job
	collectedMetrics map[string][]Metric // namespace/name -> []Metric

	// Control channels
	stopCh chan struct{}
	doneCh chan struct{}
}

// RemoteWriteJob represents a remote write job for a tenant
type RemoteWriteJob struct {
	MetricAccess *v1alpha1.MetricAccess
	StopCh       chan struct{}
	
	// Metrics for monitoring
	LastRun       time.Time
	LastError     error
	MetricsCount  int64
	SuccessCount  int64
	FailureCount  int64
}

// NewController creates a new remote write controller
func NewController(client client.Client, cfg config.RemoteWriteConfig, serviceDiscovery discovery.Discovery) *Controller {
	return &Controller{
		client:           client,
		config:           cfg,
		serviceDiscovery: serviceDiscovery,
		jobs:             make(map[string]*RemoteWriteJob),
		collectedMetrics:  make(map[string][]Metric),
		stopCh:           make(chan struct{}),
		doneCh:           make(chan struct{}),
	}
}

// Start begins the remote write controller
func (c *Controller) Start(ctx context.Context) error {
	logrus.Info("Starting remote write controller")
	
	// Initial load of existing MetricAccess resources with remote write enabled
	if err := c.loadExistingRemoteWriteJobs(ctx); err != nil {
		logrus.Errorf("Failed to load existing remote write jobs: %v", err)
	}
	
	// Start reconciliation loop
	go c.reconcileLoop(ctx)
	
	return nil
}

// Stop gracefully shuts down the controller
func (c *Controller) Stop() {
	logrus.Info("Stopping remote write controller")
	close(c.stopCh)
	
	// Stop all active jobs
	c.stopAllJobs()
	
	// Wait for reconciliation loop to finish
	<-c.doneCh
}

// reconcileLoop periodically reconciles remote write jobs
func (c *Controller) reconcileLoop(ctx context.Context) {
	defer close(c.doneCh)
	
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.stopCh:
			return
		case <-ticker.C:
			if err := c.reconcileRemoteWriteJobs(ctx); err != nil {
				logrus.Errorf("Failed to reconcile remote write jobs: %v", err)
			}
		}
	}
}

// loadExistingRemoteWriteJobs loads existing MetricAccess resources with remote write enabled
func (c *Controller) loadExistingRemoteWriteJobs(ctx context.Context) error {
	var metricAccessList v1alpha1.MetricAccessList
	if err := c.client.List(ctx, &metricAccessList); err != nil {
		return fmt.Errorf("failed to list MetricAccess resources: %w", err)
	}
	
	for _, metricAccess := range metricAccessList.Items {
		if metricAccess.Spec.RemoteWrite != nil && metricAccess.Spec.RemoteWrite.Enabled {
			if err := c.createRemoteWriteJob(ctx, &metricAccess); err != nil {
				logrus.Errorf("Failed to create remote write job for %s/%s: %v", 
					metricAccess.Namespace, metricAccess.Name, err)
			}
		}
	}
	
	logrus.Infof("Loaded %d remote write jobs", len(c.jobs))
	return nil
}

// reconcileRemoteWriteJobs reconciles the current state with desired state
func (c *Controller) reconcileRemoteWriteJobs(ctx context.Context) error {
	var metricAccessList v1alpha1.MetricAccessList
	if err := c.client.List(ctx, &metricAccessList); err != nil {
		return fmt.Errorf("failed to list MetricAccess resources: %w", err)
	}
	
	c.mu.Lock()
	defer c.mu.Unlock()
	
	// Track which jobs should exist
	desiredJobs := make(map[string]bool)
	
	for _, metricAccess := range metricAccessList.Items {
		key := fmt.Sprintf("%s/%s", metricAccess.Namespace, metricAccess.Name)
		
		if metricAccess.Spec.RemoteWrite != nil && metricAccess.Spec.RemoteWrite.Enabled {
			desiredJobs[key] = true
			
			// Create job if it doesn't exist
			if _, exists := c.jobs[key]; !exists {
				if err := c.createRemoteWriteJobLocked(ctx, &metricAccess); err != nil {
					logrus.Errorf("Failed to create remote write job for %s: %v", key, err)
				}
			}
		}
	}
	
	// Stop jobs that should no longer exist
	for key, job := range c.jobs {
		if !desiredJobs[key] {
			c.stopRemoteWriteJobLocked(key, job)
		}
	}
	
	return nil
}

// createRemoteWriteJob creates a new remote write job
func (c *Controller) createRemoteWriteJob(ctx context.Context, metricAccess *v1alpha1.MetricAccess) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.createRemoteWriteJobLocked(ctx, metricAccess)
}

// createRemoteWriteJobLocked creates a new remote write job (must be called with lock held)
func (c *Controller) createRemoteWriteJobLocked(ctx context.Context, metricAccess *v1alpha1.MetricAccess) error {
	key := fmt.Sprintf("%s/%s", metricAccess.Namespace, metricAccess.Name)
	
	// Validate target type
	targetType := metricAccess.Spec.RemoteWrite.Target.Type
	if targetType != "prometheus" && targetType != "pushgateway" && targetType != "remote_write" {
		logrus.WithFields(logrus.Fields{
			"namespace":   metricAccess.Namespace,
			"name":        metricAccess.Name,
			"target_type": targetType,
		}).Error("DIAGNOSTIC: Unsupported remote write target type")
		return fmt.Errorf("unsupported remote write target type: %s", targetType)
	}
	
	// Don't assign static targets - we'll get them dynamically during collection
	logrus.WithFields(logrus.Fields{
		"namespace":        metricAccess.Namespace,
		"name":             metricAccess.Name,
		"metric_patterns":  metricAccess.Spec.Metrics,
	}).Info("DIAGNOSTIC: Creating remote write job (targets will be resolved dynamically)")
	
	job := &RemoteWriteJob{
		MetricAccess: metricAccess.DeepCopy(),
		// No static targets - get them fresh each time
		StopCh:       make(chan struct{}),
	}
	
	c.jobs[key] = job
	
	// Start the remote write job
	go c.runRemoteWriteJob(job)
	
	logrus.WithFields(logrus.Fields{
		"namespace":       metricAccess.Namespace,
		"name":            metricAccess.Name,
		"metrics_count":   len(metricAccess.Spec.Metrics),
	}).Info("Created remote write job with dynamic target resolution")
	
	return nil
}

// stopRemoteWriteJobLocked stops a remote write job (must be called with lock held)
func (c *Controller) stopRemoteWriteJobLocked(key string, job *RemoteWriteJob) {
	close(job.StopCh)
	delete(c.jobs, key)
	logrus.Infof("Stopped remote write job for %s", key)
}

// stopAllJobs stops all active remote write jobs
func (c *Controller) stopAllJobs() {
	c.mu.Lock()
	defer c.mu.Unlock()
	
	for key, job := range c.jobs {
		c.stopRemoteWriteJobLocked(key, job)
	}
}

// runRemoteWriteJob runs a remote write job until stopped
func (c *Controller) runRemoteWriteJob(job *RemoteWriteJob) {
	logrus.WithFields(logrus.Fields{
		"namespace": job.MetricAccess.Namespace,
		"name":     job.MetricAccess.Name,
	}).Info("Starting remote write job")

	// Get collection interval
	interval := 30 * time.Second
	if job.MetricAccess.Spec.RemoteWrite.Interval.Duration > 0 {
		interval = job.MetricAccess.Spec.RemoteWrite.Interval.Duration
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Initial collection
	c.collectMetrics(job)

	for {
		select {
		case <-job.StopCh:
			logrus.WithFields(logrus.Fields{
				"namespace": job.MetricAccess.Namespace,
				"name":     job.MetricAccess.Name,
			}).Info("Stopping remote write job")
			return
		case <-ticker.C:
			c.collectMetrics(job)
		}
	}
}

// collectMetrics collects metrics from all available healthy targets for a job
func (c *Controller) collectMetrics(job *RemoteWriteJob) {
	logrus.WithFields(logrus.Fields{
		"namespace": job.MetricAccess.Namespace,
		"name":     job.MetricAccess.Name,
	}).Debug("Collecting metrics for remote write job")

	// Get fresh targets from service discovery each time
	allTargets := c.serviceDiscovery.GetTargets()
	
	// Filter for healthy targets only
	var healthyTargets []discovery.Target
	for _, target := range allTargets {
		if target.Healthy {
			healthyTargets = append(healthyTargets, target)
		}
	}
	
	logrus.WithFields(logrus.Fields{
		"namespace":        job.MetricAccess.Namespace,
		"name":             job.MetricAccess.Name,
		"all_targets":      len(allTargets),
		"healthy_targets":  len(healthyTargets),
	}).Debug("DIAGNOSTIC: Got fresh targets for metric collection")
	
	// If no healthy targets, log and return
	if len(healthyTargets) == 0 {
		logrus.WithFields(logrus.Fields{
			"namespace": job.MetricAccess.Namespace,
			"name":      job.MetricAccess.Name,
		}).Warn("DIAGNOSTIC: No healthy targets available for metric collection")
		
		// Store empty metrics
		key := fmt.Sprintf("%s/%s", job.MetricAccess.Namespace, job.MetricAccess.Name)
		c.mu.Lock()
		c.collectedMetrics[key] = []Metric{}
		c.mu.Unlock()
		
		logrus.WithFields(logrus.Fields{
			"namespace": job.MetricAccess.Namespace,
			"name":     job.MetricAccess.Name,
			"count":    0,
		}).Debug("Stored collected metrics")
		
		job.LastRun = time.Now()
		c.updateJobStatus(job)
		return
	}

	var allMetrics []Metric
	for i, target := range healthyTargets {
		logrus.WithFields(logrus.Fields{
			"target_index": i,
			"target_url":   target.URL,
			"namespace":    job.MetricAccess.Namespace,
			"name":         job.MetricAccess.Name,
		}).Debug("DIAGNOSTIC: Collecting from target")
		
		metrics, err := c.collectFromTarget(job, target)
		if err != nil {
			logrus.WithError(err).Errorf("Failed to collect metrics from target %s", target.URL)
			job.LastError = err
			job.FailureCount++
			continue
		}
		allMetrics = append(allMetrics, metrics...)
	}

	// Store the latest collected metrics for this job
	key := fmt.Sprintf("%s/%s", job.MetricAccess.Namespace, job.MetricAccess.Name)
	c.mu.Lock()
	c.collectedMetrics[key] = allMetrics
	c.mu.Unlock()

	logrus.WithFields(logrus.Fields{
		"namespace": job.MetricAccess.Namespace,
		"name":     job.MetricAccess.Name,
		"count":    len(allMetrics),
		"targets_used": len(healthyTargets),
	}).Info("DIAGNOSTIC: Stored collected metrics from all targets")

	if len(allMetrics) > 0 {
		if err := c.sendMetrics(job, allMetrics); err != nil {
			logrus.WithError(err).Error("Failed to send metrics")
			job.LastError = err
			job.FailureCount++
			return
		}
		job.SuccessCount++
		job.MetricsCount += int64(len(allMetrics))
	}

	job.LastRun = time.Now()
	c.updateJobStatus(job)
}

// collectFromTarget collects metrics from a single target
func (c *Controller) collectFromTarget(job *RemoteWriteJob, target discovery.Target) ([]Metric, error) {
	// Log target details for metric collection
	logrus.WithFields(logrus.Fields{
		"target_url": target.URL,
		"job":        fmt.Sprintf("%s/%s", job.MetricAccess.Namespace, job.MetricAccess.Name),
		"healthy":    target.Healthy,
		"labels":     fmt.Sprintf("%v", target.Labels),
	}).Debug("DIAGNOSTIC: Starting metric collection from target") 

	// Skip unhealthy targets
	if !target.Healthy {
		logrus.WithFields(logrus.Fields{
			"target_url": target.URL,
		}).Warn("DIAGNOSTIC: Skipping unhealthy target")
		return nil, fmt.Errorf("target is unhealthy")
	}

	var metrics []Metric
	client := &http.Client{
		Timeout: 30 * time.Second,
	}

	// Log all metric patterns we're going to query
	logrus.WithFields(logrus.Fields{
		"target_url":     target.URL,
		"metric_count":   len(job.MetricAccess.Spec.Metrics),
		"metric_patterns": job.MetricAccess.Spec.Metrics,
	}).Debug("DIAGNOSTIC: Metric patterns to query")

	// Query each metric pattern
	for _, pattern := range job.MetricAccess.Spec.Metrics {
		logrus.WithFields(logrus.Fields{
			"pattern":    pattern,
			"target_url": target.URL,
		}).Debug("DIAGNOSTIC: Processing metric pattern") 

		// Handle label selector patterns
		var queryPattern string
		if strings.HasPrefix(pattern, "{") && strings.HasSuffix(pattern, "}") {
			// This is already a PromQL selector pattern
			queryPattern = pattern
		} else {
			// Simple metric name pattern
			// First try with just the metric name
			queryPattern = pattern
			
			// Check if we have any label selectors to apply
			if len(job.MetricAccess.Spec.LabelSelectors) > 0 {
				// Add label selectors
				labels := []string{}
				for k, v := range job.MetricAccess.Spec.LabelSelectors {
					labels = append(labels, fmt.Sprintf("%s=\"%s\"", k, v))
				}
				queryPattern = fmt.Sprintf("{__name__=\"%s\",%s}", pattern, strings.Join(labels, ","))
			}
		}

		// Try the regular query first
		logrus.WithFields(logrus.Fields{
			"query_pattern": queryPattern,
			"target_url":    target.URL,
		}).Debug("DIAGNOSTIC: Querying with standard pattern")
		
		metricResults, err := c.queryPromQL(client, target.URL, queryPattern)
		if err != nil {
			logrus.WithFields(logrus.Fields{
				"error":        err,
				"pattern":      queryPattern,
				"target_url":   target.URL,
				"error_type":   fmt.Sprintf("%T", err),
				"error_details": err.Error(),
			}).Error("DIAGNOSTIC: Failed to query metrics")
			continue
		}

		// If we got metrics, add them to the result
		if len(metricResults) > 0 {
			metrics = append(metrics, metricResults...)
			logrus.WithFields(logrus.Fields{
				"count":        len(metricResults),
				"pattern":      queryPattern,
				"target_url":   target.URL,
				"first_metric": metricResults[0].Name,
			}).Info("DIAGNOSTIC: Found metrics with standard pattern")
			continue
		} else {
			logrus.WithFields(logrus.Fields{
				"pattern":    queryPattern,
				"target_url": target.URL,
			}).Debug("DIAGNOSTIC: No metrics found with standard pattern")
		}

		// If no metrics found and this is a simple metric name, try different strategies
		if !strings.HasPrefix(pattern, "{") && strings.Count(pattern, "\"") == 0 {
			// For node metrics (starting with "node_"), try with job="node-exporter"
			if strings.HasPrefix(pattern, "node_") {
				nodeQueryPattern := fmt.Sprintf("{__name__=\"%s\",job=\"node-exporter\"}", pattern)
				logrus.WithFields(logrus.Fields{
					"query_pattern": nodeQueryPattern,
					"target_url":    target.URL,
				}).Debug("DIAGNOSTIC: Trying node-exporter specific pattern")
				
				nodeMetrics, err := c.queryPromQL(client, target.URL, nodeQueryPattern)
				if err != nil {
					logrus.WithFields(logrus.Fields{
						"error":        err,
						"pattern":      nodeQueryPattern,
						"target_url":   target.URL,
						"error_details": err.Error(),
					}).Error("DIAGNOSTIC: Failed to query with node-exporter pattern")
				} else if len(nodeMetrics) > 0 {
					metrics = append(metrics, nodeMetrics...)
					logrus.WithFields(logrus.Fields{
						"count":        len(nodeMetrics),
						"pattern":      nodeQueryPattern,
						"target_url":   target.URL,
						"first_metric": nodeMetrics[0].Name,
					}).Info("DIAGNOSTIC: Found metrics with node-exporter pattern")
					continue
				} else {
					logrus.WithFields(logrus.Fields{
						"pattern":    nodeQueryPattern,
						"target_url": target.URL,
					}).Debug("DIAGNOSTIC: No metrics found with node-exporter pattern")
				}
			}
			
			// Try with no label selectors as last resort for any metric type
			flexibleQueryPattern := fmt.Sprintf("{__name__=\"%s\"}", pattern)
			logrus.WithFields(logrus.Fields{
				"query_pattern": flexibleQueryPattern,
				"target_url":    target.URL,
			}).Debug("DIAGNOSTIC: Trying generic pattern without selectors")
			
			flexibleMetrics, err := c.queryPromQL(client, target.URL, flexibleQueryPattern)
			if err != nil {
				logrus.WithFields(logrus.Fields{
					"error":        err,
					"pattern":      flexibleQueryPattern,
					"target_url":   target.URL,
					"error_details": err.Error(),
				}).Error("DIAGNOSTIC: Failed to query with generic pattern")
			} else if len(flexibleMetrics) > 0 {
				metrics = append(metrics, flexibleMetrics...)
				logrus.WithFields(logrus.Fields{
					"count":        len(flexibleMetrics),
					"pattern":      flexibleQueryPattern,
					"target_url":   target.URL,
					"first_metric": flexibleMetrics[0].Name,
				}).Info("DIAGNOSTIC: Found metrics with generic pattern")
			} else {
				logrus.WithFields(logrus.Fields{
					"pattern":    flexibleQueryPattern,
					"target_url": target.URL,
				}).Warn("DIAGNOSTIC: No metrics found for pattern using any query strategy")
			}
		}
	}

	logrus.WithFields(logrus.Fields{
		"count":      len(metrics),
		"target_url": target.URL,
		"job":        fmt.Sprintf("%s/%s", job.MetricAccess.Namespace, job.MetricAccess.Name),
	}).Info("DIAGNOSTIC: Completed metric collection from target") 

	return metrics, nil
}

// queryPromQL queries the Prometheus instance for the given PromQL query
func (c *Controller) queryPromQL(client *http.Client, targetURL, query string) ([]Metric, error) {
	// Query the target directly instead of going through the proxy
	// This ensures we can find metrics that exist on specific targets
	queryURL := fmt.Sprintf("%s/api/v1/query?query=%s", targetURL, url.QueryEscape(query))
	
	logrus.WithFields(logrus.Fields{
		"query_url": queryURL,
		"query": query,
		"target_url": targetURL,
		"using_proxy": false,
	}).Debug("DIAGNOSTIC: Making Prometheus query directly to target")
	
	// Create request
	req, err := http.NewRequest("GET", queryURL, nil)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"error": err,
			"url":   queryURL,
		}).Error("DIAGNOSTIC: Failed to create HTTP request")
		return nil, err
	}
	
	// Add user agent for identification
	req.Header.Set("User-Agent", "prometheus-multi-tenant-proxy/remote-write-controller")
	
	logrus.WithFields(logrus.Fields{
		"headers": req.Header,
		"url":     queryURL,
	}).Debug("DIAGNOSTIC: Request headers set for direct target query")
	
	// Make the request
	resp, err := client.Do(req)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"error": err,
			"url":   queryURL,
			"error_type": fmt.Sprintf("%T", err),
			"error_details": err.Error(),
		}).Error("DIAGNOSTIC: HTTP request failed")
		return nil, err
	}
	defer resp.Body.Close()
	
	// Parse the response
	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		logrus.WithFields(logrus.Fields{
			"status_code": resp.StatusCode,
			"url":         queryURL,
			"response":    string(bodyBytes),
		}).Error("DIAGNOSTIC: Prometheus query returned non-200 status code")
		return nil, fmt.Errorf("prometheus query failed with status %d: %s", resp.StatusCode, string(bodyBytes))
	}
	
	// Read the response body
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"error": err,
			"url":   queryURL,
		}).Error("DIAGNOSTIC: Failed to read response body")
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}
	
	// Log the raw response for debugging
	logrus.WithFields(logrus.Fields{
		"url":      queryURL,
		"response_size": len(bodyBytes),
		"response_preview": string(bodyBytes[:min(len(bodyBytes), 500)]), // Log first 500 chars max
	}).Debug("DIAGNOSTIC: Received Prometheus response")
	
	// Parse the response
	var promResp PrometheusResponse
	if err := json.Unmarshal(bodyBytes, &promResp); err != nil {
		logrus.WithFields(logrus.Fields{
			"error": err,
			"url":   queryURL,
			"body":  string(bodyBytes),
		}).Error("DIAGNOSTIC: Failed to parse Prometheus response")
		return nil, fmt.Errorf("failed to parse prometheus response: %w", err)
	}
	
	// Check the response status
	if promResp.Status != "success" {
		logrus.WithFields(logrus.Fields{
			"url":    queryURL,
			"status": promResp.Status,
			"body":   string(bodyBytes),
		}).Error("DIAGNOSTIC: Prometheus returned non-success status")
		return nil, fmt.Errorf("prometheus returned non-success status: %s", promResp.Status)
	}
	
	// Convert the results to metrics
	var metrics []Metric
	for _, result := range promResp.Data.Result {
		// Extract the metric name
		metricName := result.Metric["__name__"]
		
		// Extract labels
		labels := model.LabelSet{}
		for name, value := range result.Metric {
			if name != "__name__" {
				labels[model.LabelName(name)] = model.LabelValue(value)
			}
		}
		
		// Extract value and timestamp
		if len(result.Value) < 2 {
			logrus.WithFields(logrus.Fields{
				"url":    queryURL,
				"metric": metricName,
			}).Error("DIAGNOSTIC: Invalid value format in Prometheus response")
			continue
		}
		
		// Parse timestamp
		var timestamp time.Time
		if ts, ok := result.Value[0].(float64); ok {
			timestamp = time.Unix(int64(ts), 0)
		} else {
			logrus.WithFields(logrus.Fields{
				"url":    queryURL,
				"metric": metricName,
				"value":  fmt.Sprintf("%v", result.Value[0]),
			}).Error("DIAGNOSTIC: Invalid timestamp format in Prometheus response")
			timestamp = time.Now()
		}
		
		// Parse value
		var value float64
		switch v := result.Value[1].(type) {
		case string:
			parsedValue, err := strconv.ParseFloat(v, 64)
		if err != nil {
				logrus.WithFields(logrus.Fields{
					"url":    queryURL,
					"metric": metricName,
					"value":  v,
					"error":  err,
				}).Error("DIAGNOSTIC: Failed to parse metric value")
				continue
			}
			value = parsedValue
		case float64:
			value = v
		default:
			logrus.WithFields(logrus.Fields{
				"url":    queryURL,
				"metric": metricName,
				"value":  fmt.Sprintf("%v", result.Value[1]),
				"type":   fmt.Sprintf("%T", result.Value[1]),
			}).Error("DIAGNOSTIC: Unexpected value type in Prometheus response")
			continue
		}
		
		// Create the metric
		metric := Metric{
			Name:      metricName,
			Labels:    labels,
			Value:     value,
			Timestamp: timestamp,
		}
		
		metrics = append(metrics, metric)
	}
	
	logrus.WithFields(logrus.Fields{
		"url":           queryURL,
		"metrics_count": len(metrics),
		"query":         query,
	}).Debug("DIAGNOSTIC: Processed Prometheus query response")
	
	return metrics, nil
}

// Helper function to get the minimum of two integers
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// sendMetrics sends collected metrics to the remote write target
func (c *Controller) sendMetrics(job *RemoteWriteJob, metrics []Metric) error {
	// Create appropriate collector based on target type
	var collector Collector
	switch job.MetricAccess.Spec.RemoteWrite.Target.Type {
	case "prometheus":
		collector = NewPrometheusCollector(c.client)
	case "pushgateway":
		collector = NewPushgatewayCollector(c.client)
	case "remote_write":
		collector = NewRemoteWriteCollector(c.client)
	default:
		return fmt.Errorf("unsupported remote write target type: %s", 
			job.MetricAccess.Spec.RemoteWrite.Target.Type)
	}

	// Create context with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Send metrics using the collector
	if err := collector.Send(ctx, job.MetricAccess, metrics); err != nil {
		return fmt.Errorf("failed to send metrics: %w", err)
	}

	return nil
}

// updateJobStatus updates the status of a remote write job
func (c *Controller) updateJobStatus(job *RemoteWriteJob) {
	// Update the MetricAccess status
	status := &v1alpha1.RemoteWriteStatus{
		Active:           true,
		LastCollection:   metav1.NewTime(job.LastRun),
		MetricsCollected: int32(job.MetricsCount),
	}
	
	if job.LastError != nil {
		status.LastError = job.LastError.Error()
	}
	
	job.MetricAccess.Status.RemoteWrite = status
	
	// TODO: Update the status in Kubernetes
	// This would require using the client to patch the MetricAccess resource
}

// GetActiveJobs returns information about active remote write jobs
func (c *Controller) GetActiveJobs() map[string]*RemoteWriteJob {
	c.mu.RLock()
	defer c.mu.RUnlock()
	
	jobs := make(map[string]*RemoteWriteJob)
	for k, v := range c.jobs {
		jobs[k] = v
	}
	
	return jobs
}

// GetAllCollectedMetrics returns all collected metrics for all jobs
func (c *Controller) GetAllCollectedMetrics() map[string][]Metric {
	c.mu.RLock()
	defer c.mu.RUnlock()
	
	// Log diagnostic information if no metrics are collected
	if len(c.collectedMetrics) == 0 {
		logrus.WithFields(logrus.Fields{
			"active_jobs": len(c.jobs),
		}).Warning("No collected metrics found in remote write controller")
		
		// Log information about active jobs
		if len(c.jobs) > 0 {
			for key, job := range c.jobs {
				logrus.WithFields(logrus.Fields{
					"job_key":       key,
					"metrics_count": job.MetricsCount,
					"last_run":      job.LastRun,
					"has_error":     job.LastError != nil,
				}).Info("Active job status")
				
				if job.LastError != nil {
					logrus.WithFields(logrus.Fields{
						"job_key": key,
						"error":   job.LastError.Error(),
					}).Error("Job encountered an error")
				}
			}
		} else {
			logrus.Warning("No active remote write jobs found")
		}
	}
	
	// Make a copy to avoid concurrent map access
	metrics := make(map[string][]Metric)
	for k, v := range c.collectedMetrics {
		metrics[k] = append([]Metric{}, v...)
	}
	
	return metrics
} 