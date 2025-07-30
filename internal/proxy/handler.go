package proxy

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"github.com/prometheus-multi-tenant-proxy/internal/config"
	"github.com/prometheus-multi-tenant-proxy/internal/discovery"
	"github.com/prometheus-multi-tenant-proxy/internal/tenant"
	"github.com/prometheus-multi-tenant-proxy/internal/remote_write"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"
)

// Handler represents the main proxy handler
type Handler struct {
	config           *config.Config
	serviceDiscovery discovery.Discovery
	tenantManager    tenant.Manager
	
	// Inject remote write controller for collected metrics
	remoteWriteController interface{ GetAllCollectedMetrics() map[string][]remote_write.Metric }
	
	// Load balancer for backend targets
	loadBalancer *LoadBalancer
	
	// Metrics
	requestsTotal    *prometheus.CounterVec
	requestDuration  *prometheus.HistogramVec
	backendRequests  *prometheus.CounterVec
}

// NewHandler creates a new proxy handler
func NewHandler(cfg *config.Config, serviceDiscovery discovery.Discovery, tenantManager tenant.Manager, remoteWriteController interface{ GetAllCollectedMetrics() map[string][]remote_write.Metric }) (*Handler, error) {
	h := &Handler{
		config:           cfg,
		serviceDiscovery: serviceDiscovery,
		tenantManager:    tenantManager,
		remoteWriteController: remoteWriteController,
	}
	
	// Initialize load balancer
	h.loadBalancer = NewLoadBalancer(serviceDiscovery)
	
	// Initialize metrics if enabled
	if cfg.Proxy.EnableMetrics {
		h.initMetrics()
	}
	
	return h, nil
}

// ServeHTTP implements the http.Handler interface
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	
	// Log all incoming requests at DEBUG level for normal operations
	logrus.WithFields(logrus.Fields{
		"method":     r.Method,
		"path":       r.URL.Path,
		"query":      r.URL.RawQuery,
		"remote_ip":  r.RemoteAddr,
		"user_agent": r.UserAgent(),
		"headers":    fmt.Sprintf("%v", r.Header),
	}).Debug("DIAGNOSTIC: Received HTTP request")
	
	// Create router
	router := mux.NewRouter()
	
	// Health check endpoint
	router.HandleFunc("/health", h.handleHealth).Methods("GET")
	
	// Metrics endpoint (if enabled)
	if h.config.Proxy.EnableMetrics {
		router.Handle("/metrics", promhttp.Handler()).Methods("GET")
	}
	
	// Debug endpoints
	router.HandleFunc("/debug/targets", h.handleDebugTargets).Methods("GET")
	router.HandleFunc("/debug/tenants", h.handleDebugTenants).Methods("GET")
	
	// Collected metrics endpoint
	router.HandleFunc("/collected-metrics", h.handleCollectedMetrics).Methods("GET")
	
	// Prometheus API endpoints
	router.PathPrefix("/api/v1/").HandlerFunc(h.handlePrometheusAPI)
	router.PathPrefix("/").HandlerFunc(h.handlePrometheusAPI)
	
	// Serve the request
	router.ServeHTTP(w, r)
	
	// Log request completion
	duration := time.Since(start).Seconds()
	logrus.WithFields(logrus.Fields{
		"method":   r.Method,
		"path":     r.URL.Path,
		"duration": duration,
	}).Debug("DIAGNOSTIC: Completed HTTP request")
	
	// Record metrics
	if h.config.Proxy.EnableMetrics {
		h.requestDuration.WithLabelValues(r.Method, r.URL.Path).Observe(duration)
		h.requestsTotal.WithLabelValues(r.Method, r.URL.Path, fmt.Sprintf("%d", 200)).Inc()
	}
}

