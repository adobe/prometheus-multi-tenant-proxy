# Prometheus Multi-Tenant Proxy

A sophisticated, production-ready Go application that provides secure multi-tenant access to Prometheus metrics in Kubernetes environments. This proxy implements dynamic service discovery, tenant-based access control, intelligent request routing, and automated metric collection with remote write capabilities.

## 🚀 Key Features

### 🔍 Dynamic Service Discovery
- **Kubernetes API Integration**: Automatically discovers Prometheus instances using Kubernetes service discovery
- **Label-based Filtering**: Filter services based on labels like `app.kubernetes.io/name=prometheus`
- **Annotation Support**: Additional filtering using Kubernetes annotations
- **Multi-Resource Support**: Discover from Services, Pods, or Endpoints
- **Continuous Refresh**: Automatically updates the list of available backends with health checking

### 🏢 Multi-Tenant Architecture
- **Custom Resource Definitions**: Uses `MetricAccess` CRDs to define tenant access rules
- **Flexible Metric Patterns**: Support for exact matches, regex patterns, and PromQL-style selectors
- **Namespace Isolation**: Tenant isolation based on Kubernetes namespaces
- **Dynamic Configuration**: Real-time updates when tenant configurations change
- **Metric Filtering**: Real-time filtering of query results based on tenant permissions

### 📊 Remote Write Functionality
- **Automated Metric Collection**: Pulls metrics from infrastructure Prometheus and pushes to tenant instances
- **Multiple Target Types**: Support for Prometheus, Pushgateway, and external remote write endpoints
- **Configurable Intervals**: Per-tenant collection intervals (5s to hours)
- **Extra Labels**: Automatic addition of tenant and management labels
- **Error Handling**: Comprehensive retry logic and error reporting

### 🚦 Advanced Proxy Features
- **Load Balancing**: Round-robin load balancing across healthy backends
- **Health Checking**: Automatic health monitoring of Prometheus backends
- **Request Filtering**: Real-time filtering of metrics based on tenant access rules
- **Metric Aggregation**: Aggregates results from multiple Prometheus instances
- **Authentication**: Header-based tenant authentication with extensible auth plugins

### 📊 Observability & Monitoring
- **Prometheus Metrics**: Built-in metrics for monitoring proxy performance
- **Structured Logging**: JSON-formatted logs with configurable levels
- **Health Endpoints**: Health check and debug endpoints
- **Request Tracing**: Detailed request logging for troubleshooting
- **Debug Endpoints**: Real-time view of targets, tenants, and collected metrics

## 🏗️ Architecture

```
┌─────────────────────────────────────────────────────────────────────────────────┐
│                           Infrastructure Layer                                  │
│  ┌─────────────────┐  ┌─────────────────┐  ┌─────────────────┐                │
│  │   Prometheus    │  │   Prometheus    │  │   Prometheus    │                │
│  │   Instance A    │  │   Instance B    │  │   Instance C    │                │
│  │  (monitoring)   │  │  (monitoring)   │  │  (monitoring)   │                │
│  └─────────────────┘  └─────────────────┘  └─────────────────┘                │
└─────────────────────────────────────────────────────────────────────────────────┘
                                     │
                                     ▼
┌─────────────────────────────────────────────────────────────────────────────────┐
│                      Multi-Tenant Proxy (monitoring ns)                        │
│                                                                                 │
│  ┌─────────────────┐  ┌─────────────────┐  ┌─────────────────┐                │
│  │   Service       │  │   Tenant        │  │   Remote Write  │                │
│  │   Discovery     │  │   Manager       │  │   Controller    │                │
│  └─────────────────┘  └─────────────────┘  └─────────────────┘                │
│                                                                                 │
│  ┌─────────────────┐  ┌─────────────────┐  ┌─────────────────┐                │
│  │   Proxy         │  │   Load          │  │   Metrics       │                │
│  │   Handler       │  │   Balancer      │  │   Collector     │                │
│  └─────────────────┘  └─────────────────┘  └─────────────────┘                │
└─────────────────────────────────────────────────────────────────────────────────┘
                                     │
                                     ▼
┌─────────────────────────────────────────────────────────────────────────────────┐
│                              Tenant Layer                                      │
│                                                                                 │
│  ┌─────────────────┐  ┌─────────────────┐  ┌─────────────────┐                │
│  │  Tenant A       │  │  Tenant B       │  │  Tenant C       │                │
│  │  Namespace      │  │  Namespace      │  │  Namespace      │                │
│  │                 │  │                 │  │                 │                │
│  │ MetricAccess    │  │ MetricAccess    │  │ MetricAccess    │                │
│  │ Prometheus      │  │ Prometheus      │  │ Prometheus      │                │
│  └─────────────────┘  └─────────────────┘  └─────────────────┘                │
└─────────────────────────────────────────────────────────────────────────────────┘
```

