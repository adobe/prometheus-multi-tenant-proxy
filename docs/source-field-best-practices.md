# Source Field Best Practices

## Understanding the Source Field

The `source` field in MetricAccess serves as an identifier, but its effectiveness depends on how your metrics are labeled in the infrastructure Prometheus.

## Common Scenarios

### 1. **Namespace-Based Isolation (Most Common)**

In standard Kubernetes deployments, metrics are labeled with `namespace` but not `source`. 

**Recommendation**: Use the namespace name as the source for clarity:

```yaml
spec:
  source: ns-team-bkarthik  # Matches the namespace
  metrics:
    - 'container_cpu_usage_seconds_total{namespace="ns-team-bkarthik"}'
```

### 2. **Custom Source Labels**

If your organization adds custom `source` labels to metrics:

```yaml
spec:
  source: team-bkarthik  # Matches the custom source label
  metrics:
    - '{source="team-bkarthik"}'
```

This requires:
- Prometheus scrape configs that add source labels
- Or relabeling rules in Prometheus
- Or applications that expose metrics with source labels

### 3. **Hybrid Approach**

Use source as a logical identifier and rely on metric patterns for filtering:

```yaml
spec:
  source: team-bkarthik  # Logical identifier
  metrics:
    # Use explicit label selectors instead of relying on source
    - 'container_cpu_usage_seconds_total{namespace="ns-team-bkarthik"}'
  labelSelectors:
    namespace: "ns-team-bkarthik"
```

## How the Proxy Uses the Source Field

The proxy can use the `source` field in different ways:

1. **Service Discovery**: Find which Prometheus instances to query
2. **Query Modification**: Add source labels to queries (if configured)
3. **Access Control**: Verify tenant access rights
4. **Routing**: Determine where to send remote write data

## Best Practices

### 1. **Match Your Infrastructure**

Check what labels your metrics actually have:
```bash
# Query your infrastructure Prometheus
curl -G http://prometheus:9090/api/v1/label/__name__/values
curl -G http://prometheus:9090/api/v1/labels
```

### 2. **Be Explicit in Metric Patterns**

Instead of relying on automatic source filtering:
```yaml
# Less reliable (depends on proxy implementation)
metrics:
  - "container_cpu_usage_seconds_total"

# More reliable (explicit filtering)
metrics:
  - 'container_cpu_usage_seconds_total{namespace="ns-team-bkarthik"}'
```

### 3. **Document Your Convention**

Choose a convention and stick to it:
- `source: <namespace>` - Use namespace as source
- `source: <team>-<env>` - Use team and environment
- `source: <team>` - Use team identifier

### 4. **Consider Multi-Environment Setup**

```yaml
# Development
source: team-bkarthik-dev
metrics:
  - '{namespace="ns-team-bkarthik-dev"}'

# Production  
source: team-bkarthik-prod
metrics:
  - '{namespace="ns-team-bkarthik-prod"}'
```

## Example: Adding Source Labels to Metrics

If you want to use source labels, add them in Prometheus scrape config:

```yaml
# prometheus.yml in infrastructure Prometheus
scrape_configs:
  - job_name: 'kubernetes-pods'
    kubernetes_sd_configs:
      - role: pod
    relabel_configs:
      # Add source label based on namespace
      - source_labels: [__meta_kubernetes_namespace]
        regex: 'ns-team-(.*)'
        target_label: source
        replacement: 'team-${1}'
```

This would add `source="team-bkarthik"` to metrics from `ns-team-bkarthik` namespace.

## Troubleshooting

### Check if source filtering is working:

1. **Verify metrics in infrastructure Prometheus**:
   ```bash
   # Check if metrics have source labels
   curl -G http://infra-prometheus:9090/api/v1/query \
     --data-urlencode 'query=up{source="team-bkarthik"}'
   ```

2. **Check MetricAccess processing**:
   ```bash
   kubectl describe metricaccess -n ns-team-bkarthik
   ```

3. **Verify proxy logs**:
   ```bash
   kubectl logs -n monitoring prometheus-multi-tenant-proxy
   ```

## Conclusion

The `source` field is flexible but requires alignment with your metric labeling strategy. For most Kubernetes deployments, using namespace-based filtering in the metrics patterns is more reliable than depending on source label filtering. 