package remote_write

// First-class per-tenant collection-health metrics.
//
// HealthCollector is a prometheus.Collector that reads LIVE controller/job state
// under c.mu on every scrape rather than mutating package-global GaugeVec/
// CounterVec instances from the hot path. Reading live state each scrape has two
// deliberate consequences:
//
//   - No wiring into collectMetrics or the self-heal watchdog is required; the
//     collector simply reflects whatever the jobs currently hold.
//   - A tenant that no longer has a job in c.jobs is simply not emitted, so
//     removed/absent tenants never leave behind stale label sets (AC2). A global
//     GaugeVec would retain deleted label sets and is intentionally NOT used.

import (
	"github.com/prometheus/client_golang/prometheus"
)

var (
	collectionUpDesc = prometheus.NewDesc(
		"proxy_tenant_collection_up",
		"1 if the tenant's last collection cycle stored >=1 series and the remote-write send succeeded, else 0.",
		[]string{"namespace", "name"}, nil,
	)
	lastSuccessfulCollectionDesc = prometheus.NewDesc(
		"proxy_tenant_last_successful_collection_timestamp_seconds",
		"Unix timestamp of the tenant's last successful collection cycle; 0 if it has never succeeded.",
		[]string{"namespace", "name"}, nil,
	)
	collectedSeriesDesc = prometheus.NewDesc(
		"proxy_tenant_collected_series",
		"Number of series stored in the tenant's last collection cycle.",
		[]string{"namespace", "name"}, nil,
	)
	stallRecreationsDesc = prometheus.NewDesc(
		"proxy_tenant_stall_recreations_total",
		"Lifetime count of self-heal recreations of the tenant's stalled collection job.",
		[]string{"namespace", "name"}, nil,
	)
	// proxy_self_restart_total is proxy-wide (no per-tenant label) because a
	// last-resort pod restart is not attributable to a single tenant.
	selfRestartDesc = prometheus.NewDesc(
		"proxy_self_restart_total",
		"Proxy-wide count of last-resort self-heal pod restarts; proxy-wide because a self-restart is not attributable to a single tenant.",
		nil, nil,
	)
)

// HealthCollector exposes per-tenant collection-health metrics by reading live
// Controller job state on each scrape. Register it once on the registry backing
// the proxy's /metrics endpoint.
type HealthCollector struct {
	c *Controller
}

// NewHealthCollector returns a collector bound to the given controller.
func NewHealthCollector(c *Controller) *HealthCollector {
	return &HealthCollector{c: c}
}

// Describe implements prometheus.Collector.
func (hc *HealthCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- collectionUpDesc
	ch <- lastSuccessfulCollectionDesc
	ch <- collectedSeriesDesc
	ch <- stallRecreationsDesc
	ch <- selfRestartDesc
}

// Collect implements prometheus.Collector. It holds c.mu for read for the whole
// sweep so all job/controller fields (guarded by c.mu) are read consistently.
func (hc *HealthCollector) Collect(ch chan<- prometheus.Metric) {
	c := hc.c
	c.mu.RLock()
	defer c.mu.RUnlock()

	for _, job := range c.jobs {
		// Emit only for tenants that currently have an enabled MetricAccess job.
		if job == nil || job.MetricAccess == nil ||
			job.MetricAccess.Spec.RemoteWrite == nil ||
			!job.MetricAccess.Spec.RemoteWrite.Enabled {
			continue
		}
		ns := job.MetricAccess.Namespace
		name := job.MetricAccess.Name

		up := 0.0
		if job.lastCollectionSucceeded {
			up = 1.0
		}
		ch <- prometheus.MustNewConstMetric(collectionUpDesc, prometheus.GaugeValue, up, ns, name)

		var lastSuccess float64
		if !job.lastSuccessfulCollectionTime.IsZero() {
			lastSuccess = float64(job.lastSuccessfulCollectionTime.Unix())
		}
		ch <- prometheus.MustNewConstMetric(lastSuccessfulCollectionDesc, prometheus.GaugeValue, lastSuccess, ns, name)

		ch <- prometheus.MustNewConstMetric(collectedSeriesDesc, prometheus.GaugeValue, float64(job.lastCollectedSeriesCount), ns, name)

		ch <- prometheus.MustNewConstMetric(stallRecreationsDesc, prometheus.CounterValue, float64(job.recreateCount), ns, name)
	}

	// Proxy-wide self-restart counter, read under the same lock.
	ch <- prometheus.MustNewConstMetric(selfRestartDesc, prometheus.CounterValue, float64(c.selfRestartTotal))
}
