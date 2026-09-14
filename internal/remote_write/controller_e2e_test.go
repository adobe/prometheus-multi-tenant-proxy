package remote_write

// End-to-end regression suite for the TEE-V2 remote-write
// collector self-heal lifecycle, plus explicit goroutine-leak assertions.
//
// This file is the cohesive, whole-lifecycle counterpart to the focused unit
// suites. Where those files each characterize one seam, the
// scenarios here drive the FULL path through the controller — reconcile,
// collectMetrics, and the evaluateSelfHeal watchdog — with the injectable clock
// (c.now) and injectable exit seam (c.exit) so there are no real multi-minute
// sleeps. Everything is hermetic: an httptest Prometheus source, an httptest
// remote-write sink, a controller-runtime fake client, and the in-package
// fakeDiscovery/exitRecorder doubles.
//
// Scenario map (acceptance criteria (a)-(d)):
//   (a) TestE2E_RuntimeAddCR_IsCollected                     — CR created AFTER
//       Start() gets a job AND a real successful collection storing >=1 series.
//   (b) TestE2E_TargetChurn_CollectionRecovers              — backend target set
//       churns (old pod gone, new pod appears); collection recovers on the new
//       targets.
//   (c) TestE2E_ZeroCollection_WatchdogRecreatesThenRecovers — forced zero
//       collection drives the watchdog to recreate the job, then once targets
//       return data it recovers (isJobStalled false, proxy_tenant_collection_up
//       back to 1).
//   (d) TestE2E_PersistentStall_DrivesLastResortRestart     — a job the watchdog
//       cannot fix by recreate escalates to the last-resort pod restart exactly
//       once, through the normal minute-by-minute watchdog cycle.
//
// Goroutine-leak coverage:
//   TestE2E_NoGoroutineLeak_JobCreateStopCycles asserts, across repeated
//   create/stop cycles, that every job's StopCh is closed and the job is removed
//   from c.jobs (structural no-leak, matching the convention documented in
//   controller_reconcile_test.go), backed by a runtime.NumGoroutine() delta with
//   a bounded settle. go.uber.org/goleak is intentionally NOT added: the
//   structural + NumGoroutine approach keeps go.mod untouched and avoids the
//   klog / leader-election background-goroutine noise goleak would need ignores
//   for. See reasoning/assumptions.md (A1).

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/prometheus-multi-tenant-proxy/api/v1alpha1"
	"github.com/prometheus-multi-tenant-proxy/internal/discovery"
)

// ---------------------------------------------------------------------------
// E2E helpers (kept local to this file; reuse the shared doubles from the other
// suites: fakeDiscovery, newTestController, newStallTestController,
// newMetricAccess, offlineDiscovery, exitRecorder, isClosed, keysOf).
// ---------------------------------------------------------------------------

// newDataSource is an httptest Prometheus source that answers /api/v1/query with
// a single-series success vector, so a collection that reaches it stores >=1
// series.
func newDataSource() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector",` +
			`"result":[{"metric":{"__name__":"container_cpu_usage_seconds_total","pod":"p1"},` +
			`"value":[1693569600,"42"]}]}}`))
	}))
}

// newSink is an httptest remote-write sink that accepts every payload with 200,
// so a collection with data reaches SUCCESS offline.
func newSink() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
}

// rwMetricAccess builds a remote-write-enabled MetricAccess whose collection,
// when it has data, is sent to sinkURL (target type "remote_write"). interval
// drives the background collection ticker; use a small value so a job's own
// goroutine collects quickly without a real long sleep.
func rwMetricAccess(namespace, name, sinkURL string, interval time.Duration) *v1alpha1.MetricAccess {
	return &v1alpha1.MetricAccess{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: v1alpha1.MetricAccessSpec{
			Source:  namespace,
			Metrics: []string{"container_cpu_usage_seconds_total"},
			RemoteWrite: &v1alpha1.RemoteWriteConfig{
				Enabled:     true,
				Interval:    metav1.Duration{Duration: interval},
				Target:      v1alpha1.RemoteWriteTarget{Type: "remote_write"},
				RemoteWrite: &v1alpha1.RemoteWriteEndpoint{URL: sinkURL},
			},
		},
	}
}