// handleHealth handles health check requests
func (h *Handler) handleHealth(w http.ResponseWriter, r *http.Request) {
	targets := h.serviceDiscovery.GetTargets()
	healthyTargets := 0
	
	for _, target := range targets {
		if target.Healthy {
			healthyTargets++
		}
	}
	
	status := map[string]interface{}{
		"status":          "healthy",
		"total_targets":   len(targets),
		"healthy_targets": healthyTargets,
		"tenants":         len(h.tenantManager.GetAllTenants()),
	}
	
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	
	// Simple JSON response
	fmt.Fprintf(w, `{"status":"%s","total_targets":%d,"healthy_targets":%d,"tenants":%d}`,
		status["status"], status["total_targets"], status["healthy_targets"], status["tenants"])
}

// handleDebugTargets handles debug requests for discovered targets
func (h *Handler) handleDebugTargets(w http.ResponseWriter, r *http.Request) {
	targets := h.serviceDiscovery.GetTargets()
	
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	
	fmt.Fprintf(w, `{"targets":[`)
	for i, target := range targets {
		if i > 0 {
			fmt.Fprintf(w, ",")
		}
		fmt.Fprintf(w, `{"url":"%s","healthy":%t,"last_seen":"%s"}`,
			target.URL, target.Healthy, target.LastSeen.Format(time.RFC3339))
	}
	fmt.Fprintf(w, `]}`)
}

// handleDebugTenants handles debug requests for tenant information
func (h *Handler) handleDebugTenants(w http.ResponseWriter, r *http.Request) {
	tenants := h.tenantManager.GetAllTenants()
	
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	
	fmt.Fprintf(w, `{"tenants":[`)
	i := 0
	for _, tenant := range tenants {
		if i > 0 {
			fmt.Fprintf(w, ",")
		}
		fmt.Fprintf(w, `{"id":"%s","name":"%s","namespace":"%s","patterns":%d}`,
			tenant.ID, tenant.Name, tenant.Namespace, len(tenant.MetricPatterns))
		i++
	}
	fmt.Fprintf(w, `]}`)
}

// handleCollectedMetrics returns all collected metrics in Prometheus text format
func (h *Handler) handleCollectedMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	
	// Always log the request for debugging
	logrus.WithFields(logrus.Fields{
		"path":  r.URL.Path,
		"query": r.URL.RawQuery,
	}).Info("DIAGNOSTIC: Processing /collected-metrics request")
	
	// Check if remoteWriteController is available
	if h.remoteWriteController == nil {
		logrus.Warn("DIAGNOSTIC: No remote write controller available")
		fmt.Fprintf(w, "# No remote write controller available. RemoteWrite feature may be disabled.\n")
		fmt.Fprintf(w, "# Attempting direct Prometheus query fallback...\n\n")
		// Force fallback mechanism
		h.executeDirectPrometheusQuery(w, r)
		return
	}
	
	// Get metrics from remote write controller
	metrics := h.remoteWriteController.GetAllCollectedMetrics()
	logrus.WithFields(logrus.Fields{
		"metrics_count": len(metrics),
	}).Info("DIAGNOSTIC: Retrieved metrics from remote write controller")
	
	// FORCE FALLBACK: Always use direct query if no metrics found OR for debugging
	if len(metrics) == 0 {
		logrus.Info("DIAGNOSTIC: No metrics from remote write controller, using direct Prometheus query")
		fmt.Fprintf(w, "# No collected metrics found in remote write controller.\n")
		fmt.Fprintf(w, "# Attempting to fetch metrics directly from Prometheus backend.\n\n")
		
		h.executeDirectPrometheusQuery(w, r)
		return
	}
	
	// If we have metrics from the remote write controller, display them
	logrus.WithFields(logrus.Fields{
		"jobs_count": len(metrics),
	}).Info("DIAGNOSTIC: Displaying metrics from remote write controller")
	
	totalMetricsDisplayed := 0
	for jobKey, jobMetrics := range metrics {
		fmt.Fprintf(w, "# Job: %s (%d metrics)\n", jobKey, len(jobMetrics))
		for _, metric := range jobMetrics {
			// Build label string
			var labels []string
			for name, value := range metric.Labels {
				labels = append(labels, fmt.Sprintf("%s=%q", name, value))
			}
			labelStr := ""
			if len(labels) > 0 {
				labelStr = fmt.Sprintf("{%s}", strings.Join(labels, ","))
			}
			
			// Output in Prometheus text format
			fmt.Fprintf(w, "%s%s %g %d\n",
				metric.Name,
				labelStr,
				metric.Value,
				metric.Timestamp.UnixNano()/int64(time.Millisecond))
			totalMetricsDisplayed++
		}
		fmt.Fprintf(w, "\n")
	}
	
	logrus.WithFields(logrus.Fields{
		"total_metrics": totalMetricsDisplayed,
	}).Info("DIAGNOSTIC: Completed displaying remote write controller metrics")
}

