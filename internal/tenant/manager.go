package tenant

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/prometheus-multi-tenant-proxy/api/v1alpha1"
	"github.com/prometheus-multi-tenant-proxy/internal/config"
	"github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TenantInfo represents a tenant and their access rules
type TenantInfo struct {
	// Unique identifier for the tenant
	ID string
	
	// Display name for the tenant
	Name string
	
	// Namespace where the tenant's MetricAccess resource is defined
	Namespace string
	
	// Metric patterns this tenant can access
	MetricPatterns []MetricPattern
	
	// Source namespace/identifier
	Source string
	
	// Additional label selectors
	LabelSelectors map[string]string
	
	// Last time this tenant info was updated
	LastUpdated time.Time
}

// MetricPattern represents a metric access pattern
type MetricPattern struct {
	// Original pattern string
	Pattern string
	
	// Compiled regex if the pattern is a regex
	Regex *regexp.Regexp
	
	// Type of pattern (exact, regex, promql)
	Type PatternType
	
	// Parsed PromQL selector if applicable
	Selector map[string]string
}

// PatternType defines the type of metric pattern
type PatternType string

const (
	PatternTypeExact  PatternType = "exact"
	PatternTypeRegex  PatternType = "regex"
	PatternTypePromQL PatternType = "promql"
)

// Manager interface defines the contract for tenant management
type Manager interface {
	// Start begins watching for tenant changes
	Start(ctx context.Context) error
	
	// GetTenant returns tenant information by namespace
	GetTenant(namespace string) (*TenantInfo, error)
	
	// GetAllTenants returns all known tenants
	GetAllTenants() map[string]*TenantInfo
	
	// ValidateAccess checks if a tenant can access a specific metric
	ValidateAccess(tenantID, metricName string, labels map[string]string) bool
}

// manager implements the Manager interface
type manager struct {
	client client.Client
	config config.TenantConfig
	
	// Current tenants
	mu      sync.RWMutex
	tenants map[string]*TenantInfo // namespace -> TenantInfo
}

// NewManager creates a new tenant manager
func NewManager(client client.Client, cfg config.TenantConfig) (Manager, error) {
	return &manager{
		client:   client,
		config:   cfg,
		tenants:  make(map[string]*TenantInfo),
	}, nil
}

// Start begins watching for tenant changes
func (m *manager) Start(ctx context.Context) error {
	logrus.Info("Starting tenant manager")
	
	// Initial load of existing MetricAccess resources
	if err := m.loadExistingTenants(ctx); err != nil {
		logrus.Errorf("Failed to load existing tenants: %v", err)
	}
	
	// For now, we'll use periodic refresh instead of watching
	// TODO: Implement proper watching with controller-runtime
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	
	for {
		select {
		case <-ctx.Done():
			logrus.Info("Stopping tenant manager")
			return ctx.Err()
		case <-ticker.C:
			if err := m.loadExistingTenants(ctx); err != nil {
				logrus.Errorf("Failed to refresh tenants: %v", err)
			}
		}
	}
}

// GetTenant returns tenant information by namespace
func (m *manager) GetTenant(namespace string) (*TenantInfo, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	
	tenant, exists := m.tenants[namespace]
	if !exists {
		return nil, fmt.Errorf("tenant not found for namespace: %s", namespace)
	}
	
	return tenant, nil
}

// GetAllTenants returns all known tenants
func (m *manager) GetAllTenants() map[string]*TenantInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	
	// Return a copy to avoid race conditions
	tenants := make(map[string]*TenantInfo)
	for k, v := range m.tenants {
		tenants[k] = v
	}
	
	return tenants
}

// ValidateAccess checks if a tenant can access a specific metric
func (m *manager) ValidateAccess(tenantID, metricName string, labels map[string]string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	
	// Find tenant by ID
	var tenant *TenantInfo
	for _, t := range m.tenants {
		if t.ID == tenantID {
			tenant = t
			break
		}
	}
	
	if tenant == nil {
		logrus.Debugf("Tenant %s not found", tenantID)
		return false
	}
	
	// Check if metric matches any of the tenant's patterns
	for _, pattern := range tenant.MetricPatterns {
		if m.matchesPattern(pattern, metricName, labels) {
			// Additional label selector validation
			if m.matchesLabelSelectors(tenant.LabelSelectors, labels) {
				return true
			}
		}
	}
	
	return false
}

