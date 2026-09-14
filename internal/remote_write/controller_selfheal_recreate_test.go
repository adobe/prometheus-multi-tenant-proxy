package remote_write

// First self-heal action — auto-recreate a stalled
// in-process collection job from the reconcile tick (before any pod restart).
//
// These tests are hermetic and drive the controller directly. Time is made
// testable via the controller's injectable clock (c.now); no real multi-minute
// sleeps. Recreate is verified by evaluateSelfHeal, the watchdog step wired into
// the reconcile loop.
//
// What each scenario asserts:
//   - A stalled job is stopped and recreated by the watchdog with a fresh struct;
//     throttle bookkeeping (lastRecreateAt/recreateCount) is carried forward.
//   - A second watchdog pass inside the recreateBackoff window does NOT recreate
//     again; a pass after the window does.
//   - A successful collection resets stall/recreate state: it advances the
//     success clock, zeroes consecutiveEmptyCollections, and clears the recreate
//     throttle baseline so a future stall is handled fresh — leaving the job
//     no longer classified as stalled.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/prometheus-multi-tenant-proxy/api/v1alpha1"
	"github.com/prometheus-multi-tenant-proxy/internal/discovery"
)

// Recreate half: a genuinely FAILING job (healthy targets present but
// the send keeps failing, never had a clean cycle, created long ago) is stopped
// and recreated by the watchdog with a fresh job struct. Under the review-fixed
// model recreate acts only on a real can't-collect failure with targets present,
// so the setup drives a send failure rather than a zero-target outage.
func TestSelfHeal_StalledJobRecreatedByWatchdog(t *testing.T) {
	t0 := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	// Healthy source serves data; the send fails immediately (nil endpoint), so
	// every cycle is a FAILURE with healthy targets present — the recreatable state.
	disc, src := healthyDataDiscovery(t)
	defer src.Close()

	c := newStallTestController(t, disc, 1*time.Minute, t0)
	c.config.RecreateBackoff = 10 * time.Minute
	defer c.stopAllJobs()

	key := "tenant-a/ma-heal"
	orig := &RemoteWriteJob{
		MetricAccess:               failingSinkMetricAccess("tenant-a", "ma-heal"),
		StopCh:                     make(chan struct{}),
		createdAt:                  t0.Add(-30 * time.Minute), // never had a clean cycle, well past grace
		lastCycleHadHealthyTargets: true,                      // healthy targets present; the SEND fails
	}
	c.mu.Lock()
	c.jobs[key] = orig
	c.mu.Unlock()

	c.evaluateSelfHeal(context.Background(), t0)

	newJob, ok := c.GetActiveJobs()[key]
	if !ok {
		t.Fatalf("expected watchdog to leave a recreated job under %q", key)
	}
	if newJob == orig {
		t.Fatalf("expected a FRESH job struct after recreate, got the same pointer")
	}
	if newJob.recreateCount != 1 {
		t.Fatalf("expected recreateCount==1 after first recreate, got %d", newJob.recreateCount)
	}
	if !newJob.lastRecreateAt.Equal(t0) {
		t.Fatalf("expected lastRecreateAt==%v after recreate, got %v", t0, newJob.lastRecreateAt)
	}
	if !newJob.createdAt.Equal(t0) {
		t.Fatalf("expected recreated job createdAt==%v (fresh baseline), got %v", t0, newJob.createdAt)
	}
}

// Recreation is rate-limited by recreateBackoff. A second watchdog pass
// within the window does NOT recreate; a pass after the window does, and
// recreateCount accumulates across recreates (lifetime counter).
func TestSelfHeal_BackoffPreventsRepeatRecreate(t *testing.T) {
	t0 := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	// Healthy targets present, send fails: a genuine, recreatable failure whose
	// state (lastCycleHadHealthyTargets) the recreated job's goroutine keeps true.
	disc, src := healthyDataDiscovery(t)
	defer src.Close()

	c := newStallTestController(t, disc, 1*time.Minute, t0)
	c.config.RecreateBackoff = 10 * time.Minute
	defer c.stopAllJobs()

	key := "tenant-a/ma-backoff"
	c.mu.Lock()
	c.jobs[key] = &RemoteWriteJob{
		MetricAccess:               failingSinkMetricAccess("tenant-a", "ma-backoff"),
		StopCh:                     make(chan struct{}),
		createdAt:                  t0.Add(-30 * time.Minute),
		lastCycleHadHealthyTargets: true,
	}
	c.mu.Unlock()

	// First pass at t0: recreate.
	c.evaluateSelfHeal(context.Background(), t0)
	j1 := c.GetActiveJobs()[key]
	if j1.recreateCount != 1 {
		t.Fatalf("expected recreateCount==1 after first pass, got %d", j1.recreateCount)
	}

	// Second pass 2m later: the recreated job is stalled again (2m > 1m grace)
	// but the recreate is throttled (2m < 10m backoff) -> NOT recreated.
	t1 := t0.Add(2 * time.Minute)
	c.evaluateSelfHeal(context.Background(), t1)
	j2 := c.GetActiveJobs()[key]
	if j2 != j1 {
		t.Fatalf("expected no recreate inside backoff window (same job pointer)")
	}
	if j2.recreateCount != 1 {
		t.Fatalf("expected recreateCount to stay 1 inside backoff window, got %d", j2.recreateCount)
	}

	// Third pass 11m after the first recreate: backoff has elapsed -> recreate,
	// and the lifetime counter advances to 2.
	t2 := t0.Add(11 * time.Minute)
	c.evaluateSelfHeal(context.Background(), t2)
	j3 := c.GetActiveJobs()[key]
	if j3 == j2 {
		t.Fatalf("expected a recreate after the backoff window elapsed")
	}
	if j3.recreateCount != 2 {
		t.Fatalf("expected recreateCount==2 after backoff window elapsed, got %d", j3.recreateCount)
	}
}