## 🚀 Quick Start

### Prerequisites

- Kubernetes cluster (1.19+)
- Go 1.21+ (for building from source)
- kubectl configured to access your cluster

### 1. Deploy the CRD and Proxy

```bash
# Deploy the Custom Resource Definition
kubectl apply -f deploy/kubernetes/crd.yaml

# Deploy the proxy configuration
kubectl apply -f deploy/kubernetes/configmap.yaml

# Deploy the proxy service
kubectl apply -f deploy/kubernetes/deployment.yaml
```

### 2. Create a Tenant Configuration

```yaml
apiVersion: observability.ethos.io/v1alpha1
kind: MetricAccess
metadata:
  name: my-app-metrics
  namespace: my-app-namespace
spec:
  # Source identifier for metrics
  source: my-app-namespace
  
  # Metrics this tenant can access
  metrics:
    - "http_requests_total"
    - "http_request_duration_seconds"
    - '{job="my-app"}'
    - "up{job=\"my-app\"}"
  
  # Optional: Remote write to tenant Prometheus
  remoteWrite:
    enabled: true
    interval: "30s"
    target:
      type: "prometheus"
    prometheus:
      serviceName: "prometheus"
      servicePort: 9090
    extraLabels:
      tenant: "my-app-namespace"
      managed_by: "multi-tenant-proxy"
```

```bash
kubectl apply -f my-tenant-config.yaml
```

### 3. Query Metrics Through the Proxy

```bash
# Port forward to the proxy
kubectl port-forward svc/prometheus-multi-tenant-proxy 8080:8080 -n monitoring

# Query metrics with tenant authentication
curl -H "X-Tenant-Namespace: my-app-namespace" \
  "http://localhost:8080/api/v1/query?query=up"
```

## ⚙️ Configuration

### Proxy Configuration

The proxy is configured via a YAML file mounted as a ConfigMap:

```yaml
# Service Discovery Configuration
discovery:
  kubernetes:
    # Namespaces to watch for Prometheus services
    namespaces:
      - monitoring
    
    # Label selectors for discovering Prometheus services
    label_selectors:
      app.kubernetes.io/name: prometheus
    
    # Port name or number to use for Prometheus services
    port: "9090"
    
    # Resource types to discover
    resource_types:
      - Pod
  
  # How often to refresh the list of targets
  refresh_interval: 30s

# Tenant Management Configuration
tenants:
  # Watch all namespaces for MetricAccess resources
  watch_all_namespaces: true

# Proxy Configuration
proxy:
  # Enable query result caching
  enable_caching: true
  cache_ttl: 5m
  
  # Enable Prometheus metrics collection
  enable_metrics: true
  
  # Enable request logging
  enable_request_logging: true
  
  # Maximum concurrent requests to backends
  max_concurrent_requests: 100
  
  # Timeout for backend requests
  backend_timeout: 30s

# Remote Write Configuration
remote_write:
  # Enable remote write functionality
  enabled: true
  
  # How often to collect and forward metrics
  collection_interval: 30s
  
  # Batch size for remote write
  batch_size: 1000
```

