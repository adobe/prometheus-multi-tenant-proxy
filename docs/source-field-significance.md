# Significance of the `source` Field in MetricAccess Custom Resource

## Overview

The `source` field in the MetricAccess custom resource is a critical component that enables multi-tenant metric isolation and proper routing in the Prometheus Multi-Tenant Proxy system.

```yaml
apiVersion: observability.ethos.io/v1alpha1
kind: MetricAccess
metadata:
  name: webapp-metrics
  namespace: webapp-team
spec:
  source: webapp-team  # <-- This field
  metrics:
    - "http_requests_total"
    - "http_request_duration_seconds"
```

## Key Purposes

### 1. **Namespace/Tenant Identification**

The `source` field identifies which namespace or tenant the metrics belong to. This is essential for:

- **Metric Isolation**: Ensures that tenants can only access metrics from their designated sources
- **Multi-tenancy**: Allows multiple teams to share the same infrastructure Prometheus while maintaining data isolation

### 2. **Service Discovery Scoping**

The proxy uses the `source` field to:

```go
// In the controller
targets := c.serviceDiscovery.GetTargets(metricAccess.Spec.Source)
```

This helps the service discovery mechanism to:
- Find the correct Prometheus instances to query
- Scope the discovery to specific namespaces or labels
- Reduce the search space for better performance

### 3. **Query Routing**

When a tenant queries metrics, the proxy:

1. Checks the MetricAccess resource for the tenant
2. Uses the `source` field to determine which backend Prometheus to query
3. Routes the query to the appropriate infrastructure Prometheus

```
┌─────────────────┐
│   Tenant Query  │
│  namespace: web │
└────────┬────────┘
         │
         ▼
┌─────────────────┐
│  Multi-Tenant   │
│     Proxy       │
│                 │
│ Checks source:  │
│ "webapp-team"   │
└────────┬────────┘
         │
         ▼
┌─────────────────┐
│ Infrastructure  │
│   Prometheus    │
│                 │
│ Queries metrics │
│ with source     │
│ label filter    │
└─────────────────┘
```

### 4. **Label-Based Filtering**

The `source` field can be used to add automatic label filters to queries:

```promql
# Original query from tenant
http_requests_total

# Proxy transforms it to
http_requests_total{source="webapp-team"}
```

This ensures tenants only see metrics relevant to their source.

### 5. **Remote Write Target Selection**

For remote write operations, the `source` helps determine:
- Which metrics to collect from the infrastructure Prometheus
- Where to send the collected metrics (tenant's namespace)

### 6. **Access Control and Security**

The `source` field provides a security boundary:

- **Authorization**: Proxy can verify that the requesting tenant has access to the specified source
- **Audit Trail**: All metric access can be traced back to specific sources
- **Compliance**: Helps maintain data isolation for regulatory requirements

## Example Use Cases

### Use Case 1: Multiple Applications per Team

```yaml
# Team A has multiple applications
---
apiVersion: observability.ethos.io/v1alpha1
kind: MetricAccess
metadata:
  name: frontend-metrics
  namespace: team-a
spec:
  source: team-a-frontend
  metrics:
    - "http_*"
---
apiVersion: observability.ethos.io/v1alpha1
kind: MetricAccess
metadata:
  name: backend-metrics
  namespace: team-a
spec:
  source: team-a-backend
  metrics:
    - "api_*"
    - "database_*"
```

### Use Case 2: Environment Separation

```yaml
# Production metrics
---
apiVersion: observability.ethos.io/v1alpha1
kind: MetricAccess
metadata:
  name: prod-metrics
  namespace: webapp-team
spec:
  source: webapp-prod
  metrics:
    - "*"
---
# Staging metrics
apiVersion: observability.ethos.io/v1alpha1
kind: MetricAccess
metadata:
  name: staging-metrics
  namespace: webapp-team
spec:
  source: webapp-staging
  metrics:
    - "*"
```

### Use Case 3: Cross-Team Metric Sharing

```yaml
# Platform team exposes certain metrics to application teams
apiVersion: observability.ethos.io/v1alpha1
kind: MetricAccess
metadata:
  name: platform-metrics
  namespace: app-team-1
spec:
  source: platform-shared  # Platform team's shared metrics
  metrics:
    - "kubernetes_pod_*"
    - "kubernetes_node_*"
  labelSelectors:
    namespace: "app-team-1"  # Additional filtering
```

## Implementation Details

### Service Discovery Integration

The proxy's service discovery uses the source to find targets:

```go
func (d *Discovery) GetTargets(source string) []Target {
    // Filter Prometheus instances based on source
    // This might use Kubernetes labels, annotations, or namespaces
    
    targets := []Target{}
    for _, svc := range d.services {
        if svc.Labels["source"] == source {
            targets = append(targets, Target{
                URL: fmt.Sprintf("http://%s:%d", svc.Name, svc.Port),
                Source: source,
            })
        }
    }
    return targets
}
```

### Query Modification

The proxy modifies incoming queries to include source filtering:

```go
func (p *Proxy) modifyQuery(query string, source string) string {
    // Add source label to all metric selectors in the query
    // This ensures data isolation
    
    return addLabelToQuery(query, "source", source)
}
```

## Best Practices

1. **Consistent Naming**: Use a consistent naming convention for sources (e.g., `<team>-<app>-<env>`)

2. **Granular Sources**: Create specific sources for different applications or environments rather than broad sources

3. **Documentation**: Document what each source represents and which metrics it contains

4. **Label Alignment**: Ensure your infrastructure Prometheus actually has metrics labeled with the source values

5. **Access Reviews**: Regularly review MetricAccess resources to ensure appropriate access levels

## Benefits Summary

- **Multi-tenancy**: Enables true multi-tenant Prometheus deployments
- **Security**: Provides strong isolation between different teams/applications
- **Flexibility**: Allows fine-grained control over metric access
- **Scalability**: Helps organize metrics in large deployments
- **Auditability**: Clear tracking of who can access what metrics 