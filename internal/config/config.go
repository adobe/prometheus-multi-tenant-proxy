package config

import (
	"fmt"
	"io/ioutil"
	"time"

	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

// Config represents the main configuration structure
type Config struct {
	// Service discovery configuration
	Discovery DiscoveryConfig `yaml:"discovery"`
	
	// Tenant management configuration
	Tenants TenantConfig `yaml:"tenants"`
	
	// Proxy behavior configuration
	Proxy ProxyConfig `yaml:"proxy"`

	// Remote write configuration
	RemoteWrite RemoteWriteConfig `yaml:"remote_write"`

	// Authentication configuration (optional)
	Auth *AuthConfig `yaml:"auth,omitempty"`
}

// DiscoveryConfig holds service discovery settings
type DiscoveryConfig struct {
	// Kubernetes service discovery configuration
	Kubernetes KubernetesDiscoveryConfig `yaml:"kubernetes"`
	
	// Refresh interval for service discovery
	RefreshInterval time.Duration `yaml:"refresh_interval"`
}

// KubernetesDiscoveryConfig holds Kubernetes-specific discovery settings
type KubernetesDiscoveryConfig struct {
	// Namespaces to watch for Prometheus services (empty means all namespaces)
	Namespaces []string `yaml:"namespaces,omitempty"`
	
	// Label selectors for discovering Prometheus services
	LabelSelectors map[string]string `yaml:"label_selectors"`
	
	// Annotation selectors for additional filtering
	AnnotationSelectors map[string]string `yaml:"annotation_selectors,omitempty"`
	
	// Port name or number to use for Prometheus services
	Port string `yaml:"port"`
	
	// Service types to discover (Service, Pod, Endpoints)
	ResourceTypes []string `yaml:"resource_types"`
}

// TenantConfig holds tenant management settings
type TenantConfig struct {
	// Watch all namespaces for MetricAccess resources
	WatchAllNamespaces bool `yaml:"watch_all_namespaces"`
	
	// Specific namespaces to watch (if not watching all)
	WatchNamespaces []string `yaml:"watch_namespaces,omitempty"`
}

// ProxyConfig holds proxy behavior settings
type ProxyConfig struct {
	// Enable query caching
	EnableCaching bool `yaml:"enable_caching"`
	
	// Cache TTL
	CacheTTL time.Duration `yaml:"cache_ttl"`
	
	// Enable metrics collection
	EnableMetrics bool `yaml:"enable_metrics"`
	
	// Enable request logging
	EnableRequestLogging bool `yaml:"enable_request_logging"`
	
	// Maximum concurrent requests to backends
	MaxConcurrentRequests int `yaml:"max_concurrent_requests"`
	
	// Timeout for backend requests
	BackendTimeout time.Duration `yaml:"backend_timeout"`
}

// RemoteWriteConfig holds remote write settings
type RemoteWriteConfig struct {
	// Enable remote write controller
	Enabled bool `yaml:"enabled"`

	// Default collection interval for remote write jobs
	DefaultInterval time.Duration `yaml:"default_interval"`

	// Maximum number of concurrent remote write jobs
	MaxConcurrentJobs int `yaml:"max_concurrent_jobs"`

	// Timeout for metric collection from source targets
	CollectionTimeout time.Duration `yaml:"collection_timeout"`

	// Retry configuration for failed collections
	RetryAttempts int           `yaml:"retry_attempts"`
	RetryDelay    time.Duration `yaml:"retry_delay"`

	// BatchSize is the maximum number of metrics included in a single remote
	// write HTTP request. Prometheus 3.x rejects decoded bodies larger than
	// 32 MiB; splitting into batches keeps each request well under that limit.
	// This applies regardless of the metricIsolation setting on each
	// MetricAccess CR. Batching is always active; the only tunable is the
	// chunk size. Unset or zero values default to 5000.
	BatchSize int `yaml:"batch_size"`

	// StallGracePeriod is how long a per-tenant collection job may go without a
	// successful collection (>=1 series stored AND remote-write send succeeded)
	// before it is classified as stalled. A job that has never succeeded is
	// judged against its creation time. Unset or zero values default to 15m.
	StallGracePeriod time.Duration `yaml:"stall_grace_period"`

	// RecreateBackoff is the minimum interval between successive in-process
	// recreations of the same stalled collection job by the self-heal watchdog
	// A given job is recreated at most once per this window, preventing
	// a persistently unhealthy target from causing tight recreate churn. Unset or
	// zero values default to 5m.
	RecreateBackoff time.Duration `yaml:"recreate_backoff"`

	// PodRestartGracePeriod is how long a per-tenant collection job may remain
	// stalled AFTER at least one in-process recreate attempt before the
	// self-heal watchdog escalates to a LAST-RESORT pod restart. Unset
	// or zero values default to 45m.
	PodRestartGracePeriod time.Duration `yaml:"pod_restart_grace_period"`

	// PodRestartCooldown is the minimum interval between successive self-heal pod
	// restarts. At most one self-restart is attempted per this window,
	// preventing a crash-restart loop. Unset or zero values default to 30m.
	PodRestartCooldown time.Duration `yaml:"pod_restart_cooldown"`

	// SelfHealPodRestart enables the last-resort self-heal pod restart.
	// It is OPT-IN and ships DISABLED: a nil value is
	// treated as false by setDefaults (and by the controller accessor). Set it to
	// true, or pass --self-heal-pod-restart=true, to enable the escalation.
	// Stall detection and in-process recreate remain active regardless.
	SelfHealPodRestart *bool `yaml:"self_heal_pod_restart,omitempty"`
}

// AuthConfig holds authentication settings (optional)
type AuthConfig struct {
	// Type of authentication (jwt, apikey, none)
	Type string `yaml:"type"`
	
	// JWT specific configuration
	JWT *JWTConfig `yaml:"jwt,omitempty"`
	
	// API Key specific configuration
	APIKey *APIKeyConfig `yaml:"apikey,omitempty"`
}

// JWTConfig holds JWT authentication settings
type JWTConfig struct {
	// Secret key for JWT validation
	SecretKey string `yaml:"secret_key"`
	
	// Issuer to validate
	Issuer string `yaml:"issuer"`
	
	// Audience to validate
	Audience string `yaml:"audience"`
}

// APIKeyConfig holds API key authentication settings
type APIKeyConfig struct {
	// Header name containing the API key
	HeaderName string `yaml:"header_name"`
	
	// Static API keys (for simple setups)
	StaticKeys map[string]string `yaml:"static_keys,omitempty"`
}

// Load reads and parses the configuration file
func Load(filename string) (*Config, error) {
	data, err := ioutil.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var config Config
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	// Set defaults
	if err := setDefaults(&config); err != nil {
		return nil, fmt.Errorf("failed to set defaults: %w", err)
	}

	// Validate configuration
	if err := validate(&config); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}

	return &config, nil
}