// executeDirectPrometheusQuery performs direct queries to Prometheus targets
func (h *Handler) executeDirectPrometheusQuery(w http.ResponseWriter, r *http.Request) {
	// Get a backend target
	targets := h.serviceDiscovery.GetTargets()
	logrus.WithFields(logrus.Fields{
		"targets_count": len(targets),
	}).Debug("DIAGNOSTIC: Retrieved targets for direct query")
	
	if len(targets) == 0 {
		logrus.Warn("DIAGNOSTIC: No backend targets available")
		fmt.Fprintf(w, "# No backend targets available.\n")
		return
	}
	
	// Pick the first healthy target
	var target discovery.Target
	healthyCount := 0
	for _, t := range targets {
		if t.Healthy {
			if target.URL == "" {
				target = t // Use first healthy target
			}
			healthyCount++
		}
	}
	
	logrus.WithFields(logrus.Fields{
		"healthy_targets": healthyCount,
		"selected_target": target.URL,
	}).Debug("DIAGNOSTIC: Selected target for direct query")
	
	if target.URL == "" {
		logrus.Warn("DIAGNOSTIC: No healthy backend targets available")
		fmt.Fprintf(w, "# No healthy backend targets available.\n")
		fmt.Fprintf(w, "# Total targets: %d, Healthy targets: %d\n", len(targets), healthyCount)
		return
	}
	
	// Get metric name from query parameter if provided
	queryMetric := r.URL.Query().Get("metric")
	
	// List of common metric types to fetch if no specific metric is requested
	metricTypes := []string{
		// System-level metrics that should be available on prometheus-k8s-d
		"node_memory_MemAvailable_bytes",
		"node_cpu_seconds_total",
		"node_filesystem_avail_bytes",
		
		// Kubernetes API server metrics
		"apiserver_request_total",
		"apiserver_request_duration_seconds_count",
		
		// Pod/container metrics
		"container_cpu_usage_seconds_total",
		"container_memory_usage_bytes",
		"kube_pod_status_phase",
		
		// Service/endpoint metrics
		"kube_service_info",
		"kube_endpoint_info",
	}
	
	// If specific metric requested, only query that one
	if queryMetric != "" {
		metricTypes = []string{queryMetric}
		logrus.WithFields(logrus.Fields{
			"requested_metric": queryMetric,
		}).Info("DIAGNOSTIC: Using specific metric from query parameter")
	}
	
	client := &http.Client{Timeout: 30 * time.Second}
	allResults := make(map[string][]byte)
	successCount := 0
	
	// Fetch each metric type
	for _, metricType := range metricTypes {
		queryURL := fmt.Sprintf("%s/api/v1/query?query=%s", target.URL, url.QueryEscape(metricType))
		
		logrus.WithFields(logrus.Fields{
			"metric_type": metricType,
			"query_url":   queryURL,
		}).Debug("DIAGNOSTIC: Executing direct Prometheus query")
		
		resp, err := client.Get(queryURL)
		if err != nil {
			logrus.WithFields(logrus.Fields{
				"metric_type": metricType,
				"error":       err.Error(),
			}).Error("DIAGNOSTIC: Failed to query Prometheus target")
			fmt.Fprintf(w, "# Error querying backend for %s: %v\n", metricType, err)
			continue
		}
		
		body, err := h.readResponseBody(resp)
		resp.Body.Close()
		
		if err != nil {
			logrus.WithFields(logrus.Fields{
				"metric_type": metricType,
				"error":       err.Error(),
			}).Error("DIAGNOSTIC: Failed to read response body")
			fmt.Fprintf(w, "# Error reading response for %s: %v\n", metricType, err)
			continue
		}
		
		logrus.WithFields(logrus.Fields{
			"metric_type":     metricType,
			"response_size":   len(body),
			"response_status": resp.StatusCode,
		}).Debug("DIAGNOSTIC: Received response from Prometheus")
		
		// Store the result
		allResults[metricType] = body
		successCount++
	}
	
	logrus.WithFields(logrus.Fields{
		"total_queries":     len(metricTypes),
		"successful_queries": successCount,
	}).Info("DIAGNOSTIC: Completed all Prometheus queries")
	
	// If no results were fetched, return error
	if len(allResults) == 0 {
		logrus.Warn("DIAGNOSTIC: No metrics could be fetched from backend")
		fmt.Fprintf(w, "# No metrics could be fetched from backend\n")
		fmt.Fprintf(w, "# Target: %s\n", target.URL)
		fmt.Fprintf(w, "# Attempted %d metric types\n", len(metricTypes))
		return
	}
	
	totalMetricsDisplayed := 0
	
	// Process and output all results
	for metricType, body := range allResults {
		fmt.Fprintf(w, "# Metric: %s\n", metricType)
		
		// Parse the Prometheus response
		var promResp struct {
			Status string `json:"status"`
			Data   struct {
				ResultType string `json:"resultType"`
				Result     []struct {
					Metric map[string]string `json:"metric"`
					Value  []interface{}     `json:"value"`
				} `json:"result"`
			} `json:"data"`
		}
		
		if err := json.Unmarshal(body, &promResp); err != nil {
			logrus.WithFields(logrus.Fields{
				"metric_type": metricType,
				"error":       err.Error(),
			}).Error("DIAGNOSTIC: Failed to parse Prometheus response")
			fmt.Fprintf(w, "# Error parsing response: %v\n", err)
			continue
		}
		
		// Format as Prometheus text format
		if promResp.Status == "success" {
			resultCount := 0
			logrus.WithFields(logrus.Fields{
				"metric_type":   metricType,
				"result_count":  len(promResp.Data.Result),
			}).Debug("DIAGNOSTIC: Processing successful Prometheus response")
			
			for _, result := range promResp.Data.Result {
				// Build label string
				var labels []string
				for name, value := range result.Metric {
					labels = append(labels, fmt.Sprintf("%s=%q", name, value))
				}
				labelStr := ""
				if len(labels) > 0 {
					labelStr = fmt.Sprintf("{%s}", strings.Join(labels, ","))
				}
				
				// Get value and timestamp
				var value float64
				var timestamp int64
				
				if len(result.Value) >= 2 {
					// Convert timestamp (first element)
					if ts, ok := result.Value[0].(float64); ok {
						timestamp = int64(ts * 1000) // Convert to milliseconds
					}
					
					// Convert value (second element)
					if val, ok := result.Value[1].(string); ok {
						parsedVal, err := strconv.ParseFloat(val, 64)
						if err == nil {
							value = parsedVal
						}
					}
				}
				
				// Output in Prometheus text format
				metricName := result.Metric["__name__"]
				if metricName == "" {
					// Use the query if no name is found
					metricName = strings.Split(metricType, "{")[0]
				}
				fmt.Fprintf(w, "%s%s %g %d\n", metricName, labelStr, value, timestamp)
				resultCount++
				totalMetricsDisplayed++
				
				// Limit to 10 results per metric type to avoid overwhelming output
				if resultCount >= 10 && !strings.HasPrefix(metricType, "apiserver_") {
					fmt.Fprintf(w, "# ... more results omitted (limited to 10 samples per metric type)\n")
					break
				}
			}
			
			if len(promResp.Data.Result) == 0 {
				logrus.WithFields(logrus.Fields{
					"metric_type": metricType,
				}).Debug("DIAGNOSTIC: No data found for metric type")
				fmt.Fprintf(w, "# No data found for this metric\n")
			}
			
			fmt.Fprintf(w, "\n")
		} else {
			logrus.WithFields(logrus.Fields{
				"metric_type": metricType,
				"status":      promResp.Status,
			}).Error("DIAGNOSTIC: Prometheus returned non-success status")
			fmt.Fprintf(w, "# Error from Prometheus: %s\n\n", promResp.Status)
		}
	}
	
	logrus.WithFields(logrus.Fields{
		"total_metrics_displayed": totalMetricsDisplayed,
	}).Info("DIAGNOSTIC: Completed direct Prometheus query results")
}

