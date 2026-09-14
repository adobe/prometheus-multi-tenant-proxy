package remote_write

// Second self-heal action — a LAST-RESORT pod restart.
//
// When in-process recreate keeps failing — the job has already been
// recreated at least once (recreateCount >= 1) yet remains stalled beyond
// podRestartGracePeriod — the watchdog restarts the pod by exiting the process
// with a non-zero code. The restart is verified WITHOUT killing the test
// process via an injectable exit seam (Controller.exit), and the multi-minute
// timing is driven by the controller's injectable clock (no real sleeps).
//
// What each scenario asserts:
//   - Escalate-only-after-recreate-failed: no restart when recreateCount == 0,
//     and no restart while still within podRestartGracePeriod (in-process
//     recreate runs instead).
//   - Leader-only / flag-disabled: escalation does NOT exit when this replica is
//     not the leader, nor when --self-heal-pod-restart is disabled.
//   - Throttle: a second escalation inside podRestartCooldown does NOT exit
//     again; a pass after the cooldown does.
//   - Happy path: stalled + recreated + beyond-grace + enabled + leader -> the
//     exit seam is called exactly once with a non-zero code.

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus-multi-tenant-proxy/internal/discovery"
)

// exitRecorder is a fake process-exit seam. Unlike os.Exit it returns, so the
// code after Controller.exit(1) is exercised by the tests.
type exitRecorder struct {
	codes []int
}

func (e *exitRecorder) record(code int) { e.codes = append(e.codes, code) }

// offlineDiscovery returns a discovery whose single target is unhealthy, so any
// recreate path taken by a negative test collects from zero healthy targets and
// never touches the network.
func offlineDiscovery(at time.Time) *fakeDiscovery {
	disc := &fakeDiscovery{}
	disc.setTargets([]discovery.Target{
		{URL: "http://10.0.0.1:9090", Healthy: false, LastSeen: at},
	})
	return disc
}

// newRestartTestController builds a stall-test controller wired for pod restart:
// leader, pod-restart escalation EXPLICITLY enabled (it is opt-in and ships off
// by default), and an injectable exit recorder.
func newRestartTestController(t *testing.T, at time.Time) (*Controller, *exitRecorder) {
	t.Helper()
	c := newStallTestController(t, offlineDiscovery(at), 1*time.Minute, at)
	c.config.RecreateBackoff = 5 * time.Minute
	c.config.PodRestartGracePeriod = 45 * time.Minute
	c.config.PodRestartCooldown = 30 * time.Minute
	enabled := true
	c.config.SelfHealPodRestart = &enabled // opt-in: must be enabled explicitly now
	c.isLeader = true
	rec := &exitRecorder{}
	c.exit = rec.record
	return c, rec
}

// stalledRecreatedJob is a genuinely-failing job that never had a clean cycle, has
// been continuously stalled since createdAt (stalledSince == createdAt), has
// already been recreated recreateCount times, and — crucially for the review-fixed
// model — saw healthy targets on its last cycle (lastCycleHadHealthyTargets) so a
// send/collection failure, not a target outage, is what keeps it wedged. The
// escalation window is measured from stalledSince, so tests that drive
// evaluateSelfHeal directly set it in the past.
func stalledRecreatedJob(name string, createdAt time.Time, recreateCount int) *RemoteWriteJob {
	return &RemoteWriteJob{
		MetricAccess:               newMetricAccess("tenant-a", name),
		StopCh:                     make(chan struct{}),
		createdAt:                  createdAt,
		stalledSince:               createdAt,
		recreateCount:              recreateCount,
		lastCycleHadHealthyTargets: true,
	}
}

// Happy path — stalled + recreated + beyond grace + enabled + leader ->
// exit seam called exactly once with a non-zero code, counter incremented.
func TestSelfHeal_PodRestart_HappyPath_ExitsOnce(t *testing.T) {
	t0 := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	c, rec := newRestartTestController(t, t0)
	defer c.stopAllJobs()

	key := "tenant-a/ma-restart"
	c.mu.Lock()
	c.jobs[key] = stalledRecreatedJob("ma-restart", t0.Add(-50*time.Minute), 1)
	c.mu.Unlock()

	c.evaluateSelfHeal(context.Background(), t0)

	if len(rec.codes) != 1 {
		t.Fatalf("expected exactly one self-restart exit, got %d (%v)", len(rec.codes), rec.codes)
	}
	if rec.codes[0] == 0 {
		t.Fatalf("expected a NON-ZERO exit code to trigger a pod restart, got %d", rec.codes[0])
	}
	if c.selfRestartTotal != 1 {
		t.Fatalf("expected selfRestartTotal==1 after escalation, got %d", c.selfRestartTotal)
	}
	if !c.lastSelfRestartAt.Equal(t0) {
		t.Fatalf("expected lastSelfRestartAt==%v, got %v", t0, c.lastSelfRestartAt)
	}
}

