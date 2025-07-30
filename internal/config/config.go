package config

import (
	"fmt"
	"io/ioutil"
	"time"

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
	
	// Auth defaults
	if config.Auth != nil && config.Auth.APIKey != nil && config.Auth.APIKey.HeaderName == "" {
		config.Auth.APIKey.HeaderName = "X-API-Key"
	}
	
	return nil
}

// validate checks the configuration for required fields and consistency
func validate(config *Config) error {
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