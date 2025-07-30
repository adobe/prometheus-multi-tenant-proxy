package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// MetricAccessSpec defines the desired state of MetricAccess
type MetricAccessSpec struct {
	// Metrics defines the list of metric patterns this tenant can access
	// Supports metric names, label selectors, and PromQL-like expressions
	// Examples:
	// - "foo" (exact metric name)
	// - "bar{label=\"x\"}" (metric with specific label)
	// - "{__name__=\"foo\",label=\"x\"}" (PromQL-style selector)
	Metrics []string `json:"metrics"`

	// Source defines the namespace or source identifier for the metrics
	// This helps scope the metrics to specific sources
	Source string `json:"source"`

	// Optional: Additional label selectors for fine-grained access control
	LabelSelectors map[string]string `json:"labelSelectors,omitempty"`

	// Remote write configuration for automatic metric collection
	RemoteWrite *RemoteWriteConfig `json:"remoteWrite,omitempty"`
}

// RemoteWriteConfig defines how metrics should be sent via remote write to the tenant namespace
type RemoteWriteConfig struct {
	// Enabled determines if remote write is active for this tenant
	Enabled bool `json:"enabled"`

	// Collection interval (e.g., "30s", "1m")
	Interval metav1.Duration `json:"interval,omitempty"`

	// Target configuration for where to send metrics
	Target RemoteWriteTarget `json:"target"`

	// Prometheus target configuration (at same level as target for CRD compatibility)
	Prometheus *PrometheusTarget `json:"prometheus,omitempty"`

	// Pushgateway target configuration (at same level as target for CRD compatibility)
	Pushgateway *PushgatewayTarget `json:"pushgateway,omitempty"`

	// Remote write target configuration (at same level as target for CRD compatibility)
	RemoteWrite *RemoteWriteEndpoint `json:"remoteWrite,omitempty"`

	// Extra labels to add to all metrics
	ExtraLabels map[string]string `json:"extraLabels,omitempty"`

	// Honor labels from the source metrics
	HonorLabels bool `json:"honorLabels,omitempty"`
}

// RemoteWriteTarget defines where metrics should be sent via remote write
type RemoteWriteTarget struct {
	// Type of target: "prometheus", "pushgateway", "remote_write"
	Type string `json:"type"`

	// Prometheus target configuration
	Prometheus *PrometheusTarget `json:"prometheus,omitempty"`

	// Pushgateway target configuration
	Pushgateway *PushgatewayTarget `json:"pushgateway,omitempty"`

	// Remote write target configuration
	RemoteWrite *RemoteWriteEndpoint `json:"remoteWrite,omitempty"`
}

// PrometheusTarget defines a Prometheus instance target
type PrometheusTarget struct {
	// Service name in the tenant namespace
	ServiceName string `json:"serviceName"`

	// Service port (default: 9090)
	ServicePort int32 `json:"servicePort,omitempty"`
}

// PushgatewayTarget defines a Pushgateway target
type PushgatewayTarget struct {
	// Service name in the tenant namespace
	ServiceName string `json:"serviceName"`

	// Service port (default: 9091)
	ServicePort int32 `json:"servicePort,omitempty"`

	// Job name for pushed metrics
	JobName string `json:"jobName"`
}

// RemoteWriteEndpoint defines a remote write endpoint
type RemoteWriteEndpoint struct {
	// URL of the remote write endpoint
	URL string `json:"url"`

	// Optional authentication
	BasicAuth *BasicAuth `json:"basicAuth,omitempty"`

	// Optional headers
	Headers map[string]string `json:"headers,omitempty"`
}

// BasicAuth defines basic authentication credentials
type BasicAuth struct {
	// Username for basic auth
	Username string `json:"username"`

	// Password reference (secret name and key)
	PasswordSecret SecretReference `json:"passwordSecret"`
}

// SecretReference references a secret for sensitive data
type SecretReference struct {
	// Name of the secret
	Name string `json:"name"`

	// Key within the secret
	Key string `json:"key"`
}

// MetricAccessStatus defines the observed state of MetricAccess
type MetricAccessStatus struct {
	// Conditions represent the latest available observations of the MetricAccess state
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// LastUpdated represents the last time the status was updated
	LastUpdated metav1.Time `json:"lastUpdated,omitempty"`

	// ValidatedMetrics contains the list of validated metric patterns
	ValidatedMetrics []string `json:"validatedMetrics,omitempty"`

	// Remote write status
	RemoteWrite *RemoteWriteStatus `json:"remoteWrite,omitempty"`
}

// RemoteWriteStatus represents the status of metric remote write
type RemoteWriteStatus struct {
	// Active indicates if remote write is currently running
	Active bool `json:"active"`

	// Last collection time
	LastCollection metav1.Time `json:"lastCollection,omitempty"`

	// Number of metrics collected in last run
	MetricsCollected int32 `json:"metricsCollected,omitempty"`

	// Last error encountered
	LastError string `json:"lastError,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:resource:scope=Namespaced

// MetricAccess is the Schema for the metricaccesses API
type MetricAccess struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MetricAccessSpec   `json:"spec,omitempty"`
	Status MetricAccessStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// MetricAccessList contains a list of MetricAccess
type MetricAccessList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MetricAccess `json:"items"`
} 