// handlePrometheusAPI handles Prometheus API requests
func (h *Handler) handlePrometheusAPI(w http.ResponseWriter, r *http.Request) {
	// Extract tenant information
	tenantInfo, err := h.extractTenantInfo(r)
	if err != nil {
		h.writeError(w, http.StatusUnauthorized, fmt.Sprintf("Authentication failed: %v", err))
		return
	}

	// Get all targets
	targets := h.loadBalancer.GetAllTargets()
	if len(targets) == 0 {
		h.writeError(w, http.StatusServiceUnavailable, "No backends available")
		return
	}

	// Log request details
	logrus.WithFields(logrus.Fields{
		"tenant_id": tenantInfo.ID,
		"path":      r.URL.Path,
		"query":     r.URL.RawQuery,
		"method":    r.Method,
		"targets":   len(targets),
	}).Debug("Processing Prometheus API request")

	// Create HTTP client for querying targets
	client := &http.Client{
		Timeout: 30 * time.Second,
	}

	// Only handle query and query_range endpoints
	if strings.Contains(r.URL.Path, "/api/v1/query") || strings.Contains(r.URL.Path, "/api/v1/query_range") {
		// Query all targets and aggregate results
		aggregatedResults := h.queryAllTargetsAndAggregate(client, targets, r, tenantInfo)
		
		if aggregatedResults == nil {
			h.writeError(w, http.StatusServiceUnavailable, "No successful responses from any target")
			return
		}
		
		// Return aggregated results
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(aggregatedResults)
		return
	} else {
		// For non-query endpoints, use standard load balancing
		target, err := h.loadBalancer.GetTarget()
		if err != nil {
			h.writeError(w, http.StatusServiceUnavailable, fmt.Sprintf("Failed to get target: %v", err))
			return
		}

		// Create reverse proxy
		proxyURL, err := url.Parse(target.URL)
		if err != nil {
			h.writeError(w, http.StatusInternalServerError, fmt.Sprintf("Failed to parse target URL: %v", err))
			return
		}

		// Create and use the reverse proxy
		proxy := httputil.NewSingleHostReverseProxy(proxyURL)
		proxy.ServeHTTP(w, r)
	}
}

