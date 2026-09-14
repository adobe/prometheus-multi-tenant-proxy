package remote_write

// Detect a silent per-tenant collection stall.
//
// These tests are hermetic and drive the controller directly. Time is made
// testable via the controller's injectable clock (c.now) and by setting the
// job's lastSuccessfulCollectionTime in the past — no real 15m sleeps.
//
// What each scenario asserts:
//   - A job whose last success is older than the stall grace period is stalled.
//   - A job that just succeeded is not stalled.
//   - A never-succeeded job is judged against its creation time baseline.
//   - collectMetrics records a SUCCESS (lastSuccessfulCollectionTime advances,
//     consecutiveEmptyCollections resets) only when >=1 series is sent with no
//     error; the zero-target branch records an empty cycle instead.

import (
	"testing"
	"time"

	"github.com/prometheus-multi-tenant-proxy/internal/config"
	"github.com/prometheus-multi-tenant-proxy/internal/discovery"
)

// newStallTestController builds a controller with a fixed injectable clock and
// an explicit stall grace period.
func newStallTestController(t *testing.T, disc discovery.Discovery, grace time.Duration, now time.Time) *Controller {
	t.Helper()
	c := newTestController(t, disc)
	c.config.StallGracePeriod = grace
	c.now = func() time.Time { return now }
	return c
}

func TestIsJobStalled_LastCleanCycleBeyondGracePeriod_ReturnsTrue(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	c := newStallTestController(t, &fakeDiscovery{}, 15*time.Minute, now)

	// The self-heal trigger is FAILURE-based: it keys off lastCleanCycleTime (the
	// last error-free reachable cycle), not strict success.
	job := &RemoteWriteJob{
		MetricAccess:       newMetricAccess("tenant-a", "ma-stalled"),
		StopCh:             make(chan struct{}),
		lastCleanCycleTime: now.Add(-20 * time.Minute), // beyond 15m grace
		createdAt:          now.Add(-1 * time.Hour),
	}

	if !c.isJobStalled(job, now) {
		t.Fatalf("expected job with last clean cycle 20m ago (grace 15m) to be stalled")
	}
}

func TestIsJobStalled_RecentCleanCycle_ReturnsFalse(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	c := newStallTestController(t, &fakeDiscovery{}, 15*time.Minute, now)

	job := &RemoteWriteJob{
		MetricAccess:       newMetricAccess("tenant-a", "ma-fresh"),
		StopCh:             make(chan struct{}),
		lastCleanCycleTime: now.Add(-1 * time.Minute), // well within grace
		createdAt:          now.Add(-1 * time.Hour),
	}

	if c.isJobStalled(job, now) {
		t.Fatalf("expected a job with a recent clean cycle to not be stalled")
	}
}

func TestIsJobStalled_NeverSucceeded_UsesCreationBaseline(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	c := newStallTestController(t, &fakeDiscovery{}, 15*time.Minute, now)

	// Never succeeded, created long ago -> stalled against creation baseline.
	oldJob := &RemoteWriteJob{
		MetricAccess: newMetricAccess("tenant-a", "ma-never-old"),
		StopCh:       make(chan struct{}),
		createdAt:    now.Add(-30 * time.Minute),
	}
	if !c.isJobStalled(oldJob, now) {
		t.Fatalf("expected never-succeeded job created 30m ago to be stalled (creation baseline)")
	}

	// Never succeeded, just created -> not yet stalled.
	freshJob := &RemoteWriteJob{
		MetricAccess: newMetricAccess("tenant-a", "ma-never-fresh"),
		StopCh:       make(chan struct{}),
		createdAt:    now.Add(-1 * time.Minute),
	}
	if c.isJobStalled(freshJob, now) {
		t.Fatalf("expected never-succeeded job created 1m ago to not be stalled yet")
	}
}

func TestIsJobStalled_DisabledCR_ReturnsFalse(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	c := newStallTestController(t, &fakeDiscovery{}, 15*time.Minute, now)

	ma := newMetricAccess("tenant-a", "ma-disabled")
	ma.Spec.RemoteWrite.Enabled = false
	job := &RemoteWriteJob{
		MetricAccess:                ma,
		StopCh:                      make(chan struct{}),
		lastSuccessfulCollectionTime: now.Add(-1 * time.Hour),
		createdAt:                   now.Add(-2 * time.Hour),
	}

	if c.isJobStalled(job, now) {
		t.Fatalf("expected a disabled CR to never be classified as stalled")
	}
}

func TestStallGracePeriod_DefaultsWhenUnset(t *testing.T) {
	c := newTestController(t, &fakeDiscovery{}) // config has no StallGracePeriod set
	if got := c.stallGracePeriod(); got != 15*time.Minute {
		t.Fatalf("expected default stall grace period of 15m, got %v", got)
	}

	c.config.StallGracePeriod = 5 * time.Minute
	if got := c.stallGracePeriod(); got != 5*time.Minute {
		t.Fatalf("expected configured stall grace period of 5m, got %v", got)
	}
}

// collectMetrics on the zero-healthy-targets branch must record an empty cycle
// (bump consecutiveEmptyCollections) and must NOT advance the success clock.
func TestCollectMetrics_ZeroTargets_RecordsEmptyCycleNotSuccess(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	disc := &fakeDiscovery{}
	disc.setTargets([]discovery.Target{
		{URL: "http://10.0.0.1:9090", Healthy: false, LastSeen: now},
	})
	c := newStallTestController(t, disc, 15*time.Minute, now)

	job := &RemoteWriteJob{
		MetricAccess: newMetricAccess("tenant-c", "ma-wedge"),
		StopCh:       make(chan struct{}),
	}

	c.collectMetrics(job)

	if job.consecutiveEmptyCollections != 1 {
		t.Fatalf("expected consecutiveEmptyCollections==1 after a zero-target cycle, got %d", job.consecutiveEmptyCollections)
	}
	if !job.lastSuccessfulCollectionTime.IsZero() {
		t.Fatalf("expected no successful collection recorded on zero-target cycle, got %v", job.lastSuccessfulCollectionTime)
	}
}

// A stalled config used as a regression guard: NewController must default the
// clock seam so real callers keep working.
func TestNewController_HasDefaultClock(t *testing.T) {
	c := NewController(nil, config.RemoteWriteConfig{}, &fakeDiscovery{})
	if c.now == nil {
		t.Fatalf("expected NewController to install a default clock seam")
	}
	if delta := time.Since(c.now()); delta < 0 || delta > time.Minute {
		t.Fatalf("expected default clock to track wall time, got delta %v", delta)
	}
}