### MetricAccess Custom Resource

The `MetricAccess` CRD defines tenant access rules and remote write configuration:

```yaml
apiVersion: observability.ethos.io/v1alpha1
kind: MetricAccess
metadata:
  name: tenant-metrics-access
  namespace: tenant-namespace
spec:
  # Source namespace/identifier
  source: tenant-namespace
  
  # Metric patterns (supports multiple formats)
  metrics:
    - "http_requests_total"                    # Exact match
    - "http_.*"                               # Regex pattern
    - '{job="my-app"}'                        # PromQL selector
    - '{__name__="up",instance=".*"}'         # Complex selector
    - "node_cpu_seconds_total{job=\"node-exporter\"}"  # With labels
  
  # Optional: Additional label selectors
  labelSelectors:
    environment: "production"
    team: "webapp"
  
  # Remote write configuration
  remoteWrite:
    enabled: true
    interval: "30s"
    target:
      type: "prometheus"  # or "pushgateway", "remote_write"
    prometheus:
      serviceName: "prometheus"
      servicePort: 9090
    extraLabels:
      tenant: "tenant-namespace"
      managed_by: "multi-tenant-proxy"
      environment: "production"
    honorLabels: true
```

## 📊 Remote Write Feature

The remote write functionality automatically collects metrics from infrastructure Prometheus instances and forwards them to tenant-specific targets.

### How Remote Write Works

1. **Metric Collection**: The proxy queries infrastructure Prometheus instances for metrics matching tenant patterns
2. **Filtering**: Applies tenant-specific filtering rules to collected metrics
3. **Enrichment**: Adds extra labels for tenant identification and management
4. **Forwarding**: Sends metrics to the tenant's Prometheus instance using the standard `/api/v1/write` endpoint

### Remote Write Target Types

#### 1. Prometheus Target
```yaml
remoteWrite:
  enabled: true
  interval: "30s"
  target:
    type: "prometheus"
  prometheus:
    serviceName: "prometheus"
    servicePort: 9090
  extraLabels:
    tenant: "my-team"
```

#### 2. Pushgateway Target
```yaml
remoteWrite:
  enabled: true
  interval: "60s"
  target:
    type: "pushgateway"
  pushgateway:
    serviceName: "pushgateway"
    servicePort: 9091
    jobName: "remote-write-metrics"
```

#### 3. External Remote Write Endpoint
```yaml
remoteWrite:
  enabled: true
  interval: "15s"
  target:
    type: "remote_write"
  remoteWrite:
    url: "https://external-prometheus.example.com/api/v1/write"
    basicAuth:
      username: "monitoring-user"
      passwordSecret:
        name: "prometheus-auth"
        key: "password"
    headers:
      X-Tenant: "my-team"
```

### Tenant Prometheus Configuration

For remote write to work, the tenant Prometheus instance must be configured to accept remote write requests:

```yaml
# Tenant Prometheus configuration
apiVersion: v1
kind: ConfigMap
metadata:
  name: prometheus-config
  namespace: tenant-namespace
data:
  prometheus.yml: |
    global:
      scrape_interval: 15s
    
    scrape_configs:
      - job_name: 'prometheus'
        static_configs:
          - targets: ['localhost:9090']

---
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: prometheus
  namespace: tenant-namespace
spec:
  serviceName: prometheus
  template:
    spec:
      containers:
      - name: prometheus
        image: prom/prometheus:v2.45.0
        args:
          - '--config.file=/etc/prometheus/prometheus.yml'
          - '--storage.tsdb.path=/prometheus'
          - '--web.enable-remote-write-receiver'  # Enable remote write
          - '--web.console.libraries=/etc/prometheus/console_libraries'
          - '--web.console.templates=/etc/prometheus/consoles'
        ports:
        - containerPort: 9090
          name: web
```

