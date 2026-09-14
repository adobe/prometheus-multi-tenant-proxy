package remote_write

// Self-heal safety regression suite.
//
// The original self-heal conflated "cannot collect" with "nothing to collect":
// a tenant with zero matching source series — or any period with zero healthy
// targets (upstream outage) — was classified as stalled forever and drove a
// process-wide pod restart LOOP, shipping ENABLED by default. These tests pin the
// fixed contract:
//
//   - Self-heal acts ONLY on a genuine can't-collect FAILURE while healthy
//     targets are present. A cleanly-empty tenant (targets fine, 0 series) and a
//     zero-healthy-targets outage are NEVER recreated and NEVER restart the pod.
//   - The self-heal trigger keys off lastCleanCycleTime, diverging from the
//     STRICT-success lastCollectionSucceeded that still drives the
//     up-gauge / stall alert.
//   - The last-resort pod restart is OPT-IN: with SelfHealPodRestart unset it
//     never fires, even for a genuinely failing job (in-process recreate still
//     runs).
//
// All tests are hermetic and use the injectable clock (c.now) and exit seam
// (c.exit); no real sleeps.

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus-multi-tenant-proxy/internal/discovery"
)

// Blocker fix #1: a cleanly-empty tenant (healthy targets, 0 series, no error) is
// NEVER classified as stalled and is neither recreated nor restarted, even with
// escalation explicitly enabled and the clock advanced far past every grace
// window — so long as it keeps producing clean cycles.
func TestSelfHeal_CleanlyEmptyTenant_NeverStalledRecreatedOrRestarted(t *testing.T) {
	t0 := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	// Healthy target that answers queries successfully with an EMPTY result set:
	// legitimately nothing to collect.
	empty := newFakePromServer()
	defer empty.close()
	disc := &fakeDiscovery{}
	disc.setTargets([]discovery.Target{
		{URL: empty.server.URL, Healthy: true, LastSeen: t0},
	})

	c := newStallTestController(t, disc, 15*time.Minute, t0)
	c.config.RecreateBackoff = 5 * time.Minute
	c.config.PodRestartGracePeriod = 45 * time.Minute
	c.config.PodRestartCooldown = 30 * time.Minute
	enabled := true
	c.config.SelfHealPodRestart = &enabled // escalation ON, to prove even then nothing fires
	c.isLeader = true
	rec := &exitRecorder{}
	c.exit = rec.record
	defer c.stopAllJobs()

	now := t0
	c.now = func() time.Time { return now }

	key := "tenant-a/ma-empty"
	job := &RemoteWriteJob{
		MetricAccess: newMetricAccess("tenant-a", "ma-empty"),
		StopCh:       make(chan struct{}),
		createdAt:    t0.Add(-2 * time.Hour),
	}
	c.mu.Lock()
	c.jobs[key] = job
	c.mu.Unlock()

	// One clean-empty cycle characterizes the fields.
	c.collectMetrics(job)
	if job.lastCleanCycleTime.IsZero() {
		t.Fatalf("expected a clean-empty cycle to refresh lastCleanCycleTime")
	}
	if !job.lastCycleHadHealthyTargets {
		t.Fatalf("expected clean-empty cycle to record healthy targets present")
	}
	if job.lastCollectionSucceeded {
		t.Fatalf("clean-empty must still report collection_up=0 (strict success stays false)")
	}
	if !job.lastSuccessfulCollectionTime.IsZero() {
		t.Fatalf("clean-empty is NOT a strict success; lastSuccessfulCollectionTime must stay zero")
	}

	// Drive many clean-empty cycles well past the restart grace (60 * 5m = 5h).
	for i := 1; i <= 60; i++ {
		now = t0.Add(time.Duration(i) * 5 * time.Minute)
		c.collectMetrics(job)
		c.evaluateSelfHeal(context.Background(), now)

		if c.isJobStalled(job, now) {
			t.Fatalf("a continuously clean-empty tenant must never be stalled (at %v)", now)
		}
		if len(rec.codes) != 0 {
			t.Fatalf("a cleanly-empty tenant must NEVER restart the pod, exits=%v at %v", rec.codes, now)
		}
		got := c.GetActiveJobs()[key]
		if got == nil || got.recreateCount != 0 {
			t.Fatalf("a cleanly-empty tenant must NEVER be recreated, recreateCount=%v at %v", got, now)
		}
	}
}

// Blocker fix #2: a zero-healthy-targets outage (upstream down) does NOT restart
// the pod and does NOT recreate the job, no matter how long it lasts — recreate
// and restart cannot fix "no targets". It DOES look down for the alert path.
func TestSelfHeal_ZeroHealthyTargetsOutage_NeverRestartsOrRecreates(t *testing.T) {
	t0 := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	disc := offlineDiscovery(t0) // single unhealthy target -> zero healthy targets
	c := newStallTestController(t, disc, 15*time.Minute, t0)
	c.config.RecreateBackoff = 5 * time.Minute
	c.config.PodRestartGracePeriod = 45 * time.Minute
	c.config.PodRestartCooldown = 30 * time.Minute
	enabled := true
	c.config.SelfHealPodRestart = &enabled
	c.isLeader = true
	rec := &exitRecorder{}
	c.exit = rec.record
	defer c.stopAllJobs()

	now := t0
	c.now = func() time.Time { return now }

	key := "tenant-a/ma-outage"
	job := &RemoteWriteJob{
		MetricAccess: newMetricAccess("tenant-a", "ma-outage"),
		StopCh:       make(chan struct{}),
		createdAt:    t0,
	}
	c.mu.Lock()
	c.jobs[key] = job
	c.mu.Unlock()

	// 60 * 5m = 5h of continuous outage, far beyond every grace window.
	for i := 1; i <= 60; i++ {
		now = t0.Add(time.Duration(i) * 5 * time.Minute)
		c.collectMetrics(job) // zero-healthy-targets FAILURE branch
		c.evaluateSelfHeal(context.Background(), now)
	}

	if len(rec.codes) != 0 {
		t.Fatalf("a zero-healthy-targets outage must NEVER restart the pod, exits=%v", rec.codes)
	}
	got := c.GetActiveJobs()[key]
	if got == nil {
		t.Fatalf("job should still exist after the outage")
	}
	if got.recreateCount != 0 {
		t.Fatalf("a zero-healthy-targets outage must NEVER recreate, recreateCount=%d", got.recreateCount)
	}
	if got.lastCollectionSucceeded {
		t.Fatalf("expected collection_up=0 during an outage (alert path still fires)")
	}
}

