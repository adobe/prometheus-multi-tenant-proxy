# Examples Directory

This directory contains comprehensive examples for configuring and deploying the Prometheus Multi-Tenant Proxy with various tenant configurations.

## 📁 File Overview

### Configuration Examples
- **`config.yaml`** - Main proxy configuration for development/testing
- **`tenant-metrics-access.yaml`** - Multiple MetricAccess examples showing different access patterns
- **`tenant-with-remote-write.yaml`** - Advanced remote write configurations (Prometheus, Pushgateway, external)

### Team-Specific Examples
- **`team-bkarthik-simple.yaml`** - Simple MetricAccess for basic tenant setup
- **`team-bkarthik-minimal.yaml`** - Complete minimal setup with StatefulSet + MetricAccess
- **`team-bkarthik-namespace-based.yaml`** - Namespace-based filtering examples
- **`team-bkarthik-prometheus-deployment.yaml`** - Production-ready tenant Prometheus deployment

## 🚀 Quick Start

### 1. Basic Tenant Setup
Start with the simple example:
```bash
kubectl apply -f team-bkarthik-simple.yaml
```

### 2. Complete Tenant Environment
Deploy a full tenant setup with Prometheus:
```bash
kubectl apply -f team-bkarthik-minimal.yaml
```

### 3. Production Deployment
For production use:
```bash
kubectl apply -f team-bkarthik-prometheus-deployment.yaml
```

## 📋 Example Categories

### Basic MetricAccess Examples

#### Simple Metric Access
```yaml
# From: team-bkarthik-simple.yaml
spec:
  source: ns-team-bkarthik
  metrics:
    - 'kube_node_info'
    - 'up'
    - 'kube_pod_info'
    - 'node_memory_Active_bytes'
  remoteWrite:
    enabled: true
    interval: "30s"
```

#### Namespace-Based Filtering
```yaml
# From: team-bkarthik-namespace-based.yaml
spec:
  source: ns-team-bkarthik
  metrics:
    - 'container_cpu_usage_seconds_total{namespace="ns-team-bkarthik"}'
    - 'kube_pod_status_phase{namespace="ns-team-bkarthik"}'
    - '{namespace="ns-team-bkarthik",__name__=~"kube_pod_.*"}'
```

### Remote Write Configurations

#### Basic Prometheus Remote Write
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
    tenant: "team-bkarthik"
    managed_by: "multi-tenant-proxy"
```

#### Pushgateway Remote Write
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

#### External Remote Write
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
```

### Tenant Prometheus Deployments

#### Minimal Setup (Development)
- Uses `emptyDir` for storage
- Basic Prometheus configuration
- Suitable for testing and development

#### Production Setup
- Uses `PersistentVolumeClaim` for storage
- Comprehensive RBAC configuration
- Resource limits and requests
- Health checks and monitoring

## 🔧 Customization Guide

### Adapting Examples for Your Environment

1. **Change Namespace**: Replace `ns-team-bkarthik` with your namespace
2. **Update Metrics**: Modify the `metrics` array based on your requirements
3. **Adjust Resources**: Update CPU/memory limits in StatefulSet configurations
4. **Configure Storage**: Replace `emptyDir` with appropriate storage class

### Common Customizations

#### Different Metric Patterns
```yaml
# Application-specific metrics
metrics:
  - "http_requests_total"
  - "http_request_duration_seconds"
  - '{job="my-app"}'

# Infrastructure metrics
metrics:
  - "node_.*"
  - "kube_.*"
  - "container_.*"

# Regex patterns
metrics:
  - '{__name__=~"webapp_.*"}'
  - "prometheus_.*"
```

#### Custom Labels
```yaml
extraLabels:
  tenant: "my-team"
  environment: "production"
  cluster: "us-west-2"
  managed_by: "multi-tenant-proxy"
```

#### Different Remote Write Intervals
```yaml
# High-frequency for critical metrics
interval: "5s"

# Standard frequency
interval: "30s"

# Low-frequency for batch jobs
interval: "5m"
```

## 🧪 Testing Your Configuration

### 1. Validate MetricAccess
```bash
kubectl get metricaccess -n <your-namespace>
kubectl describe metricaccess <name> -n <your-namespace>
```

### 2. Check Proxy Health
```bash
kubectl port-forward svc/prometheus-multi-tenant-proxy 8080:8080 -n monitoring
curl http://localhost:8080/health
```

### 3. Test Metric Access
```bash
curl -H "X-Tenant-Namespace: <your-namespace>" \
  "http://localhost:8080/api/v1/query?query=up"
```

### 4. Verify Remote Write
```bash
# Check tenant Prometheus
kubectl port-forward svc/prometheus 9090:9090 -n <your-namespace>
curl "http://localhost:9090/api/v1/query?query=up"
```

## 📚 Related Documentation

- **Main README**: `../README.md` - Complete system overview
- **Remote Write Flow**: `../docs/remote-write-flow.md` - Detailed remote write process
- **Source Field Best Practices**: `../docs/source-field-best-practices.md` - Source field usage guide

## 🔍 Troubleshooting

### Common Issues

1. **MetricAccess not working**: Check namespace and RBAC permissions
2. **Remote write failing**: Verify tenant Prometheus has `--web.enable-remote-write-receiver`
3. **No metrics collected**: Ensure metric patterns match available metrics
4. **Network connectivity**: Check service discovery and network policies

### Debug Commands

```bash
# Check proxy logs
kubectl logs -n monitoring -l app=prometheus-multi-tenant-proxy

# Check tenant Prometheus logs
kubectl logs -n <tenant-namespace> <prometheus-pod-name>

# Verify service endpoints
kubectl get endpoints -n <tenant-namespace>

# Check MetricAccess status
kubectl get metricaccess -n <tenant-namespace> -o yaml
```

## 🎯 Best Practices

1. **Start Simple**: Begin with basic examples and gradually add complexity
2. **Test Incrementally**: Verify each component works before adding the next
3. **Monitor Resource Usage**: Set appropriate resource limits for production
4. **Use Specific Metrics**: Avoid broad patterns like `.*` in production
5. **Implement Proper RBAC**: Use least-privilege access for production deployments
6. **Plan Storage**: Use persistent volumes for production Prometheus instances 