// setDefaults sets default values for configuration fields
func setDefaults(config *Config) error {
	// Discovery defaults
	if config.Discovery.RefreshInterval == 0 {
		config.Discovery.RefreshInterval = 30 * time.Second
	}
	
	if config.Discovery.Kubernetes.Port == "" {
		config.Discovery.Kubernetes.Port = "9090"
	}
	
	if len(config.Discovery.Kubernetes.ResourceTypes) == 0 {
		config.Discovery.Kubernetes.ResourceTypes = []string{"Service"}
	}
	
	if len(config.Discovery.Kubernetes.LabelSelectors) == 0 {
		config.Discovery.Kubernetes.LabelSelectors = map[string]string{
			"app": "prometheus",
		}
	}
	
	// Proxy defaults
	if config.Proxy.CacheTTL == 0 {
		config.Proxy.CacheTTL = 5 * time.Minute
	}
	
	if config.Proxy.MaxConcurrentRequests == 0 {
		config.Proxy.MaxConcurrentRequests = 100
	}
	
	if config.Proxy.BackendTimeout == 0 {
		config.Proxy.BackendTimeout = 30 * time.Second
	}
	
	// Remote write defaults
	if config.RemoteWrite.DefaultInterval == 0 {
		config.RemoteWrite.DefaultInterval = 30 * time.Second
	}
	
	if config.RemoteWrite.MaxConcurrentJobs == 0 {
		config.RemoteWrite.MaxConcurrentJobs = 10
	}
	
	if config.RemoteWrite.CollectionTimeout == 0 {
		config.RemoteWrite.CollectionTimeout = 30 * time.Second
	}
	
	if config.RemoteWrite.RetryAttempts == 0 {
		config.RemoteWrite.RetryAttempts = 3
	}
	
	if config.RemoteWrite.RetryDelay == 0 {
		config.RemoteWrite.RetryDelay = 5 * time.Second
	}

	if config.RemoteWrite.BatchSize <= 0 {
		config.RemoteWrite.BatchSize = 5000
	}

	if config.RemoteWrite.StallGracePeriod <= 0 {
		config.RemoteWrite.StallGracePeriod = 15 * time.Minute
	}

	if config.RemoteWrite.RecreateBackoff <= 0 {
		config.RemoteWrite.RecreateBackoff = 5 * time.Minute
	}

	if config.RemoteWrite.PodRestartGracePeriod <= 0 {
		config.RemoteWrite.PodRestartGracePeriod = 45 * time.Minute
	}

	if config.RemoteWrite.PodRestartCooldown <= 0 {
		config.RemoteWrite.PodRestartCooldown = 30 * time.Minute
	}

	// SelfHealPodRestart is OPT-IN: the last-resort pod restart ships DISABLED, so
	// a nil (unset) value defaults to false. Stall detection
	// and in-process recreate remain on by default.
	if config.RemoteWrite.SelfHealPodRestart == nil {
		disabled := false
		config.RemoteWrite.SelfHealPodRestart = &disabled
	}

	// Auth defaults
	if config.Auth != nil && config.Auth.APIKey != nil && config.Auth.APIKey.HeaderName == "" {
		config.Auth.APIKey.HeaderName = "X-API-Key"
	}

	return nil
}

