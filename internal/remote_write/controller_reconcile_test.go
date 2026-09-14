package remote_write

// Guarantee runtime reconcile-on-add of remote-write
// collection jobs, and prove the full lifecycle (create / enable / restart /
// delete / disable) is handled by a single direct call to the unexported
// reconcileRemoteWriteJobs — no sleeping on the 30s ticker.
//
// The create-after-start path and the hermetic harness (fakeDiscovery,
// newTestController, newMetricAccess, keysOf) are
// reused here. This file hardens that behavior into explicit per-transition
// regression tests and adds the stop-path guarantees the shipped 0.0.8 image
// lacked: on delete/disable the job is removed from c.jobs AND its StopCh is
// closed (so the runRemoteWriteJob goroutine exits — no leak), and on a
// metric-set change the job is genuinely restarted (fresh StopCh, old one
// closed).
//
// go.uber.org/goleak is not a dependency, so the no-leak guarantee is asserted
// structurally: the job is gone from the map and its StopCh — the only signal
// runRemoteWriteJob selects on to return — is closed.

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/types"

	"github.com/prometheus-multi-tenant-proxy/api/v1alpha1"
)

// isClosed reports whether ch has been closed, without blocking.
func isClosed(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// disabledMetricAccess returns a remote-write CR with collection disabled.
func disabledMetricAccess(namespace, name string) *v1alpha1.MetricAccess {
	ma := newMetricAccess(namespace, name)
	ma.Spec.RemoteWrite.Enabled = false
	return ma
}

// ---------------------------------------------------------------------------
// Create-after-start. A CR that appears after the controller is
// started gets a collection job within one reconcile interval (one direct
// reconcile call == one 30s tick).
// ---------------------------------------------------------------------------

func TestReconcile_CreateAfterStart_CreatesJob(t *testing.T) {
	disc := &fakeDiscovery{} // no healthy targets -> collection stays offline
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

	ma := newMetricAccess("tenant-a", "ma-create")
	if err := c.client.Create(ctx, ma); err != nil {
		t.Fatalf("failed to create MetricAccess: %v", err)
	}

	// One reconcile == one interval: a job must exist afterward.
	if err := c.reconcileRemoteWriteJobs(ctx); err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	jobs := c.GetActiveJobs()
	if _, ok := jobs["tenant-a/ma-create"]; !ok {
		t.Fatalf("expected a collection job for create-after-start CR; jobs=%v", keysOf(jobs))
	}
}

// ---------------------------------------------------------------------------
// Enable-after-start. A CR that existed while disabled gets no job;
// flipping remoteWrite.enabled=true is reconciled into a new job.
// ---------------------------------------------------------------------------

func TestReconcile_EnableAfterStart_CreatesJob(t *testing.T) {
	ctx := context.Background()
	disc := &fakeDiscovery{}
	// Seed the cluster with a DISABLED CR.
	c := newTestController(t, disc, disabledMetricAccess("tenant-b", "ma-enable"))

	// A disabled CR must not produce a job.
	if err := c.reconcileRemoteWriteJobs(ctx); err != nil {
		t.Fatalf("reconcile (disabled) failed: %v", err)
	}
	if got := len(c.GetActiveJobs()); got != 0 {
		t.Fatalf("expected 0 jobs for disabled CR, got %d (%v)", got, keysOf(c.GetActiveJobs()))
	}

	// Enable remote write on the existing CR.
	key := types.NamespacedName{Namespace: "tenant-b", Name: "ma-enable"}
	var fetched v1alpha1.MetricAccess
	if err := c.client.Get(ctx, key, &fetched); err != nil {
		t.Fatalf("failed to get CR: %v", err)
	}
	fetched.Spec.RemoteWrite.Enabled = true
	if err := c.client.Update(ctx, &fetched); err != nil {
		t.Fatalf("failed to enable CR: %v", err)
	}

	// Next reconcile must create the job.
	if err := c.reconcileRemoteWriteJobs(ctx); err != nil {
		t.Fatalf("reconcile (enabled) failed: %v", err)
	}
	if _, ok := c.GetActiveJobs()["tenant-b/ma-enable"]; !ok {
		t.Fatalf("expected a job after enabling remoteWrite; jobs=%v", keysOf(c.GetActiveJobs()))
	}
}

// ---------------------------------------------------------------------------
// Metric-set change restarts the job. hasConfigurationChanged must
// fire, the old job's StopCh must close (its goroutine returns), and a fresh
// job (new StopCh) must take its place with the updated metric set.
// ---------------------------------------------------------------------------

func TestReconcile_MetricSetChange_RestartsJob(t *testing.T) {
	ctx := context.Background()
	disc := &fakeDiscovery{}
	c := newTestController(t, disc, newMetricAccess("tenant-c", "ma-restart"))

	if err := c.reconcileRemoteWriteJobs(ctx); err != nil {
		t.Fatalf("initial reconcile failed: %v", err)
	}
	oldJob, ok := c.GetActiveJobs()["tenant-c/ma-restart"]
	if !ok {
		t.Fatalf("expected initial job to exist; jobs=%v", keysOf(c.GetActiveJobs()))
	}
	oldStopCh := oldJob.StopCh

	// Change the metric set (add a metric) so hasConfigurationChanged fires.
	key := types.NamespacedName{Namespace: "tenant-c", Name: "ma-restart"}
	var fetched v1alpha1.MetricAccess
	if err := c.client.Get(ctx, key, &fetched); err != nil {
		t.Fatalf("failed to get CR: %v", err)
	}
	fetched.Spec.Metrics = append(fetched.Spec.Metrics, "container_memory_usage_bytes")
	if err := c.client.Update(ctx, &fetched); err != nil {
		t.Fatalf("failed to update metric set: %v", err)
	}

	if err := c.reconcileRemoteWriteJobs(ctx); err != nil {
		t.Fatalf("reconcile after metric change failed: %v", err)
	}

	newJob, ok := c.GetActiveJobs()["tenant-c/ma-restart"]
	if !ok {
		t.Fatalf("expected job to still exist after restart; jobs=%v", keysOf(c.GetActiveJobs()))
	}

	// Restart semantics: old goroutine signalled to stop, new job is a distinct
	// object carrying the new metric set.
	if !isClosed(oldStopCh) {
		t.Fatalf("expected old job's StopCh to be closed on restart (goroutine leak otherwise)")
	}
	if newJob.StopCh == oldStopCh {
		t.Fatalf("expected a fresh StopCh after restart; got the same channel")
	}
	if len(newJob.MetricAccess.Spec.Metrics) != 2 {
		t.Fatalf("expected restarted job to carry the new 2-metric set, got %d: %v",
			len(newJob.MetricAccess.Spec.Metrics), newJob.MetricAccess.Spec.Metrics)
	}
}

// ---------------------------------------------------------------------------
// Deleting the CR stops and removes the job. Job gone from c.jobs
// AND its StopCh closed (no goroutine leak).
// ---------------------------------------------------------------------------

func TestReconcile_DeleteCR_StopsAndRemovesJob(t *testing.T) {
	ctx := context.Background()
	disc := &fakeDiscovery{}
	ma := newMetricAccess("tenant-d", "ma-delete")
	c := newTestController(t, disc, ma)

	if err := c.reconcileRemoteWriteJobs(ctx); err != nil {
		t.Fatalf("initial reconcile failed: %v", err)
	}
	job, ok := c.GetActiveJobs()["tenant-d/ma-delete"]
	if !ok {
		t.Fatalf("expected job to exist before delete; jobs=%v", keysOf(c.GetActiveJobs()))
	}
	stopCh := job.StopCh

	if err := c.client.Delete(ctx, ma); err != nil {
		t.Fatalf("failed to delete CR: %v", err)
	}

	if err := c.reconcileRemoteWriteJobs(ctx); err != nil {
		t.Fatalf("reconcile after delete failed: %v", err)
	}

	if _, ok := c.GetActiveJobs()["tenant-d/ma-delete"]; ok {
		t.Fatalf("expected job removed from c.jobs after CR delete; jobs=%v", keysOf(c.GetActiveJobs()))
	}
	if !isClosed(stopCh) {
		t.Fatalf("expected StopCh closed after CR delete (goroutine leak otherwise)")
	}
}

// ---------------------------------------------------------------------------
// Disabling the CR (remoteWrite.enabled=false) stops and removes the
// job. Job gone from c.jobs AND its StopCh closed (no goroutine leak).
// ---------------------------------------------------------------------------

func TestReconcile_DisableCR_StopsAndRemovesJob(t *testing.T) {
	ctx := context.Background()
	disc := &fakeDiscovery{}
	c := newTestController(t, disc, newMetricAccess("tenant-e", "ma-disable"))

	if err := c.reconcileRemoteWriteJobs(ctx); err != nil {
		t.Fatalf("initial reconcile failed: %v", err)
	}
	job, ok := c.GetActiveJobs()["tenant-e/ma-disable"]
	if !ok {
		t.Fatalf("expected job to exist before disable; jobs=%v", keysOf(c.GetActiveJobs()))
	}
	stopCh := job.StopCh

	// Disable remote write on the CR.
	key := types.NamespacedName{Namespace: "tenant-e", Name: "ma-disable"}
	var fetched v1alpha1.MetricAccess
	if err := c.client.Get(ctx, key, &fetched); err != nil {
		t.Fatalf("failed to get CR: %v", err)
	}
	fetched.Spec.RemoteWrite.Enabled = false
	if err := c.client.Update(ctx, &fetched); err != nil {
		t.Fatalf("failed to disable CR: %v", err)
	}

	if err := c.reconcileRemoteWriteJobs(ctx); err != nil {
		t.Fatalf("reconcile after disable failed: %v", err)
	}

	if _, ok := c.GetActiveJobs()["tenant-e/ma-disable"]; ok {
		t.Fatalf("expected job removed from c.jobs after disable; jobs=%v", keysOf(c.GetActiveJobs()))
	}
	if !isClosed(stopCh) {
		t.Fatalf("expected StopCh closed after disable (goroutine leak otherwise)")
	}
}