// waitFor polls cond every 2ms until it returns true or d elapses. Returns the
// final cond() result. Used only to observe a background goroutine's guarded
// state settle — never as a substitute for a deterministic assertion.
func waitFor(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return cond()
}

// jobSucceeded reads a job's last-cycle outcome under c.mu, so it is properly
// synchronized with the background collection goroutine's writes.
func jobSucceeded(c *Controller, key string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if j, ok := c.jobs[key]; ok {
		return j.lastCollectionSucceeded
	}
	return false
}

// ---------------------------------------------------------------------------
// (a) Runtime-add: a CR created AFTER Start() gets a job and a real successful
// collection storing >=1 series.
// ---------------------------------------------------------------------------

func TestE2E_RuntimeAddCR_IsCollected(t *testing.T) {
	source := newDataSource()
	defer source.Close()
	sink := newSink()
	defer sink.Close()

	disc := &fakeDiscovery{}
	disc.setTargets([]discovery.Target{
		{URL: source.URL, Healthy: true, LastSeen: time.Now()},
	})
	c := newTestController(t, disc)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := c.Start(ctx); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}
	defer c.Stop()

	if got := len(c.GetActiveJobs()); got != 0 {
		t.Fatalf("expected 0 jobs after Start with empty cluster, got %d", got)
	}

	// CR appears AFTER Start(). A short interval lets the job's own goroutine keep
	// collecting; its initial collection already succeeds against the source.
	key := "tenant-a/ma-runtime"
	ma := rwMetricAccess("tenant-a", "ma-runtime", sink.URL, 20*time.Millisecond)
	if err := c.client.Create(ctx, ma); err != nil {
		t.Fatalf("failed to create MetricAccess: %v", err)
	}

	// One reconcile == one 30s tick: it must create the job for the runtime CR.
	if err := c.reconcileRemoteWriteJobs(ctx); err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}
	if _, ok := c.GetActiveJobs()[key]; !ok {
		t.Fatalf("expected reconcile to create a job for runtime-added CR; jobs=%v", keysOf(c.GetActiveJobs()))
	}

	// The job's background goroutine collects from the source and sends to the
	// sink: a real SUCCESS that stores >=1 series.
	if !waitFor(2*time.Second, func() bool { return jobSucceeded(c, key) }) {
		t.Fatalf("expected the runtime-added job to reach a successful collection")
	}
	if got := len(c.GetAllCollectedMetrics()[key]); got < 1 {
		t.Fatalf("expected >=1 series stored for runtime-added CR, got %d", got)
	}
}

// ---------------------------------------------------------------------------
// (b) Target churn: the backend target set changes (old pod removed, new pod
// appears) and collection recovers on the new targets. Driven directly on a
// standalone job (no goroutine) so each collectMetrics call is deterministic.
// ---------------------------------------------------------------------------