## 🔧 Metric Pattern Types

### 1. Exact Match
```yaml
metrics:
  - "http_requests_total"
  - "up"
  - "prometheus_build_info"
```

### 2. Regex Patterns
```yaml
metrics:
  - "http_.*"                    # All metrics starting with "http_"
  - ".*_duration_.*"             # All metrics containing "_duration_"
  - "node_.*"                    # All node exporter metrics
```

### 3. PromQL-style Selectors
```yaml
metrics:
  - '{job="my-app"}'                           # All metrics from specific job
  - '{__name__="up",instance=".*"}'            # Up metric from any instance
  - "http_requests_total{method=\"GET\"}"      # Specific metric with label
  - '{__name__=~"node_.*",job="node-exporter"}' # Regex with job filter
```

### 4. Complex Selectors
```yaml
metrics:
  - '{__name__=~"node_cpu_seconds_total|node_memory_MemAvailable_bytes",job="node-exporter",container="kube-rbac-proxy",namespace="monitoring"}'
```

## 🛠️ Development

### Building from Source

```bash
# Clone the repository
git clone https://github.com/your-org/prometheus-multi-tenant-proxy
cd prometheus-multi-tenant-proxy

# Build the binary
go build -o prometheus-multi-tenant-proxy ./cmd/proxy

# Run tests
go test ./...

# Build Docker image
docker build -t prometheus-multi-tenant-proxy:latest .
```

### Local Development

```bash
# Run locally with debug logging
./prometheus-multi-tenant-proxy \
  --config=examples/config.yaml \
  --port=8080 \
  --log-level=debug
```

### Project Structure

```
├── api/v1alpha1/           # Custom Resource Definitions
│   ├── metricaccess_types.go
│   └── zz_generated.deepcopy.go
├── cmd/proxy/              # Main application entry point
│   └── main.go
├── internal/
│   ├── config/            # Configuration management
│   ├── discovery/         # Kubernetes service discovery
│   ├── proxy/             # HTTP proxy and load balancing
│   ├── tenant/            # Tenant management and access control
│   └── remote_write/      # Remote write functionality
├── deploy/kubernetes/      # Kubernetes manifests
├── examples/              # Example configurations
├── docs/                  # Documentation
├── Dockerfile             # Container image definition
├── Makefile              # Build automation
└── README.md             # This file
```

## 🚀 Deployment Options

### Kubernetes (Recommended)

Deploy using the provided Kubernetes manifests:

```bash
# Deploy everything
kubectl apply -f deploy/kubernetes/

# Or step by step
kubectl apply -f deploy/kubernetes/crd.yaml
kubectl apply -f deploy/kubernetes/configmap.yaml
kubectl apply -f deploy/kubernetes/deployment.yaml
```

### Docker

Run as a Docker container:

```bash
docker run -p 8080:8080 \
  -v /path/to/config.yaml:/etc/prometheus-proxy/config.yaml \
  bnkarthik6/prometheus-multi-tenant-proxy:latest \
  --config=/etc/prometheus-proxy/config.yaml
```

### Binary

Run the compiled binary directly:

```bash
./prometheus-multi-tenant-proxy \
  --config=config.yaml \
  --port=8080 \
  --log-level=info
```

## 📡 API Endpoints

### Prometheus API (Proxied & Filtered)
- `GET /api/v1/query` - Query endpoint with tenant filtering
- `GET /api/v1/query_range` - Range query endpoint with tenant filtering
- `GET /api/v1/series` - Series endpoint (proxied)
- `GET /api/v1/labels` - Labels endpoint (proxied)

### Management Endpoints
- `GET /health` - Health check with tenant and target statistics
- `GET /metrics` - Prometheus metrics (if enabled)
- `GET /debug/targets` - Show discovered Prometheus targets
- `GET /debug/tenants` - Show tenant information and access rules
- `GET /collected-metrics` - Show metrics collected by remote write controller

