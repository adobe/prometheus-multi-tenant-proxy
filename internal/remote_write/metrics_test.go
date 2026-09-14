package remote_write

// Per-tenant collection-health Prometheus metrics.
//
// These tests drive the HealthCollector directly against a controller whose
// c.jobs map is populated by hand, then use the client_golang testutil to
// gather and compare the exposed samples. No live cluster or network is used.
//
// What each scenario asserts:
//   - proxy_tenant_collection_up flips 1 -> 0 -> 1 across a stall then recovery
//     for a tenant.
//   - A tenant removed from c.jobs is no longer emitted — the live-reading
//     Collect() naturally avoids stale cardinality.
//   - The remaining series (series count, last-success timestamp, stall
//     recreations, and the proxy-wide self-restart counter) carry the values
//     sourced from live job / controller state.

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// putJob installs a job directly into the controller's map under lock.
func putJob(c *Controller, key string, job *RemoteWriteJob) {
	c.mu.Lock()
	c.jobs[key] = job
	c.mu.Unlock()
}

// TestHealthCollector_CollectionUp_FlipsAcrossStallAndRecover verifies the
// up gauge tracks the job's last-cycle outcome across stall and recovery.
func TestHealthCollector_CollectionUp_FlipsAcrossStallAndRecover(t *testing.T) {
	c := newTestController(t, &fakeDiscovery{})
	hc := NewHealthCollector(c)

	job := &RemoteWriteJob{
		MetricAccess:            newMetricAccess("tenant-a", "ma"),
		StopCh:                  make(chan struct{}),
		lastCollectionSucceeded: true,
	}
	putJob(c, "tenant-a/ma", job)

	const metric = "proxy_tenant_collection_up"

	// Healthy: up == 1.
	expectUp1 := `
# HELP proxy_tenant_collection_up 1 if the tenant's last collection cycle stored >=1 series and the remote-write send succeeded, else 0.
# TYPE proxy_tenant_collection_up gauge
proxy_tenant_collection_up{name="ma",namespace="tenant-a"} 1
`
	if err := testutil.CollectAndCompare(hc, strings.NewReader(expectUp1), metric); err != nil {
		t.Fatalf("healthy state: %v", err)
	}

	// Stall: last cycle failed -> up == 0.
	c.mu.Lock()
	job.lastCollectionSucceeded = false
	c.mu.Unlock()
	expectUp0 := `
# HELP proxy_tenant_collection_up 1 if the tenant's last collection cycle stored >=1 series and the remote-write send succeeded, else 0.
# TYPE proxy_tenant_collection_up gauge
proxy_tenant_collection_up{name="ma",namespace="tenant-a"} 0
`
	if err := testutil.CollectAndCompare(hc, strings.NewReader(expectUp0), metric); err != nil {
		t.Fatalf("stalled state: %v", err)
	}

	// Recover: up flips back to 1.
	c.mu.Lock()
	job.lastCollectionSucceeded = true
	c.mu.Unlock()
	if err := testutil.CollectAndCompare(hc, strings.NewReader(expectUp1), metric); err != nil {
		t.Fatalf("recovered state: %v", err)
	}
}

// TestHealthCollector_RemovedTenant_NoStaleSeries verifies a tenant removed from
// c.jobs disappears from the exposition (no retained label set).
func TestHealthCollector_RemovedTenant_NoStaleSeries(t *testing.T) {
	c := newTestController(t, &fakeDiscovery{})
	hc := NewHealthCollector(c)

	putJob(c, "tenant-a/ma", &RemoteWriteJob{
		MetricAccess:            newMetricAccess("tenant-a", "ma"),
		StopCh:                  make(chan struct{}),
		lastCollectionSucceeded: true,
	})
	putJob(c, "tenant-b/mb", &RemoteWriteJob{
		MetricAccess:            newMetricAccess("tenant-b", "mb"),
		StopCh:                  make(chan struct{}),
		lastCollectionSucceeded: true,
	})

	const metric = "proxy_tenant_collection_up"

	both := `
# HELP proxy_tenant_collection_up 1 if the tenant's last collection cycle stored >=1 series and the remote-write send succeeded, else 0.
# TYPE proxy_tenant_collection_up gauge
proxy_tenant_collection_up{name="ma",namespace="tenant-a"} 1
proxy_tenant_collection_up{name="mb",namespace="tenant-b"} 1
`
	if err := testutil.CollectAndCompare(hc, strings.NewReader(both), metric); err != nil {
		t.Fatalf("both tenants present: %v", err)
	}

	// Remove tenant-b.
	c.mu.Lock()
	delete(c.jobs, "tenant-b/mb")
	c.mu.Unlock()

	onlyA := `
# HELP proxy_tenant_collection_up 1 if the tenant's last collection cycle stored >=1 series and the remote-write send succeeded, else 0.
# TYPE proxy_tenant_collection_up gauge
proxy_tenant_collection_up{name="ma",namespace="tenant-a"} 1
`
	if err := testutil.CollectAndCompare(hc, strings.NewReader(onlyA), metric); err != nil {
		t.Fatalf("removed tenant still emitted (stale cardinality): %v", err)
	}
}

// TestHealthCollector_ExposesAllHealthMetrics verifies the remaining metrics
// carry live job / controller values, and the self-restart counter is
// proxy-wide (no tenant labels).
func TestHealthCollector_ExposesAllHealthMetrics(t *testing.T) {
	c := newTestController(t, &fakeDiscovery{})
	hc := NewHealthCollector(c)

	lastSuccess := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	putJob(c, "tenant-a/ma", &RemoteWriteJob{
		MetricAccess:                 newMetricAccess("tenant-a", "ma"),
		StopCh:                       make(chan struct{}),
		lastCollectionSucceeded:      true,
		lastSuccessfulCollectionTime: lastSuccess,
		lastCollectedSeriesCount:     42,
		recreateCount:                3,
	})

	c.mu.Lock()
	c.selfRestartTotal = 2
	c.mu.Unlock()

	expected := `
# HELP proxy_tenant_collected_series Number of series stored in the tenant's last collection cycle.
# TYPE proxy_tenant_collected_series gauge
proxy_tenant_collected_series{name="ma",namespace="tenant-a"} 42
# HELP proxy_tenant_last_successful_collection_timestamp_seconds Unix timestamp of the tenant's last successful collection cycle; 0 if it has never succeeded.
# TYPE proxy_tenant_last_successful_collection_timestamp_seconds gauge
proxy_tenant_last_successful_collection_timestamp_seconds{name="ma",namespace="tenant-a"} 1.788264e+09
# HELP proxy_tenant_stall_recreations_total Lifetime count of self-heal recreations of the tenant's stalled collection job.
# TYPE proxy_tenant_stall_recreations_total counter
proxy_tenant_stall_recreations_total{name="ma",namespace="tenant-a"} 3
# HELP proxy_self_restart_total Proxy-wide count of last-resort self-heal pod restarts; proxy-wide because a self-restart is not attributable to a single tenant.
# TYPE proxy_self_restart_total counter
proxy_self_restart_total 2
`
	if err := testutil.CollectAndCompare(hc, strings.NewReader(expected),
		"proxy_tenant_collected_series",
		"proxy_tenant_last_successful_collection_timestamp_seconds",
		"proxy_tenant_stall_recreations_total",
		"proxy_self_restart_total",
	); err != nil {
		t.Fatalf("health metrics mismatch: %v", err)
	}
}