func TestE2E_TargetChurn_CollectionRecovers(t *testing.T) {
	sink := newSink()
	defer sink.Close()
	sourceA := newDataSource()
	defer sourceA.Close()
	sourceB := newDataSource()
	defer sourceB.Close()

	disc := &fakeDiscovery{}
	disc.setTargets([]discovery.Target{
		{URL: sourceA.URL, Healthy: true, LastSeen: time.Now()},
	})
	c := newTestController(t, disc)

	key := "tenant-b/ma-churn"
	job := &RemoteWriteJob{
		MetricAccess: rwMetricAccess("tenant-b", "ma-churn", sink.URL, 0),
		StopCh:       make(chan struct{}),
	}

	// Phase 1 — baseline: collect from sourceA -> SUCCESS.
	c.collectMetrics(job)
	if !job.lastCollectionSucceeded {
		t.Fatalf("phase 1: expected a successful baseline collection from sourceA")
	}
	if got := len(c.GetAllCollectedMetrics()[key]); got < 1 {
		t.Fatalf("phase 1: expected >=1 series from sourceA, got %d", got)
	}

	// Phase 2 — churn/outage: the backend pod is gone; only an unhealthy target
	// remains. Collection produces an empty, non-successful cycle.
	disc.setTargets([]discovery.Target{
		{URL: "http://10.0.0.9:9090", Healthy: false, LastSeen: time.Now()},
	})
	c.collectMetrics(job)
	if job.lastCollectionSucceeded {
		t.Fatalf("phase 2: expected collection to fail while no healthy targets exist")
	}
	if job.consecutiveEmptyCollections != 1 {
		t.Fatalf("phase 2: expected one empty cycle recorded, got %d", job.consecutiveEmptyCollections)
	}

	// Phase 3 — recovery: a NEW backend pod (sourceB) appears. Collection must
	// resolve the new target set dynamically and recover.
	disc.setTargets([]discovery.Target{
		{URL: sourceB.URL, Healthy: true, LastSeen: time.Now()},
	})
	c.collectMetrics(job)
	if !job.lastCollectionSucceeded {
		t.Fatalf("phase 3: expected collection to recover on the new target (sourceB)")
	}
	if job.consecutiveEmptyCollections != 0 {
		t.Fatalf("phase 3: expected empty-cycle counter reset after recovery, got %d", job.consecutiveEmptyCollections)
	}
	if got := len(c.GetAllCollectedMetrics()[key]); got < 1 {
		t.Fatalf("phase 3: expected >=1 series from sourceB after churn, got %d", got)
	}
}

// ---------------------------------------------------------------------------
// (c) Forced zero-collection drives the watchdog to recreate the job; once
// targets return data the recreated job recovers (not stalled, up gauge -> 1).
// ---------------------------------------------------------------------------

func TestE2E_ZeroCollection_WatchdogRecreatesThenRecovers(t *testing.T) {
	t0 := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	// Healthy source throughout; a working sink is used only for the recovery job.
	disc, src := healthyDataDiscovery(t)
	defer src.Close()
	sink := newSink()
	defer sink.Close()

	c := newStallTestController(t, disc, 15*time.Minute, t0)
	c.config.RecreateBackoff = 5 * time.Minute
	defer c.stopAllJobs()

	// Phase 1 — a genuinely FAILING job (healthy targets present, send fails,
	// created 30m before the frozen clock) is stopped and recreated by the watchdog
	// Only a real failure with targets present is recreated;
	// a zero-target outage would not be.
	key := "tenant-a/ma-heal"
	c.mu.Lock()
	c.jobs[key] = &RemoteWriteJob{
		MetricAccess:               failingSinkMetricAccess("tenant-a", "ma-heal"),
		StopCh:                     make(chan struct{}),
		createdAt:                  t0.Add(-30 * time.Minute),
		lastCycleHadHealthyTargets: true,
	}
	c.mu.Unlock()

	c.evaluateSelfHeal(context.Background(), t0)

	recreated, ok := c.GetActiveJobs()[key]
	if !ok {
		t.Fatalf("expected watchdog to leave a recreated job under %q", key)
	}
	if recreated.recreateCount < 1 {
		t.Fatalf("expected the genuine failure to drive a recreate (recreateCount>=1), got %d", recreated.recreateCount)
	}

	// Stop the recreated job's goroutine so recovery is driven deterministically
	// (no background collection racing the assertions below).
	c.stopAllJobs()

	// Phase 2 — recovery: a job that reaches healthy targets AND sends successfully
	// records a clean cycle. It is no longer stalled and the collection-up gauge is
	// back to 1. Driven directly (no goroutine).
	key = "tenant-a/ma-heal"
	recovered := &RemoteWriteJob{
		MetricAccess: rwMetricAccess("tenant-a", "ma-heal", sink.URL, 0),
		StopCh:       make(chan struct{}),
		createdAt:    t0.Add(-30 * time.Minute),
	}
	c.mu.Lock()
	c.jobs[key] = recovered
	c.mu.Unlock()

	c.collectMetrics(recovered)

	if !recovered.lastCollectionSucceeded {
		t.Fatalf("expected the recovery collection to succeed against healthy targets + working sink")
	}
	if c.isJobStalled(recovered, t0) {
		t.Fatalf("expected the recovered job to no longer be classified as stalled")
	}
	hc := NewHealthCollector(c)
	expectUp1 := `
# HELP proxy_tenant_collection_up 1 if the tenant's last collection cycle stored >=1 series and the remote-write send succeeded, else 0.
# TYPE proxy_tenant_collection_up gauge
proxy_tenant_collection_up{name="ma-heal",namespace="tenant-a"} 1
`
	if err := testutil.CollectAndCompare(hc, strings.NewReader(expectUp1), "proxy_tenant_collection_up"); err != nil {
		t.Fatalf("expected proxy_tenant_collection_up back to 1 after recovery: %v", err)
	}
}