### Authentication

Requests must include tenant identification via:
- `X-Tenant-Namespace` header (primary method)
- `namespace` query parameter (fallback)

Example:
```bash
curl -H "X-Tenant-Namespace: my-app-namespace" \
  "http://localhost:8080/api/v1/query?query=up"
```

## 🔒 Security Considerations

### Authentication & Authorization
- **Header-based Authentication**: Uses `X-Tenant-Namespace` header for tenant identification
- **Namespace Isolation**: Tenants can only access their own namespace configurations
- **Metric-level Access Control**: Fine-grained control over which metrics tenants can access
- **Label-based Filtering**: Additional filtering based on metric labels

### Network Security
- **Service Account**: Runs with minimal RBAC permissions
- **Non-root User**: Container runs as non-root user (65534)
- **Read-only Filesystem**: Container filesystem is read-only
- **Security Context**: Drops all capabilities and prevents privilege escalation

### Tenant Isolation
- **Namespace Boundaries**: Each tenant operates within their own namespace
- **Resource Quotas**: Kubernetes resource quotas can limit tenant resource usage
- **Network Policies**: Can be used to restrict network access between tenants

## 📊 Monitoring and Troubleshooting

### Metrics

The proxy exposes the following metrics:

- `prometheus_proxy_requests_total` - Total requests processed
- `prometheus_proxy_request_duration_seconds` - Request duration histogram
- `prometheus_proxy_backend_requests_total` - Backend request counters
- `prometheus_proxy_targets_discovered` - Number of discovered targets
- `prometheus_proxy_tenants_active` - Number of active tenants

### Logging

Structured JSON logs with configurable levels:

```json
{
  "level": "info",
  "time": "2024-01-15T10:30:00Z",
  "msg": "Aggregated and filtered results from all targets",
  "tenant_id": "my-app-namespace/my-app-metrics",
  "total_metrics": 1363,
  "filtered_metrics": 1363,
  "successful_targets": 3
}
```

### Debug Endpoints

- `/health` - Health status and statistics
- `/debug/targets` - View discovered Prometheus targets with health status
- `/debug/tenants` - View active tenant configurations and access rules
- `/collected-metrics` - View metrics collected by the remote write controller

### Troubleshooting Common Issues

1. **No metrics returned**: Check tenant authentication header and MetricAccess configuration
2. **Metrics not filtered**: Verify MetricAccess patterns and tenant namespace
3. **Remote write not working**: Check tenant Prometheus configuration and network connectivity
4. **Discovery issues**: Verify service discovery configuration and RBAC permissions

## 🤝 Contributing

We welcome contributions! Please see our [Contributing Guide](CONTRIBUTING.md) for details.

### Development Workflow

1. Fork the repository
2. Create a feature branch
3. Make your changes
4. Add tests
5. Run the test suite: `go test ./...`
6. Submit a pull request

## 📄 License

This project is licensed under the Apache License 2.0 - see the [LICENSE](LICENSE) file for details.

## 🆘 Support

- 📖 [Documentation](docs/)
- 🐛 [Issue Tracker](https://github.com/your-org/prometheus-multi-tenant-proxy/issues)
- 💬 [Discussions](https://github.com/your-org/prometheus-multi-tenant-proxy/discussions)

## 🗺️ Roadmap

- [x] ✅ Multi-tenant metric filtering
- [x] ✅ Remote write functionality
- [x] ✅ Dynamic service discovery
- [x] ✅ Kubernetes CRD support
- [ ] 🔄 Advanced query rewriting
- [ ] 🔄 Multi-cluster support
- [ ] 🔄 Grafana integration
- [ ] 🔄 Advanced authentication providers (OIDC, LDAP)
- [ ] 🔄 Query result caching with Redis
- [ ] 🔄 Horizontal pod autoscaling support
- [ ] 🔄 Custom metrics and alerting rules