// proxyToTarget proxies a request to a single target
func (h *Handler) proxyToTarget(w http.ResponseWriter, r *http.Request, target *discovery.Target) {
	targetURL, err := url.Parse(target.URL)
	if err != nil {
		h.writeError(w, http.StatusInternalServerError, fmt.Sprintf("Invalid target URL: %v", err))
		return
	}

	// Create reverse proxy
	proxy := httputil.NewSingleHostReverseProxy(targetURL)

	// Log request if enabled
	if h.config.Proxy.EnableRequestLogging {
		logrus.WithFields(logrus.Fields{
			"method":    r.Method,
			"path":      r.URL.Path,
			"target":    target.URL,
			"remote_ip": r.RemoteAddr,
		}).Info("Proxying request to single target")
	}

	// Record backend metrics
	if h.config.Proxy.EnableMetrics {
		h.backendRequests.WithLabelValues(target.URL, "success").Inc()
	}

	// Proxy the request
	proxy.ServeHTTP(w, r)
}

// extractTenantInfo extracts tenant information from the request
func (h *Handler) extractTenantInfo(r *http.Request) (*tenant.TenantInfo, error) {
	// Check for internal collection bypass header first
	if r.Header.Get("X-Internal-Collection") == "true" {
		logrus.WithFields(logrus.Fields{
			"path":       r.URL.Path,
			"user_agent": r.Header.Get("User-Agent"),
		}).Info("DIAGNOSTIC: Bypassing tenant filtering for internal collection")
		
		// Return a special internal tenant that has access to all metrics
		return &tenant.TenantInfo{
			ID:             "internal-collection",
			Name:           "Internal Collection Service",
			Namespace:      "internal-collection",
			MetricPatterns: []tenant.MetricPattern{
				{
					Pattern: ".*",  // Match all metrics
					Type:    tenant.PatternTypeRegex,
				},
			},
			Source:         "internal",
			LabelSelectors: map[string]string{}, // No label restrictions
			LastUpdated:    time.Now(),
		}, nil
	}
	
	// Try to extract tenant from different sources
	
	// 1. From X-Tenant-Namespace header
	namespace := r.Header.Get("X-Tenant-Namespace")
	if namespace != "" {
		return h.tenantManager.GetTenant(namespace)
	}
	
	// 2. From Authorization header (if auth is configured)
	if h.config.Auth != nil {
		return h.extractTenantFromAuth(r)
	}
	
	// 3. From query parameter
	namespace = r.URL.Query().Get("namespace")
	if namespace != "" {
		return h.tenantManager.GetTenant(namespace)
	}
	
	return nil, fmt.Errorf("no tenant information found in request")
}

