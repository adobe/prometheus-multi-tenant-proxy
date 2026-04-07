# Remote Write Flow

## Overview

The Prometheus Multi-Tenant Proxy implements a **pull-then-push** architecture for remote write functionality:

1. **Pulls metrics** from infrastructure Prometheus instances based on tenant access rules
2. **Filters metrics** according to tenant-specific patterns and permissions
3. **Enriches metrics** with tenant-specific labels
4. **Pushes metrics** to tenant-specific targets (Prometheus, Pushgateway, or external endpoints)

## Architecture Flow

```
┌─────────────────────────────────────────────────────────────────────────────────┐
│                           Infrastructure Layer                                  │
│                                                                                 │
│  ┌─────────────────┐  ┌─────────────────┐  ┌─────────────────┐                │
│  │   Prometheus    │  │   Prometheus    │  │   Prometheus    │                │
│  │   Instance A    │  │   Instance B    │  │   Instance C    │                │
│  │  (monitoring)   │  │  (monitoring)   │  │  (monitoring)   │                │
│  └─────────────────┘  └─────────────────┘  └─────────────────┘                │
│          │                       │                       │                     │
│          │                       │                       │                     │
│          └───────────────────────┼───────────────────────┘                     │
│                                  │                                             │
│                                  │ Pull metrics via                            │
│                                  │ /api/v1/query                               │
└─────────────────────────────────────────────────────────────────────────────────┘
                                   │
                                   ▼
┌─────────────────────────────────────────────────────────────────────────────────┐
│                      Multi-Tenant Proxy (monitoring ns)                        │
│                                                                                 │
│  ┌─────────────────┐  ┌─────────────────┐  ┌─────────────────┐                │
│  │   Remote Write  │  │   Tenant        │  │   Service       │                │
│  │   Controller    │  │   Manager       │  │   Discovery     │                │
│  └─────────────────┘  └─────────────────┘  └─────────────────┘                │
│                                                                                 │
│  ┌─────────────────┐  ┌─────────────────┐  ┌─────────────────┐                │
│  │   Metrics       │  │   Filtering     │  │   Label         │                │
│  │   Collector     │  │   Engine        │  │   Enrichment    │                │
│  └─────────────────┘  └─────────────────┘  └─────────────────┘                │
└─────────────────────────────────────────────────────────────────────────────────┘
                                   │
                                   │ Push metrics via
                                   │ /api/v1/write
                                   ▼
┌─────────────────────────────────────────────────────────────────────────────────┐
│                              Tenant Layer                                      │
│                                                                                 │
│  ┌─────────────────┐  ┌─────────────────┐  ┌─────────────────┐                │
│  │  Tenant A       │  │  Tenant B       │  │  Tenant C       │                │
│  │  Namespace      │  │  Namespace      │  │  Namespace      │                │
│  │                 │  │                 │  │                 │                │
│  │ ┌─────────────┐ │  │ ┌─────────────┐ │  │ ┌─────────────┐ │                │
│  │ │ Prometheus  │ │  │ │ Prometheus  │ │  │ │ Pushgateway │ │                │
│  │ │ (port 9090) │ │  │ │ (port 9090) │ │  │ │ (port 9091) │ │                │
│  │ └─────────────┘ │  │ └─────────────┘ │  │ └─────────────┘ │                │
│  └─────────────────┘  └─────────────────┘  └─────────────────┘                │
└─────────────────────────────────────────────────────────────────────────────────┘
```

## Detailed Flow

### 1. Configuration and Discovery

When a tenant creates a `MetricAccess` resource with remote write enabled:

```yaml
apiVersion: observability.ethos.io/v1alpha1
kind: MetricAccess
metadata:
  name: webapp-metrics
  namespace: webapp-team
spec:
  source: webapp-team
  metrics:
    - "http_requests_total"
    - "http_request_duration_seconds"
    - '{job="webapp"}'
  
  remoteWrite:
    enabled: true
    interval: "30s"
    target:
      type: "prometheus"
    prometheus:
      serviceName: "prometheus"
      servicePort: 9090
    extraLabels:
      tenant: "webapp-team"
      managed_by: "multi-tenant-proxy"
```

### 2. Remote Write Controller Initialization

The Remote Write Controller:
- Watches for `MetricAccess` resources with `remoteWrite.enabled: true`
- Creates a `RemoteWriteJob` for each enabled configuration
- Starts a goroutine that runs on the specified interval (default: 30s)

### 3. Metric Collection Process

For each collection cycle:

1. **Target Discovery**: Gets healthy Prometheus targets from service discovery
2. **Metric Querying**: For each metric pattern in the `MetricAccess`:
   ```go
   // Example queries generated
   queryPattern := "http_requests_total"
   queryURL := fmt.Sprintf("%s/api/v1/query?query=%s", targetURL, url.QueryEscape(queryPattern))
   ```
3. **Result Aggregation**: Combines metrics from all healthy targets
4. **Filtering**: Applies tenant-specific access rules to collected metrics

### 4. Metric Enrichment & Relabeling

Before forwarding, metrics are processed:
- **Metric Relabeling**: `metricRelabelings` rules are applied first, enabling conditional label manipulation (e.g., adding `metrics_path` based on `__name__` patterns)
- **Tenant Labels**: Added from `extraLabels` configuration
- **Management Labels**: Automatic labels like `managed_by: "multi-tenant-proxy"`
- **Original Labels**: Preserved from source metrics (if `honorLabels: true`)
- **Label Conflict Resolution**: When `honorLabels: true`, original metric labels take precedence over `extraLabels`