// loadExistingTenants loads all existing MetricAccess resources
func (m *manager) loadExistingTenants(ctx context.Context) error {
	namespaces := m.getWatchNamespaces()
	
	for _, namespace := range namespaces {
		var metricAccessList v1alpha1.MetricAccessList
		
		listOpts := []client.ListOption{}
		if namespace != metav1.NamespaceAll {
			listOpts = append(listOpts, client.InNamespace(namespace))
		}
		
		if err := m.client.List(ctx, &metricAccessList, listOpts...); err != nil {
			logrus.Errorf("Failed to list MetricAccess resources in namespace %s: %v", namespace, err)
			continue
		}
		
		for _, metricAccess := range metricAccessList.Items {
			if err := m.processTenantUpdate(&metricAccess); err != nil {
				logrus.Errorf("Failed to process MetricAccess %s/%s: %v", 
					metricAccess.Namespace, metricAccess.Name, err)
			}
		}
	}
	
	logrus.Infof("Loaded %d tenants", len(m.tenants))
	return nil
}

// processTenantUpdate processes an update to a MetricAccess resource
func (m *manager) processTenantUpdate(metricAccess *v1alpha1.MetricAccess) error {
	tenantInfo, err := m.convertToTenantInfo(metricAccess)
	if err != nil {
		return fmt.Errorf("failed to convert MetricAccess to TenantInfo: %w", err)
	}
	
	m.mu.Lock()
	m.tenants[metricAccess.Namespace] = tenantInfo
	m.mu.Unlock()
	
	logrus.Infof("Updated tenant: %s (namespace: %s)", tenantInfo.ID, tenantInfo.Namespace)
	return nil
}

// convertToTenantInfo converts a MetricAccess resource to TenantInfo
func (m *manager) convertToTenantInfo(metricAccess *v1alpha1.MetricAccess) (*TenantInfo, error) {
	patterns, err := m.parseMetricPatterns(metricAccess.Spec.Metrics)
	if err != nil {
		return nil, fmt.Errorf("failed to parse metric patterns: %w", err)
	}
	
	// Use resource name as tenant ID, namespace as display name
	tenantID := fmt.Sprintf("%s/%s", metricAccess.Namespace, metricAccess.Name)
	
	return &TenantInfo{
		ID:             tenantID,
		Name:           metricAccess.Name,
		Namespace:      metricAccess.Namespace,
		MetricPatterns: patterns,
		Source:         metricAccess.Spec.Source,
		LabelSelectors: metricAccess.Spec.LabelSelectors,
		LastUpdated:    time.Now(),
	}, nil
}

// parseMetricPatterns parses metric patterns from the MetricAccess spec
func (m *manager) parseMetricPatterns(metrics []string) ([]MetricPattern, error) {
	var patterns []MetricPattern
	
	for _, metric := range metrics {
		pattern, err := m.parseMetricPattern(metric)
		if err != nil {
			logrus.Errorf("Failed to parse metric pattern '%s': %v", metric, err)
			continue
		}
		patterns = append(patterns, pattern)
	}
	
	return patterns, nil
}

// parseMetricPattern parses a single metric pattern
func (m *manager) parseMetricPattern(metric string) (MetricPattern, error) {
	pattern := MetricPattern{
		Pattern: metric,
	}
	
	// Check if it's a PromQL-style selector
	if strings.HasPrefix(metric, "{") && strings.HasSuffix(metric, "}") {
		pattern.Type = PatternTypePromQL
		selector, err := m.parsePromQLSelector(metric)
		if err != nil {
			return pattern, fmt.Errorf("failed to parse PromQL selector: %w", err)
		}
		pattern.Selector = selector
		return pattern, nil
	}
	
	// Check if it contains label selectors
	if strings.Contains(metric, "{") && strings.Contains(metric, "}") {
		pattern.Type = PatternTypePromQL
		selector, err := m.parseMetricWithLabels(metric)
		if err != nil {
			return pattern, fmt.Errorf("failed to parse metric with labels: %w", err)
		}
		pattern.Selector = selector
		return pattern, nil
	}
	
	// Check if it's a regex pattern (contains regex metacharacters)
	if strings.ContainsAny(metric, ".*+?^$[](){}|\\") {
		pattern.Type = PatternTypeRegex
		regex, err := regexp.Compile(metric)
		if err != nil {
			return pattern, fmt.Errorf("failed to compile regex: %w", err)
		}
		pattern.Regex = regex
		return pattern, nil
	}
	
	// Default to exact match
	pattern.Type = PatternTypeExact
	return pattern, nil
}