// extractTenantFromAuth extracts tenant information from authentication
func (h *Handler) extractTenantFromAuth(r *http.Request) (*tenant.TenantInfo, error) {
	// This is a simplified implementation
	// In a real scenario, you'd validate the auth token and extract tenant info
	
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		return nil, fmt.Errorf("missing authorization header")
	}
	
	// For now, just extract namespace from a custom header
	namespace := r.Header.Get("X-Tenant-Namespace")
	if namespace == "" {
		return nil, fmt.Errorf("missing tenant namespace")
	}
	
	return h.tenantManager.GetTenant(namespace)
}

// modifyResponse modifies the response based on tenant access rules
func (h *Handler) modifyResponse(resp *http.Response, tenantInfo *tenant.TenantInfo) error {
	// For query endpoints, we need to filter the response
	if strings.Contains(resp.Request.URL.Path, "/api/v1/query") {
		return h.filterQueryResponse(resp, tenantInfo)
	}
	
	// For other endpoints, pass through as-is
	return nil
}

// filterQueryResponse filters query responses based on tenant access rules
func (h *Handler) filterQueryResponse(resp *http.Response, tenantInfo *tenant.TenantInfo) error {
	// Read the response body
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response body: %w", err)
	}
	resp.Body.Close()
	
	// Parse the Prometheus response
	var response map[string]interface{}
	if err := json.Unmarshal(body, &response); err != nil {
		return fmt.Errorf("failed to parse response JSON: %w", err)
	}
	
	// Check if response has the expected structure
	data, ok := response["data"].(map[string]interface{})
	if !ok {
		// If not a standard query response, pass through as-is
		resp.Body = io.NopCloser(strings.NewReader(string(body)))
		resp.ContentLength = int64(len(body))
		return nil
	}
	
	result, ok := data["result"].([]interface{})
	if !ok {
		// If no result array, pass through as-is
		resp.Body = io.NopCloser(strings.NewReader(string(body)))
		resp.ContentLength = int64(len(body))
		return nil
	}
	
	// Filter the results based on tenant access rules
	var filteredResults []interface{}
	filteredCount := 0
	
	for _, item := range result {
		if resultItem, ok := item.(map[string]interface{}); ok {
			// Extract metric information
			metric, ok := resultItem["metric"].(map[string]interface{})
			if !ok {
				continue
			}
			
			// Get metric name
			metricName := ""
			if name, ok := metric["__name__"].(string); ok {
				metricName = name
			}
			
			// Convert metric labels to string map
			labels := make(map[string]string)
			for key, value := range metric {
				if strValue, ok := value.(string); ok {
					labels[key] = strValue
				}
			}
			
			// Validate access using tenant manager
			if h.tenantManager.ValidateAccess(tenantInfo.ID, metricName, labels) {
				filteredResults = append(filteredResults, item)
				filteredCount++
			}
		}
	}
	
	// Log filtering results
	logrus.WithFields(logrus.Fields{
		"tenant_id":        tenantInfo.ID,
		"original_count":   len(result),
		"filtered_count":   filteredCount,
		"filtered_out":     len(result) - filteredCount,
	}).Debug("Filtered query response based on tenant access rules")
	
	// Update the response with filtered results
	data["result"] = filteredResults
	response["data"] = data
	
	// Marshal the filtered response back to JSON
	filteredBody, err := json.Marshal(response)
	if err != nil {
		return fmt.Errorf("failed to marshal filtered response: %w", err)
	}
	
	// Create a new response body with filtered content
	resp.Body = io.NopCloser(strings.NewReader(string(filteredBody)))
	resp.ContentLength = int64(len(filteredBody))
	
	return nil
}