### 5. Remote Write Delivery

Metrics are sent to the target using the appropriate collector:

#### Prometheus Target
- **Single Replica**: `http://<serviceName>.<namespace>.svc.cluster.local:<port>/api/v1/write`
- **Multi-Replica**: When `replicas` and `statefulSetName` are configured, writes to each pod:
  `http://<statefulSetName>-{0..N}.{serviceName}.{namespace}.svc.cluster.local:{port}/api/v1/write`
- **Format**: Prometheus remote write protocol (protobuf + snappy compression)
- **Headers**: `Content-Type: application/x-protobuf`, `Content-Encoding: snappy`
- **Concurrency**: All replicas are written to concurrently for minimal latency

#### Pushgateway Target
- **Endpoint**: `http://<serviceName>.<namespace>.svc.cluster.local:<port>/metrics/job/<jobName>`
- **Format**: Prometheus exposition format
- **Method**: PUT or POST

#### External Remote Write
- **Endpoint**: User-specified URL
- **Authentication**: Basic auth or custom headers
- **Format**: Prometheus remote write protocol

## Configuration Examples

### Basic Prometheus Remote Write
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

### Pushgateway Configuration
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
  extraLabels:
    tenant: "my-team"
```

### Multi-Replica Prometheus HA
```yaml
remoteWrite:
  enabled: true
  interval: "30s"
  target:
    type: "prometheus"
  prometheus:
    serviceName: "prometheus-operated"
    servicePort: 9090
    replicas: 2
    statefulSetName: "prometheus-ha"
  extraLabels:
    tenant: "my-team"
    cluster: "ethos11-prod-va7"
  honorLabels: true
```

### Per-Metric Relabeling
```yaml
remoteWrite:
  enabled: true
  interval: "30s"
  target:
    type: "prometheus"
  prometheus:
    serviceName: "prometheus-operated"
    servicePort: 9090
  metricRelabelings:
    - sourceLabels: [__name__]
      regex: "container_(.*)"
      targetLabel: metrics_path
      replacement: "/metrics/cadvisor"
    - sourceLabels: [__name__]
      regex: "kubelet_(.*)"
      targetLabel: metrics_path
      replacement: "/metrics"
  extraLabels:
    monitoring_job: "tee-my-namespace"
  honorLabels: true
```

### External Remote Write
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
  extraLabels:
    source_cluster: "production"
```

## Tenant Prometheus Requirements

For successful remote write to a tenant Prometheus instance:

### 1. Enable Remote Write Receiver
```yaml
containers:
- name: prometheus
  image: prom/prometheus:v2.45.0
  args:
    - '--config.file=/etc/prometheus/prometheus.yml'
    - '--storage.tsdb.path=/prometheus'
    - '--web.enable-remote-write-receiver'  # Essential for remote write
```

### 2. Service Configuration
```yaml
apiVersion: v1
kind: Service
metadata:
  name: prometheus  # Must match serviceName in MetricAccess
  namespace: tenant-namespace
spec:
  ports:
  - port: 9090      # Must match servicePort in MetricAccess
    targetPort: 9090
  selector:
    app: prometheus
```

### 3. Network Access
Ensure the multi-tenant proxy can reach the tenant Prometheus:
- **Service discovery**: Prometheus service must be discoverable
- **Network policies**: Allow traffic from monitoring namespace to tenant namespace
- **Firewall rules**: Ensure port 9090 is accessible

## Monitoring and Troubleshooting

### Metrics

The remote write controller exposes metrics:
- `remote_write_metrics_collected_total{tenant, target_type}`
- `remote_write_requests_total{tenant, target_type, status}`
- `remote_write_request_duration_seconds{tenant, target_type}`

### Logs

Key log messages to monitor:
```json
{
  "level": "info",
  "msg": "Collected metrics for remote write job",
  "namespace": "webapp-team",
  "name": "webapp-metrics",
  "count": 1250,
  "targets_used": 3
}
```

### Common Issues

1. **No metrics collected**: Check metric patterns and infrastructure Prometheus accessibility
2. **Remote write failures**: Verify tenant Prometheus has `--web.enable-remote-write-receiver`
3. **Network connectivity**: Ensure proxy can reach tenant services
4. **Authentication errors**: Check service account permissions and network policies

### Debug Endpoints

- `GET /collected-metrics` - View latest collected metrics for all tenants
- `GET /debug/targets` - Check discovered infrastructure Prometheus targets
- `GET /health` - Overall system health including remote write status

## Best Practices

1. **Interval Configuration**: Balance between data freshness and system load
2. **Metric Patterns**: Use specific patterns to avoid collecting unnecessary metrics
3. **Label Management**: Use `extraLabels` for tenant identification and management
4. **Error Handling**: Monitor remote write failures and set up alerting
5. **Resource Management**: Consider memory and CPU impact of frequent collections
6. **Security**: Use network policies to restrict access between namespaces
7. **Multi-Replica**: Always set `replicas` and `statefulSetName` when using Prometheus HA to ensure all replicas receive metrics
8. **Metric Relabeling**: Use `metricRelabelings` instead of multiple CRs when you need to add labels conditionally based on metric name patterns
9. **HonorLabels**: Set `honorLabels: true` when source metrics already carry the correct `job` label to preserve them during remote write
10. **Multiple CRs**: Use multiple `MetricAccess` CRs per namespace when you need different remote write targets or intervals for different metric groups 