// No restart when recreate has not been attempted yet (recreateCount==0).
func TestSelfHeal_PodRestart_NoEscalationWhenNeverRecreated(t *testing.T) {
	t0 := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	c, rec := newRestartTestController(t, t0)
	defer c.stopAllJobs()

	c.mu.Lock()
	// Stalled 50m (> grace) but recreateCount == 0: recreate must run, not restart.
	c.jobs["tenant-a/ma-fresh"] = stalledRecreatedJob("ma-fresh", t0.Add(-50*time.Minute), 0)
	c.mu.Unlock()

	c.evaluateSelfHeal(context.Background(), t0)

	if len(rec.codes) != 0 {
		t.Fatalf("expected NO self-restart when recreateCount==0, got exits %v", rec.codes)
	}
}

// No restart while still within podRestartGracePeriod even though the
// job has been recreated and is stalled.
func TestSelfHeal_PodRestart_NoEscalationWithinGrace(t *testing.T) {
	t0 := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	c, rec := newRestartTestController(t, t0)
	defer c.stopAllJobs()

	c.mu.Lock()
	// Stalled 10m: past the 1m stall grace but well within the 45m restart grace.
	c.jobs["tenant-a/ma-young"] = stalledRecreatedJob("ma-young", t0.Add(-10*time.Minute), 2)
	c.mu.Unlock()

	c.evaluateSelfHeal(context.Background(), t0)

	if len(rec.codes) != 0 {
		t.Fatalf("expected NO self-restart within podRestartGracePeriod, got exits %v", rec.codes)
	}
}

// Not the leader -> escalation must NOT exit even when all other
// conditions are met.
func TestSelfHeal_PodRestart_NonLeaderDoesNotExit(t *testing.T) {
	t0 := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	c, rec := newRestartTestController(t, t0)
	c.isLeader = false // standby replica
	defer c.stopAllJobs()

	c.mu.Lock()
	c.jobs["tenant-a/ma-standby"] = stalledRecreatedJob("ma-standby", t0.Add(-50*time.Minute), 1)
	c.mu.Unlock()

	c.evaluateSelfHeal(context.Background(), t0)

	if len(rec.codes) != 0 {
		t.Fatalf("expected NO self-restart when not the leader, got exits %v", rec.codes)
	}
}

// Flag disabled -> escalation must NOT exit.
func TestSelfHeal_PodRestart_FlagDisabledDoesNotExit(t *testing.T) {
	t0 := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	c, rec := newRestartTestController(t, t0)
	disabled := false
	c.config.SelfHealPodRestart = &disabled
	defer c.stopAllJobs()

	c.mu.Lock()
	c.jobs["tenant-a/ma-off"] = stalledRecreatedJob("ma-off", t0.Add(-50*time.Minute), 1)
	c.mu.Unlock()

	c.evaluateSelfHeal(context.Background(), t0)

	if len(rec.codes) != 0 {
		t.Fatalf("expected NO self-restart when --self-heal-pod-restart=false, got exits %v", rec.codes)
	}
}

// Throttle — a second escalation inside podRestartCooldown does NOT exit
// again; a pass after the cooldown window does.
func TestSelfHeal_PodRestart_ThrottledByCooldown(t *testing.T) {
	t0 := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	c, rec := newRestartTestController(t, t0)
	defer c.stopAllJobs()

	key := "tenant-a/ma-throttle"
	c.mu.Lock()
	// Never recovers: created 50m before t0, escalation-eligible for the whole run.
	c.jobs[key] = stalledRecreatedJob("ma-throttle", t0.Add(-50*time.Minute), 1)
	c.mu.Unlock()

	// First pass: exit.
	c.evaluateSelfHeal(context.Background(), t0)
	if len(rec.codes) != 1 {
		t.Fatalf("expected one exit on first escalation, got %v", rec.codes)
	}

	// Second pass 10m later (< 30m cooldown): still stalled/eligible but throttled.
	c.evaluateSelfHeal(context.Background(), t0.Add(10*time.Minute))
	if len(rec.codes) != 1 {
		t.Fatalf("expected NO second exit inside cooldown window, got %v", rec.codes)
	}
	if c.selfRestartTotal != 1 {
		t.Fatalf("expected selfRestartTotal to stay 1 inside cooldown, got %d", c.selfRestartTotal)
	}

	// Third pass 40m after the first (> 30m cooldown): escalate again.
	c.evaluateSelfHeal(context.Background(), t0.Add(40*time.Minute))
	if len(rec.codes) != 2 {
		t.Fatalf("expected a second exit after the cooldown elapsed, got %v", rec.codes)
	}
	if c.selfRestartTotal != 2 {
		t.Fatalf("expected selfRestartTotal==2 after cooldown elapsed, got %d", c.selfRestartTotal)
	}
}