// writeError writes an error response
func (h *Handler) writeError(w http.ResponseWriter, statusCode int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	fmt.Fprintf(w, `{"error":"%s"}`, message)
}

// initMetrics initializes Prometheus metrics
func (h *Handler) initMetrics() {
	h.requestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "prometheus_proxy_requests_total",
			Help: "Total number of requests processed by the proxy",
		},
		[]string{"method", "path", "status"},
	)
	
	h.requestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: "prometheus_proxy_request_duration_seconds",
			Help: "Duration of requests processed by the proxy",
		},
		[]string{"method", "path"},
	)
	
	h.backendRequests = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "prometheus_proxy_backend_requests_total",
			Help: "Total number of requests sent to backends",
		},
		[]string{"backend", "status"},
	)
	
	// Register metrics
	prometheus.MustRegister(h.requestsTotal)
	prometheus.MustRegister(h.requestDuration)
	prometheus.MustRegister(h.backendRequests)
}

// queryAllTargetsAndAggregate queries all targets and aggregates successful responses
func (h *Handler) queryAllTargetsAndAggregate(client *http.Client, targets []discovery.Target, r *http.Request, tenantInfo *tenant.TenantInfo) []byte {
	type targetResponse struct {
		target   string
		response map[string]interface{}
		success  bool
	}
	
	responses := make([]targetResponse, 0, len(targets))
	successCount := 0
	
	// Query all targets
	for _, target := range targets {
		targetURL := target.URL
		
		// Create new request for the target
		targetReq, err := http.NewRequest(r.Method, fmt.Sprintf("%s%s", targetURL, r.URL.RequestURI()), nil)
		if err != nil {
			logrus.WithFields(logrus.Fields{
				"target": targetURL,
				"error":  err,
			}).Debug("Failed to create target request")
			continue
		}
		
		// Copy headers
		for key, values := range r.Header {
			for _, value := range values {
				targetReq.Header.Add(key, value)
			}
		}
		
		resp, err := client.Do(targetReq)
		if err != nil {
			logrus.WithFields(logrus.Fields{
				"target": targetURL,
				"error":  err,
			}).Debug("Failed to execute request to target")
			continue
		}
		
		// Read response body with gzip decompression support
		body, err := h.readResponseBody(resp)
		resp.Body.Close()
		
		if err != nil {
			logrus.WithFields(logrus.Fields{
				"target": targetURL,
				"error":  err,
			}).Debug("Failed to read response body from target")
			continue
		}
		
		// Parse response
		var result map[string]interface{}
		if err := json.Unmarshal(body, &result); err != nil {
			logrus.WithFields(logrus.Fields{
				"target": targetURL,
				"error":  err,
			}).Debug("Failed to parse response JSON from target")
			continue
		}
		
		// Check if response is successful and has results
		if status, ok := result["status"].(string); ok && status == "success" {
			if data, ok := result["data"].(map[string]interface{}); ok {
				if resultArray, ok := data["result"].([]interface{}); ok {
					logrus.WithFields(logrus.Fields{
						"target":       targetURL,
						"status":       resp.StatusCode,
						"result_count": len(resultArray),
					}).Debug("Successful response from target")
					
					responses = append(responses, targetResponse{
						target:   targetURL,
						response: result,
						success:  true,
					})
					successCount++
				}
			}
		}
	}
	
	logrus.WithFields(logrus.Fields{
		"total_targets":      len(targets),
		"successful_targets": successCount,
	}).Debug("Completed querying all targets")
	
	if successCount == 0 {
		return nil
	}
	
	// Aggregate results from all successful responses
	aggregatedResult := map[string]interface{}{
		"status": "success",
		"data": map[string]interface{}{
			"resultType": "vector",
			"result":     []interface{}{},
		},
	}
	
	var allResults []interface{}
	totalMetrics := 0
	filteredMetrics := 0
	
	for _, resp := range responses {
		if !resp.success {
			continue
		}
		
		if data, ok := resp.response["data"].(map[string]interface{}); ok {
			if resultArray, ok := data["result"].([]interface{}); ok {
				// Filter results based on tenant access rules
				var filteredResults []interface{}
				
				for _, item := range resultArray {
					if resultItem, ok := item.(map[string]interface{}); ok {
						// Extract metric information
						metric, ok := resultItem["metric"].(map[string]interface{})
						if !ok {
							continue
						}
						
						// Get metric name
						metricName := ""
						if name, ok := metric["__name__"].(string); ok {
							metricName = name
						}
						
						// Convert metric labels to string map
						labels := make(map[string]string)
						for key, value := range metric {
							if strValue, ok := value.(string); ok {
								labels[key] = strValue
							}
						}
						
						// Validate access using tenant manager
						if h.tenantManager.ValidateAccess(tenantInfo.ID, metricName, labels) {
							filteredResults = append(filteredResults, item)
							filteredMetrics++
						}
					}
				}
				
				allResults = append(allResults, filteredResults...)
				totalMetrics += len(resultArray)
				
				logrus.WithFields(logrus.Fields{
					"target":           resp.target,
					"original_count":   len(resultArray),
					"filtered_count":   len(filteredResults),
					"tenant_id":        tenantInfo.ID,
				}).Debug("Added filtered metrics from target to aggregated result")
			}
		}
	}
	
	// Set aggregated results
	if dataMap, ok := aggregatedResult["data"].(map[string]interface{}); ok {
		dataMap["result"] = allResults
	}
	
	logrus.WithFields(logrus.Fields{
		"total_metrics":      totalMetrics,
		"filtered_metrics":   filteredMetrics,
		"successful_targets": successCount,
		"tenant_id":          tenantInfo.ID,
	}).Info("Aggregated and filtered results from all targets")
	
	// Convert back to JSON
	aggregatedJSON, err := json.Marshal(aggregatedResult)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"error": err,
		}).Error("Failed to marshal aggregated results")
		return nil
	}
	
	return aggregatedJSON
}

// readResponseBody reads and decompresses HTTP response body if needed
func (h *Handler) readResponseBody(resp *http.Response) ([]byte, error) {
	var reader io.Reader = resp.Body
	
	// Check if response is gzipped
	if resp.Header.Get("Content-Encoding") == "gzip" {
		gzipReader, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("failed to create gzip reader: %v", err)
		}
		defer gzipReader.Close()
		reader = gzipReader
	}
	
	return io.ReadAll(reader)
}

// min returns the smaller of two integers
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}