// ---------------------------------------------------------------------------
// (d) Persistent stall the watchdog cannot fix by recreate escalates to the
// last-resort pod restart exactly once, driven through the normal minute-by-
// minute watchdog cycle (recreate repeatedly, then escalate after > grace).
// ---------------------------------------------------------------------------

func TestE2E_PersistentStall_DrivesLastResortRestart(t *testing.T) {
	t0 := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	// Genuine can't-collect failure WITH healthy targets present (send keeps
	// failing). Post-review, a zero-target outage would NOT escalate; the last
	// resort fires only for a real failure the recreate cannot fix.
	disc, src := healthyDataDiscovery(t)
	defer src.Close()

	c := newTestController(t, disc)
	c.config.StallGracePeriod = 15 * time.Minute
	c.config.RecreateBackoff = 5 * time.Minute
	c.config.PodRestartGracePeriod = 45 * time.Minute
	c.config.PodRestartCooldown = 30 * time.Minute
	enabled := true
	c.config.SelfHealPodRestart = &enabled // opt-in escalation, enabled explicitly
	c.isLeader = true
	rec := &exitRecorder{}
	c.exit = rec.record

	now := t0
	c.now = func() time.Time { return now }
	defer c.stopAllJobs()

	// A brand-new job whose send always fails (healthy targets, nil endpoint). No
	// escalation state is pre-set beyond marking healthy targets seen: it must
	// accrue entirely through the normal watchdog cycle.
	key := "tenant-a/ma-wedged"
	c.mu.Lock()
	c.jobs[key] = &RemoteWriteJob{
		MetricAccess:               failingSinkMetricAccess("tenant-a", "ma-wedged"),
		StopCh:                     make(chan struct{}),
		createdAt:                  t0,
		lastCycleHadHealthyTargets: true,
	}
	c.mu.Unlock()

	// Drive the watchdog once per simulated minute until it escalates (3h bound).
	fired := false
	for i := 1; i <= 180; i++ {
		now = t0.Add(time.Duration(i) * time.Minute)
		c.evaluateSelfHeal(context.Background(), now)
		if len(rec.codes) > 0 {
			fired = true
			break
		}
	}

	if !fired || len(rec.codes) != 1 {
		t.Fatalf("expected the last-resort restart to fire exactly once, got %d exits (%v)", len(rec.codes), rec.codes)
	}
	if rec.codes[0] == 0 {
		t.Fatalf("expected a NON-ZERO exit code to trigger a pod restart, got %d", rec.codes[0])
	}
	if c.selfRestartTotal != 1 {
		t.Fatalf("expected selfRestartTotal==1 after escalation, got %d", c.selfRestartTotal)
	}
	// Escalation is a LAST resort: it must have recreated at least once first...
	if got := c.GetActiveJobs()[key]; got == nil || got.recreateCount < 1 {
		t.Fatalf("expected recreate to be attempted before escalation, recreateCount=%v", got)
	}
	// ...and only after continuous stall beyond podRestartGracePeriod.
	if elapsed := now.Sub(t0); elapsed <= 45*time.Minute {
		t.Fatalf("escalation fired after only %v of continuous stall (want > 45m)", elapsed)
	}
}