// parsePromQLSelector parses a PromQL-style label selector
func (m *manager) parsePromQLSelector(selector string) (map[string]string, error) {
	// Remove outer braces
	selector = strings.Trim(selector, "{}")
	
	labels := make(map[string]string)
	
	// Simple parsing - split by comma and parse key=value pairs
	parts := strings.Split(selector, ",")
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		
		// Handle key="value" format
		if strings.Contains(part, "=") {
			kv := strings.SplitN(part, "=", 2)
			if len(kv) == 2 {
				key := strings.TrimSpace(kv[0])
				value := strings.Trim(strings.TrimSpace(kv[1]), "\"'")
				labels[key] = value
			}
		}
	}
	
	return labels, nil
}

// parseMetricWithLabels parses a metric name with label selectors
func (m *manager) parseMetricWithLabels(metric string) (map[string]string, error) {
	// Extract metric name and label selector
	parts := strings.SplitN(metric, "{", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid metric format")
	}
	
	metricName := strings.TrimSpace(parts[0])
	labelPart := strings.TrimSuffix(parts[1], "}")
	
	labels, err := m.parsePromQLSelector("{" + labelPart + "}")
	if err != nil {
		return nil, err
	}
	
	// Add metric name as __name__ label
	if metricName != "" {
		labels["__name__"] = metricName
	}
	
	return labels, nil
}

// matchesPattern checks if a metric matches a specific pattern
func (m *manager) matchesPattern(pattern MetricPattern, metricName string, labels map[string]string) bool {
	switch pattern.Type {
	case PatternTypeExact:
		return metricName == pattern.Pattern
	case PatternTypeRegex:
		return pattern.Regex != nil && pattern.Regex.MatchString(metricName)
	case PatternTypePromQL:
		return m.matchesPromQLSelector(pattern.Selector, metricName, labels)
	default:
		return false
	}
}

// matchesPromQLSelector checks if a metric matches a PromQL selector
func (m *manager) matchesPromQLSelector(selector map[string]string, metricName string, labels map[string]string) bool {
	// Check metric name if specified
	if expectedName, exists := selector["__name__"]; exists {
		if metricName != expectedName {
			return false
		}
	}
	
	// Check all other labels
	for key, expectedValue := range selector {
		if key == "__name__" {
			continue
		}
		
		actualValue, exists := labels[key]
		if !exists || actualValue != expectedValue {
			return false
		}
	}
	
	return true
}

// matchesLabelSelectors checks if labels match the tenant's label selectors
func (m *manager) matchesLabelSelectors(selectors map[string]string, labels map[string]string) bool {
	// If no selectors, allow all metrics
	if len(selectors) == 0 {
		return true
	}
	
	// If namespace selector is present, metric must have a namespace label
	if namespaceSelector, hasNamespaceSelector := selectors["namespace"]; hasNamespaceSelector {
		metricNamespace, hasNamespaceLabel := labels["namespace"]
		if !hasNamespaceLabel {
			return false
		}
		if metricNamespace != namespaceSelector {
			return false
		}
		// Remove namespace from selectors since we've handled it
		selectorsCopy := make(map[string]string)
		for k, v := range selectors {
			if k != "namespace" {
				selectorsCopy[k] = v
			}
		}
		selectors = selectorsCopy
	}

	// Check remaining selectors
	for key, expectedValue := range selectors {
		actualValue, exists := labels[key]
		if !exists || actualValue != expectedValue {
			return false
		}
	}
	
	return true
}

// getWatchNamespaces returns the list of namespaces to watch
func (m *manager) getWatchNamespaces() []string {
	if m.config.WatchAllNamespaces {
		return []string{metav1.NamespaceAll}
	}
	
	if len(m.config.WatchNamespaces) > 0 {
		return m.config.WatchNamespaces
	}
	
	// Default to all namespaces if none specified
	return []string{metav1.NamespaceAll}
} 