// Accessor defaults mirror stallGracePeriod / recreateBackoff.
func TestPodRestart_DefaultsWhenUnset(t *testing.T) {
	c := newTestController(t, &fakeDiscovery{}) // no pod-restart config set

	if got := c.podRestartGracePeriod(); got != 45*time.Minute {
		t.Fatalf("expected default pod restart grace of 45m, got %v", got)
	}
	if got := c.podRestartCooldown(); got != 30*time.Minute {
		t.Fatalf("expected default pod restart cooldown of 30m, got %v", got)
	}
	// SelfHealPodRestart is OPT-IN: a nil (unset) config value means DISABLED.
	if c.selfHealPodRestartEnabled() {
		t.Fatalf("expected pod-restart self-heal to be DISABLED (opt-in) by default")
	}
	// Explicitly enabling it flips the accessor.
	enabled := true
	c.config.SelfHealPodRestart = &enabled
	if !c.selfHealPodRestartEnabled() {
		t.Fatalf("expected pod-restart self-heal to be enabled when set true")
	}

	c.config.PodRestartGracePeriod = 90 * time.Second
	c.config.PodRestartCooldown = 2 * time.Minute
	if got := c.podRestartGracePeriod(); got != 90*time.Second {
		t.Fatalf("expected configured grace 90s, got %v", got)
	}
	if got := c.podRestartCooldown(); got != 2*time.Minute {
		t.Fatalf("expected configured cooldown 2m, got %v", got)
	}
}

// The default exit seam is os.Exit (verified indirectly: NewController must set a
// non-nil exit so production restarts actually terminate the process).
func TestNewController_SetsExitSeam(t *testing.T) {
	c := newTestController(t, &fakeDiscovery{})
	if c.exit == nil {
		t.Fatalf("expected NewController to install a non-nil exit seam (os.Exit)")
	}
}

// REACHABILITY regression (the key correctness test): with realistic config the
// escalation must actually fire through the NORMAL watchdog cycle — not only when
// job state is set directly. The in-process recreate resets per-job createdAt every
// ~stallGracePeriod, so the escalation window is tracked by a PERSISTENT
// stalledSince that survives recreates and clears only on a real clean cycle.
// Under the review-fixed model the wedge must be a GENUINE failure with healthy
// targets present (send keeps failing), not a target outage: here a brand-new job
// with healthy targets never sends successfully; the watchdog recreates it
// repeatedly and must escalate exactly once after >45m of continuous failure.
func TestSelfHeal_PodRestart_ReachableAcrossRecreates_NormalCycle(t *testing.T) {
	t0 := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

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

	// A brand-new job created at t0 whose send always fails (healthy targets, nil
	// endpoint). No escalation state is pre-set beyond marking that healthy targets
	// were seen — it must accrue through the normal cycle.
	key := "tenant-a/ma-wedged"
	c.mu.Lock()
	c.jobs[key] = &RemoteWriteJob{
		MetricAccess:               failingSinkMetricAccess("tenant-a", "ma-wedged"),
		StopCh:                     make(chan struct{}),
		createdAt:                  t0,
		lastCycleHadHealthyTargets: true,
	}
	c.mu.Unlock()

	// Drive the reconcile watchdog once per minute, advancing the clock, until it
	// escalates (or 3h elapses as a safety bound).
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
		t.Fatalf("expected escalation to fire exactly once through the normal recreate cycle, got %d exits (%v)", len(rec.codes), rec.codes)
	}
	if c.selfRestartTotal != 1 {
		t.Fatalf("expected selfRestartTotal==1, got %d", c.selfRestartTotal)
	}
	// It must have escalated only after multiple in-process recreates...
	if got := c.GetActiveJobs()[key]; got == nil || got.recreateCount < 2 {
		t.Fatalf("expected multiple recreates before escalation, recreateCount=%v", got)
	}
	// ...and only after >45m of continuous stall.
	if elapsed := now.Sub(t0); elapsed <= 45*time.Minute {
		t.Fatalf("escalation fired after only %v of continuous stall (want > 45m)", elapsed)
	}
}