// validate checks the configuration for required fields and consistency
func validate(config *Config) error {
	// Non-fatal sanity check: if the pod-restart grace
	// window is shorter than the stall grace period, the last-resort escalation can
	// trigger before in-process recreate has had a fair chance to recover
	// the job. Warn but proceed. Runs after setDefaults, so both values are set.
	if config.RemoteWrite.PodRestartGracePeriod < config.RemoteWrite.StallGracePeriod {
		logrus.WithFields(logrus.Fields{
			"pod_restart_grace_period": config.RemoteWrite.PodRestartGracePeriod,
			"stall_grace_period":       config.RemoteWrite.StallGracePeriod,
		}).Warn("remote_write.pod_restart_grace_period is shorter than stall_grace_period; last-resort pod-restart escalation may be premature")
	}

	// Validate discovery configuration
	if config.Discovery.RefreshInterval <= 0 {
		return fmt.Errorf("discovery.refresh_interval must be positive")
	}
	
	// Validate resource types
	validResourceTypes := map[string]bool{
		"Service":   true,
		"Pod":       true,
		"Endpoints": true,
	}
	
	for _, resourceType := range config.Discovery.Kubernetes.ResourceTypes {
		if !validResourceTypes[resourceType] {
			return fmt.Errorf("invalid resource type: %s", resourceType)
		}
	}
	
	// Validate auth configuration if provided
	if config.Auth != nil {
		validAuthTypes := map[string]bool{
			"jwt":    true,
			"apikey": true,
			"none":   true,
		}
		
		if !validAuthTypes[config.Auth.Type] {
			return fmt.Errorf("invalid auth.type: %s", config.Auth.Type)
		}
		
		if config.Auth.Type == "jwt" && (config.Auth.JWT == nil || config.Auth.JWT.SecretKey == "") {
			return fmt.Errorf("auth.jwt.secret_key is required when using JWT authentication")
		}
	}
	
	return nil
} 