// Blocker fix #3: a genuinely FAILING job (healthy targets present, send fails)
// DOES recreate and, with escalation explicitly enabled, DOES restart the pod
// exactly once after the grace window, reached through the normal watchdog cycle.
func TestSelfHeal_FailingJobWithTargets_RecreatesThenRestartsOnce(t *testing.T) {
	t0 := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	disc, src := healthyDataDiscovery(t)
	defer src.Close()

	c := newStallTestController(t, disc, 15*time.Minute, t0)
	c.config.RecreateBackoff = 5 * time.Minute
	c.config.PodRestartGracePeriod = 45 * time.Minute
	c.config.PodRestartCooldown = 30 * time.Minute
	enabled := true
	c.config.SelfHealPodRestart = &enabled
	c.isLeader = true
	rec := &exitRecorder{}
	c.exit = rec.record
	defer c.stopAllJobs()

	now := t0
	c.now = func() time.Time { return now }

	key := "tenant-a/ma-failing"
	c.mu.Lock()
	c.jobs[key] = &RemoteWriteJob{
		MetricAccess:               failingSinkMetricAccess("tenant-a", "ma-failing"),
		StopCh:                     make(chan struct{}),
		createdAt:                  t0,
		lastCycleHadHealthyTargets: true, // healthy targets present; the SEND fails
	}
	c.mu.Unlock()

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
		t.Fatalf("expected exactly one pod restart for a genuinely failing job, got %d (%v)", len(rec.codes), rec.codes)
	}
	if c.selfRestartTotal != 1 {
		t.Fatalf("expected selfRestartTotal==1, got %d", c.selfRestartTotal)
	}
	got := c.GetActiveJobs()[key]
	if got == nil || got.recreateCount < 1 {
		t.Fatalf("expected at least one in-process recreate before escalation, recreateCount=%v", got)
	}
	if elapsed := now.Sub(t0); elapsed <= 45*time.Minute {
		t.Fatalf("escalation fired after only %v of continuous failure (want > 45m)", elapsed)
	}
}

// Opt-in: with SelfHealPodRestart unset (nil), the last-resort
// pod restart NEVER fires even for a genuinely failing job — but in-process
// recreate still runs by default.
func TestSelfHeal_DefaultConfig_FailingJob_NeverRestartsButRecreates(t *testing.T) {
	t0 := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	disc, src := healthyDataDiscovery(t)
	defer src.Close()

	c := newStallTestController(t, disc, 15*time.Minute, t0)
	c.config.RecreateBackoff = 5 * time.Minute
	c.config.PodRestartGracePeriod = 45 * time.Minute
	c.config.PodRestartCooldown = 30 * time.Minute
	// SelfHealPodRestart left nil: opt-in default OFF.
	c.isLeader = true
	rec := &exitRecorder{}
	c.exit = rec.record
	defer c.stopAllJobs()

	now := t0
	c.now = func() time.Time { return now }

	key := "tenant-a/ma-failing-default"
	c.mu.Lock()
	c.jobs[key] = &RemoteWriteJob{
		MetricAccess:               failingSinkMetricAccess("tenant-a", "ma-failing-default"),
		StopCh:                     make(chan struct{}),
		createdAt:                  t0,
		lastCycleHadHealthyTargets: true,
	}
	c.mu.Unlock()

	for i := 1; i <= 180; i++ {
		now = t0.Add(time.Duration(i) * time.Minute)
		c.evaluateSelfHeal(context.Background(), now)
	}

	if len(rec.codes) != 0 {
		t.Fatalf("with pod-restart opt-in default OFF, a failing job must NEVER restart the pod, exits=%v", rec.codes)
	}
	got := c.GetActiveJobs()[key]
	if got == nil || got.recreateCount < 1 {
		t.Fatalf("in-process recreate must still run by default, recreateCount=%v", got)
	}
}

// The self-heal trigger keys off lastCleanCycleTime, NOT strict success: a job
// whose last STRICT success is ancient but which is still producing clean cycles
// (up-gauge/alert would flag it) must NOT be classified as stalled by the
// watchdog. This is the divergence that prevents the restart loop.
func TestIsJobStalled_FreshCleanCycleButOldSuccess_NotStalled(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	c := newStallTestController(t, &fakeDiscovery{}, 15*time.Minute, now)

	job := &RemoteWriteJob{
		MetricAccess:                 newMetricAccess("tenant-a", "ma-clean"),
		StopCh:                       make(chan struct{}),
		lastSuccessfulCollectionTime: now.Add(-2 * time.Hour), // alert path: looks down
		lastCleanCycleTime:           now.Add(-1 * time.Minute), // self-heal path: fresh
		createdAt:                    now.Add(-3 * time.Hour),
	}

	if c.isJobStalled(job, now) {
		t.Fatalf("self-heal must key off lastCleanCycleTime, not strict success")
	}
}