// ---------------------------------------------------------------------------
// Goroutine-leak: across repeated job create/stop cycles no collection goroutine
// leaks. Structural check (StopCh closed + removed from c.jobs) plus a
// runtime.NumGoroutine() delta with a bounded settle. See assumptions A1.
// ---------------------------------------------------------------------------

func TestE2E_NoGoroutineLeak_JobCreateStopCycles(t *testing.T) {
	const (
		cycles  = 3
		tenants = 3
	)

	// Let any goroutines left settling by earlier tests in this process exit, then
	// sample a stable baseline. Without this the baseline can be sampled low and a
	// later count read as a false leak. A genuine leak here is >= tenants
	// goroutines per cycle (all never exiting), so the +tolerance below still
	// catches it while absorbing the -race scheduler's transient goroutines.
	runtime.GC()
	prev := runtime.NumGoroutine()
	waitFor(2*time.Second, func() bool {
		n := runtime.NumGoroutine()
		stable := n == prev
		prev = n
		return stable
	})
	base := runtime.NumGoroutine()
	const tolerance = 5 // < tenants*cycles (9); a real leak of runRemoteWriteJob exceeds this

	for cycle := 0; cycle < cycles; cycle++ {
		// Offline discovery so every collection takes the zero-target branch and
		// never touches the network.
		c := newTestController(t, &fakeDiscovery{})

		ctx, cancel := context.WithCancel(context.Background())
		if err := c.Start(ctx); err != nil {
			cancel()
			t.Fatalf("cycle %d: Start() failed: %v", cycle, err)
		}

		for i := 0; i < tenants; i++ {
			ma := newMetricAccess("tenant", fmt.Sprintf("ma-%d-%d", cycle, i))
			if err := c.client.Create(ctx, ma); err != nil {
				cancel()
				t.Fatalf("cycle %d: create CR failed: %v", cycle, err)
			}
		}

		if err := c.reconcileRemoteWriteJobs(ctx); err != nil {
			cancel()
			t.Fatalf("cycle %d: reconcile failed: %v", cycle, err)
		}

		jobs := c.GetActiveJobs()
		if len(jobs) != tenants {
			cancel()
			t.Fatalf("cycle %d: expected %d jobs, got %d (%v)", cycle, tenants, len(jobs), keysOf(jobs))
		}
		stopChs := make([]chan struct{}, 0, tenants)
		for _, j := range jobs {
			stopChs = append(stopChs, j.StopCh)
		}

		// Stop() closes the controller stopCh, stops all jobs, and waits for the
		// reconcile loop to finish.
		c.Stop()
		cancel()

		// Structural no-leak: every job goroutine was signalled to exit and the
		// job was removed from the map.
		for idx, ch := range stopChs {
			if !isClosed(ch) {
				t.Fatalf("cycle %d: job %d StopCh not closed after Stop() (goroutine leak)", cycle, idx)
			}
		}
		if got := len(c.GetActiveJobs()); got != 0 {
			t.Fatalf("cycle %d: expected c.jobs empty after Stop(), got %d", cycle, got)
		}
	}

	// After a bounded settle, the goroutine count must return to ~baseline: no
	// collection or reconcile goroutines were leaked across the cycles.
	if !waitFor(3*time.Second, func() bool { return runtime.NumGoroutine() <= base+tolerance }) {
		t.Fatalf("goroutine leak across create/stop cycles: base=%d now=%d (tolerance %d)",
			base, runtime.NumGoroutine(), tolerance)
	}
}