// Recovery half: once targets return data, a successful collection
// resets stall/recreate state and the job is no longer stalled — no manual
// action. Drives collectMetrics all the way to a real SUCCESS using a fake
// Prometheus source and a fake remote-write sink (both offline httptest).
func TestSelfHeal_SuccessfulCollectionRecoversAndResetsThrottle(t *testing.T) {
	t0 := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	// Fake remote-write sink accepts the payload with 200.
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer sink.Close()

	// Fake Prometheus source returns one series so collectMetrics has data to send.
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector",` +
			`"result":[{"metric":{"__name__":"container_cpu_usage_seconds_total","pod":"p1"},` +
			`"value":[1693569600,"42"]}]}}`))
	}))
	defer source.Close()

	disc := &fakeDiscovery{}
	disc.setTargets([]discovery.Target{
		{URL: source.URL, Healthy: true, LastSeen: t0},
	})

	c := newStallTestController(t, disc, 1*time.Minute, t0)
	c.config.RecreateBackoff = 10 * time.Minute

	// A job that was recreated earlier and is currently stalled again: it has a
	// throttle baseline set and has never had a successful collection.
	ma := &v1alpha1.MetricAccess{
		ObjectMeta: metav1.ObjectMeta{Name: "ma-recover", Namespace: "tenant-a"},
		Spec: v1alpha1.MetricAccessSpec{
			Source:  "tenant-a",
			Metrics: []string{"container_cpu_usage_seconds_total"},
			RemoteWrite: &v1alpha1.RemoteWriteConfig{
				Enabled:     true,
				Target:      v1alpha1.RemoteWriteTarget{Type: "remote_write"},
				RemoteWrite: &v1alpha1.RemoteWriteEndpoint{URL: sink.URL},
			},
		},
	}
	job := &RemoteWriteJob{
		MetricAccess:                ma,
		StopCh:                      make(chan struct{}),
		createdAt:                   t0.Add(-30 * time.Minute),
		lastRecreateAt:              t0.Add(-5 * time.Minute),
		recreateCount:               3,
		consecutiveEmptyCollections: 7,
	}

	// Precondition: stalled (never succeeded, created 30m ago, grace 1m).
	if !c.isJobStalled(job, t0) {
		t.Fatalf("precondition failed: expected job to be stalled before recovery")
	}

	// Targets now return data — drive one collection to SUCCESS.
	c.collectMetrics(job)

	if job.lastSuccessfulCollectionTime.IsZero() {
		t.Fatalf("expected a successful collection to advance lastSuccessfulCollectionTime")
	}
	if job.consecutiveEmptyCollections != 0 {
		t.Fatalf("expected consecutiveEmptyCollections reset to 0 on success, got %d", job.consecutiveEmptyCollections)
	}
	if !job.lastRecreateAt.IsZero() {
		t.Fatalf("expected recreate throttle baseline cleared on success, got %v", job.lastRecreateAt)
	}
	if job.recreateCount != 3 {
		t.Fatalf("expected recreateCount preserved as a lifetime counter (3), got %d", job.recreateCount)
	}

	// Recovered: no longer stalled, no manual action.
	if c.isJobStalled(job, t0) {
		t.Fatalf("expected job to no longer be stalled after a successful collection")
	}
}

// Default: recreateBackoff falls back to 5m when unset and honors an
// explicit override, mirroring stallGracePeriod.
func TestRecreateBackoff_DefaultsWhenUnset(t *testing.T) {
	c := newTestController(t, &fakeDiscovery{}) // no RecreateBackoff configured
	if got := c.recreateBackoff(); got != 5*time.Minute {
		t.Fatalf("expected default recreate backoff of 5m, got %v", got)
	}

	c.config.RecreateBackoff = 90 * time.Second
	if got := c.recreateBackoff(); got != 90*time.Second {
		t.Fatalf("expected configured recreate backoff of 90s, got %v", got)
	}
}
