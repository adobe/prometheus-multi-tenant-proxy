package remote_write

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/common/model"
	"github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/prometheus-multi-tenant-proxy/api/v1alpha1"
	"github.com/prometheus-multi-tenant-proxy/internal/config"
	"github.com/prometheus-multi-tenant-proxy/internal/discovery"
)

// PrometheusResponse represents the response from a Prometheus query
type PrometheusResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Value  []interface{}     `json:"value"`
		} `json:"result"`
	} `json:"data"`
}

// Controller manages metric remote write for tenants
type Controller struct {
	client           client.Client
	config           config.RemoteWriteConfig
	serviceDiscovery discovery.Discovery

	// Active remote write jobs
	mu   sync.RWMutex
	jobs map[string]*RemoteWriteJob // namespace/name -> job

	// In-memory store for latest collected metrics per job
	collectedMetrics map[string][]Metric // namespace/name -> []Metric

	// Control channels
	stopCh chan struct{}
	doneCh chan struct{}

	// now is an injectable clock seam. It defaults to time.Now in NewController
	// and is overridden in tests to make stall detection deterministic without
	// real sleeps.
	now func() time.Time

	// exit is an injectable process-exit seam used by the last-resort
	// self-heal to restart the pod. It defaults to os.Exit in NewController and
	// is overridden in tests to record the code instead of terminating the test
	// process.
	exit func(int)

	// isLeader records that this controller is running as the remote-write
	// leader. It is set true by reconcileLoop, which main.go starts ONLY from
	// the leader-election OnStartedLeading callback — so reaching the self-heal
	// pod restart is structurally leader-only. This field is an
	// explicit, testable second gate on the escalation and MUST NOT be used to
	// weaken that structural guarantee. Guarded by mu.
	isLeader bool

	// Self-heal pod-restart escalation state. Guarded by
	// mu.
	//
	// lastSelfRestartAt is the controller-clock time of the last self-restart
	// attempt; the zero value means never. It throttles the escalation to at
	// most one attempt per podRestartCooldown.
	lastSelfRestartAt time.Time
	// selfRestartTotal counts last-resort self-restart escalations triggered by
	// the watchdog. Exported as a metric by the health collector.
	selfRestartTotal int64
}

// defaultStallGracePeriod is used when RemoteWriteConfig.StallGracePeriod is
// unset or non-positive.
const defaultStallGracePeriod = 15 * time.Minute

// defaultRecreateBackoff is used when RemoteWriteConfig.RecreateBackoff is unset
// or non-positive.
const defaultRecreateBackoff = 5 * time.Minute

// defaultPodRestartGracePeriod is used when
// RemoteWriteConfig.PodRestartGracePeriod is unset or non-positive.
const defaultPodRestartGracePeriod = 45 * time.Minute

// defaultPodRestartCooldown is used when RemoteWriteConfig.PodRestartCooldown is
// unset or non-positive.
const defaultPodRestartCooldown = 30 * time.Minute

// RemoteWriteJob represents a remote write job for a tenant
type RemoteWriteJob struct {
	MetricAccess *v1alpha1.MetricAccess
	StopCh       chan struct{}
	
	// Metrics for monitoring
	LastRun       time.Time
	LastError     error
	MetricsCount  int64
	SuccessCount  int64
	FailureCount  int64

	// Collection health tracking. A collection SUCCESS is
	// (>=1 series stored) AND (remote-write send returned no error). These fields
	// are guarded by Controller.mu.
	//
	// lastSuccessfulCollectionTime is the controller-clock time of the last
	// SUCCESS; zero if the job has never succeeded.
	lastSuccessfulCollectionTime time.Time
	// lastCollectedSeriesCount is the series count of the last SUCCESS.
	lastCollectedSeriesCount int
	// consecutiveEmptyCollections counts consecutive non-successful cycles
	// (zero healthy targets, zero series after collection, or a send error).
	// Reset to 0 on a SUCCESS.
	consecutiveEmptyCollections int
	// lastCollectionSucceeded records the outcome of the MOST RECENT collection
	// cycle: true iff that cycle stored >=1 series AND the remote-write send
	// returned no error. It is the source for the
	// proxy_tenant_collection_up gauge and the stall ALERT, and is set on every
	// cycle branch of collectMetrics (empty/zero/send-error -> false, success ->
	// true). This stays STRICT success — it is deliberately NOT the self-heal
	// trigger (see lastCleanCycleTime).
	lastCollectionSucceeded bool
	// lastCleanCycleTime is the controller-clock time of the last ERROR-FREE cycle
	// in which healthy targets were reachable: either a SUCCESS (>=1 series sent
	// with no error) or a CLEAN-EMPTY cycle (healthy targets queried WITHOUT error
	// but 0 series returned — legitimately nothing to collect). It is the self-heal
	// stall baseline: a cleanly-empty tenant keeps
	// this current and is therefore NEVER classified as stalled by the watchdog, so
	// it is neither recreated nor restarted. Distinct from
	// lastSuccessfulCollectionTime, which is STRICT success and drives the
	// up-gauge/alert. Guarded by Controller.mu.
	lastCleanCycleTime time.Time
	// lastCycleHadHealthyTargets records whether the MOST RECENT collection cycle
	// saw at least one healthy target. The self-heal escalations gate on this: an
	// upstream outage (zero healthy targets) must NOT drive an in-process recreate
	// or a pod restart, because neither can fix "no targets". It is carried forward
	// across recreates alongside stalledSince. Guarded by Controller.mu.
	lastCycleHadHealthyTargets bool
	// createdAt is the controller-clock time the job was created; used as the
	// stall baseline for a job that has never had a clean cycle.
	createdAt time.Time

	// Self-heal recreate throttle. Guarded by
	// Controller.mu.
	//
	// lastRecreateAt is the controller-clock time this job (across recreates)
	// was last recreated by the watchdog; zero if never recreated. It is the
	// baseline for the recreateBackoff rate limit and is carried forward onto the
	// fresh job struct at recreate time. A successful collection clears it so a
	// future stall is handled fresh.
	lastRecreateAt time.Time
	// recreateCount is a lifetime counter of how many times this logical job has
	// been recreated by the watchdog. It is carried forward across recreates and
	// is intentionally NOT reset on a successful collection.
	recreateCount int

	// stalledSince is the controller-clock time this logical job was first
	// observed continuously stalled. Unlike createdAt —
	// which the recreate step resets on every recreate — stalledSince PERSISTS ACROSS
	// RECREATES (carried forward onto the fresh job struct alongside
	// recreateCount) and is cleared only by a genuinely successful collection.
	// It is therefore the correct baseline for the pod-restart escalation window:
	// it measures how long the job has stayed wedged despite repeated in-process
	// recreates. Zero means the job is not currently known to be stalled.
	// Guarded by Controller.mu.
	stalledSince time.Time
}

// NewController creates a new remote write controller
func NewController(client client.Client, cfg config.RemoteWriteConfig, serviceDiscovery discovery.Discovery) *Controller {
	return &Controller{
		client:           client,
		config:           cfg,
		serviceDiscovery: serviceDiscovery,
		jobs:             make(map[string]*RemoteWriteJob),
		collectedMetrics:  make(map[string][]Metric),
		stopCh:           make(chan struct{}),
		doneCh:           make(chan struct{}),
		now:              time.Now,
		exit:             os.Exit,
	}
}

// stallGracePeriod returns the configured stall grace period, falling back to
// defaultStallGracePeriod when unset. Callers must not hardcode the default.
func (c *Controller) stallGracePeriod() time.Duration {
	if c.config.StallGracePeriod > 0 {
		return c.config.StallGracePeriod
	}
	return defaultStallGracePeriod
}

// recreateBackoff returns the configured self-heal recreate backoff, falling
// back to defaultRecreateBackoff when unset. Callers must not hardcode the
// default.
func (c *Controller) recreateBackoff() time.Duration {
	if c.config.RecreateBackoff > 0 {
		return c.config.RecreateBackoff
	}
	return defaultRecreateBackoff
}

// podRestartGracePeriod returns the configured pod-restart grace period,
// falling back to defaultPodRestartGracePeriod when unset. Mirrors
// stallGracePeriod/recreateBackoff.
func (c *Controller) podRestartGracePeriod() time.Duration {
	if c.config.PodRestartGracePeriod > 0 {
		return c.config.PodRestartGracePeriod
	}
	return defaultPodRestartGracePeriod
}

// podRestartCooldown returns the configured pod-restart cooldown, falling
// back to defaultPodRestartCooldown when unset.
func (c *Controller) podRestartCooldown() time.Duration {
	if c.config.PodRestartCooldown > 0 {
		return c.config.PodRestartCooldown
	}
	return defaultPodRestartCooldown
}

// selfHealPodRestartEnabled reports whether the last-resort pod restart
// is enabled. It is OPT-IN: a nil (unset) config value
// is treated as FALSE, so the escalation ships DISABLED unless setDefaults or the
// --self-heal-pod-restart flag explicitly enables it. Stall detection and
// in-process recreate are unaffected and remain on by default.
func (c *Controller) selfHealPodRestartEnabled() bool {
	if c.config.SelfHealPodRestart == nil {
		return false
	}
	return *c.config.SelfHealPodRestart
}

// isJobStalled reports whether an enabled collection job has gone without an
// error-free (clean) collection cycle for longer than the stall grace period.
// The trigger is FAILURE-based: it keys off
// lastCleanCycleTime — refreshed on both a strict SUCCESS and a CLEAN-EMPTY cycle
// (healthy targets, no error, 0 series) — NOT off strict success. A tenant that
// legitimately has nothing to collect keeps this baseline current and is
// therefore NEVER classified as stalled. The recovery
// action lives in evaluateSelfHeal. NOTE: this is deliberately
// distinct from lastCollectionSucceeded, which stays STRICT success for the
// up-gauge / stall alert.
func (c *Controller) isJobStalled(job *RemoteWriteJob, now time.Time) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.isJobStalledLocked(job, now)
}

// isJobStalledLocked is the lock-free core of isJobStalled. Callers must hold
// c.mu (read or write). Extracted so the self-heal watchdog can evaluate stall
// state while already holding c.mu.Lock without re-entering the RWMutex.
func (c *Controller) isJobStalledLocked(job *RemoteWriteJob, now time.Time) bool {
	if job == nil || job.MetricAccess == nil ||
		job.MetricAccess.Spec.RemoteWrite == nil ||
		!job.MetricAccess.Spec.RemoteWrite.Enabled {
		return false
	}

	baseline := job.lastCleanCycleTime
	if baseline.IsZero() {
		// No clean cycle yet: fall back to the job's creation time.
		baseline = job.createdAt
	}

	if baseline.IsZero() {
		// No baseline to judge against; avoid a false positive.
		return false
	}

	return now.Sub(baseline) > c.stallGracePeriod()
}

// Start begins the remote write controller
func (c *Controller) Start(ctx context.Context) error {
	logrus.Info("Starting remote write controller")
	
	// Initial load of existing MetricAccess resources with remote write enabled
	if err := c.loadExistingRemoteWriteJobs(ctx); err != nil {
		logrus.Errorf("Failed to load existing remote write jobs: %v", err)
	}
	
	// Start reconciliation loop
	go c.reconcileLoop(ctx)
	
	return nil
}

// Stop gracefully shuts down the controller
func (c *Controller) Stop() {
	logrus.Info("Stopping remote write controller")
	close(c.stopCh)
	
	// Stop all active jobs
	c.stopAllJobs()
	
	// Wait for reconciliation loop to finish
	<-c.doneCh
}

// reconcileLoop periodically reconciles remote write jobs
func (c *Controller) reconcileLoop(ctx context.Context) {
	defer close(c.doneCh)

	// LEADER-ONLY marker: reconcileLoop is started by Controller.Start,
	// which cmd/proxy/main.go invokes ONLY from the leader-election
	// OnStartedLeading callback (and, with leader election disabled, only for the
	// single active replica). Reaching this code therefore means this replica is
	// the remote-write leader, so the self-heal pod restart performed from
	// evaluateSelfHeal is structurally leader-only. Record it as an explicit,
	// testable gate on that escalation.
	c.mu.Lock()
	c.isLeader = true
	c.mu.Unlock()

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.stopCh:
			return
		case <-ticker.C:
			if err := c.reconcileRemoteWriteJobs(ctx); err != nil {
				logrus.Errorf("Failed to reconcile remote write jobs: %v", err)
			}
			// Self-heal: after reconciling desired state, sweep active
			// jobs and recreate any that have stalled. Reuses this ticker rather
			// than spawning a separate goroutine.
			c.evaluateSelfHeal(ctx, c.now())
		}
	}
}

// loadExistingRemoteWriteJobs loads existing MetricAccess resources with remote write enabled
func (c *Controller) loadExistingRemoteWriteJobs(ctx context.Context) error {
	var metricAccessList v1alpha1.MetricAccessList
	if err := c.client.List(ctx, &metricAccessList); err != nil {
		return fmt.Errorf("failed to list MetricAccess resources: %w", err)
	}
	
	for _, metricAccess := range metricAccessList.Items {
		if metricAccess.Spec.RemoteWrite != nil && metricAccess.Spec.RemoteWrite.Enabled {
			if err := c.createRemoteWriteJob(ctx, &metricAccess); err != nil {
				logrus.Errorf("Failed to create remote write job for %s/%s: %v", 
					metricAccess.Namespace, metricAccess.Name, err)
			}
		}
	}
	
	logrus.Infof("Loaded %d remote write jobs", len(c.jobs))
	return nil
}

// reconcileRemoteWriteJobs reconciles the current state with desired state
func (c *Controller) reconcileRemoteWriteJobs(ctx context.Context) error {
	var metricAccessList v1alpha1.MetricAccessList
	if err := c.client.List(ctx, &metricAccessList); err != nil {
		return fmt.Errorf("failed to list MetricAccess resources: %w", err)
	}
	
	c.mu.Lock()
	defer c.mu.Unlock()
	
	// Track which jobs should exist
	desiredJobs := make(map[string]*v1alpha1.MetricAccess)
	
	for _, metricAccess := range metricAccessList.Items {
		key := fmt.Sprintf("%s/%s", metricAccess.Namespace, metricAccess.Name)
		
		if metricAccess.Spec.RemoteWrite != nil && metricAccess.Spec.RemoteWrite.Enabled {
			desiredJobs[key] = &metricAccess
			
			// Check if job exists and if configuration has changed
			if existingJob, exists := c.jobs[key]; exists {
				// Check if the configuration has changed
				if c.hasConfigurationChanged(existingJob.MetricAccess, &metricAccess) {
					logrus.WithFields(logrus.Fields{
						"namespace": metricAccess.Namespace,
						"name":     metricAccess.Name,
					}).Info("DIAGNOSTIC: MetricAccess configuration changed, restarting remote write job")
					
					// Stop the existing job
					c.stopRemoteWriteJobLocked(key, existingJob)
					
					// Create a new job with updated configuration
					if err := c.createRemoteWriteJobLocked(ctx, &metricAccess); err != nil {
						logrus.Errorf("Failed to recreate remote write job for %s: %v", key, err)
					}
				}
			} else {
				// Create job if it doesn't exist
				if err := c.createRemoteWriteJobLocked(ctx, &metricAccess); err != nil {
					logrus.Errorf("Failed to create remote write job for %s: %v", key, err)
				}
			}
		}
	}
	
	// Stop jobs that should no longer exist
	for key, job := range c.jobs {
		if _, shouldExist := desiredJobs[key]; !shouldExist {
			c.stopRemoteWriteJobLocked(key, job)
		}
	}
	
	return nil
}

// createRemoteWriteJob creates a new remote write job
func (c *Controller) createRemoteWriteJob(ctx context.Context, metricAccess *v1alpha1.MetricAccess) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.createRemoteWriteJobLocked(ctx, metricAccess)
}

// createRemoteWriteJobLocked creates a new remote write job (must be called with lock held)
func (c *Controller) createRemoteWriteJobLocked(ctx context.Context, metricAccess *v1alpha1.MetricAccess) error {
	key := fmt.Sprintf("%s/%s", metricAccess.Namespace, metricAccess.Name)
	
	// Validate target type
	targetType := metricAccess.Spec.RemoteWrite.Target.Type
	if targetType != "prometheus" && targetType != "pushgateway" && targetType != "remote_write" {
		logrus.WithFields(logrus.Fields{
			"namespace":   metricAccess.Namespace,
			"name":        metricAccess.Name,
			"target_type": targetType,
		}).Error("DIAGNOSTIC: Unsupported remote write target type")
		return fmt.Errorf("unsupported remote write target type: %s", targetType)
	}
	
	// Don't assign static targets - we'll get them dynamically during collection
	logrus.WithFields(logrus.Fields{
		"namespace":        metricAccess.Namespace,
		"name":             metricAccess.Name,
		"metric_patterns":  metricAccess.Spec.Metrics,
	}).Info("DIAGNOSTIC: Creating remote write job (targets will be resolved dynamically)")
	
	job := &RemoteWriteJob{
		MetricAccess: metricAccess.DeepCopy(),
		// No static targets - get them fresh each time
		StopCh:       make(chan struct{}),
		// Stall baseline for a job that has never had a successful collection.
		createdAt: c.now(),
	}
	
	c.jobs[key] = job
	
	// Start the remote write job
	go c.runRemoteWriteJob(job)
	
	logrus.WithFields(logrus.Fields{
		"namespace":       metricAccess.Namespace,
		"name":            metricAccess.Name,
		"metrics_count":   len(metricAccess.Spec.Metrics),
	}).Info("Created remote write job with dynamic target resolution")
	
	return nil
}

// stopRemoteWriteJobLocked stops a remote write job (must be called with lock held)
func (c *Controller) stopRemoteWriteJobLocked(key string, job *RemoteWriteJob) {
	close(job.StopCh)
	delete(c.jobs, key)
	logrus.Infof("Stopped remote write job for %s", key)
}

// stopAllJobs stops all active remote write jobs
func (c *Controller) stopAllJobs() {
	c.mu.Lock()
	defer c.mu.Unlock()

	for key, job := range c.jobs {
		c.stopRemoteWriteJobLocked(key, job)
	}
}

// shouldEscalatePodRestart reports whether a stalled job qualifies for the
// last-resort pod restart: the feature is enabled, this replica is the
// leader, in-process recreate has already been attempted (recreateCount >= 1),
// and the job has been CONTINUOUSLY stalled (stalledSince) beyond
// podRestartGracePeriod. The window is measured from stalledSince — which
// survives recreates — rather than per-job createdAt, so a job that recreate
// keeps failing to fix actually reaches the threshold in normal operation. It
// does NOT apply the cooldown throttle — that stays with the caller so
// lastSelfRestartAt is only advanced when a restart actually fires. It also
// requires that the job's last cycle saw healthy targets:
// a restart cannot fix "no targets", so a zero-healthy-targets outage must
// never escalate to a pod restart. Callers must hold c.mu.
func (c *Controller) shouldEscalatePodRestart(job *RemoteWriteJob, now time.Time) bool {
	if !c.selfHealPodRestartEnabled() {
		return false
	}
	if !c.isLeader {
		return false
	}
	if !job.lastCycleHadHealthyTargets {
		// No healthy targets last cycle: restarting the pod cannot help.
		return false
	}
	if job.recreateCount < 1 {
		return false
	}
	if job.stalledSince.IsZero() {
		return false
	}
	return now.Sub(job.stalledSince) > c.podRestartGracePeriod()
}

// evaluateSelfHeal runs the self-heal watchdog from the reconcile tick.
// For each stalled job it applies a two-step escalation:
//
//	Step 1 (recreate): stop the in-process collection job and recreate it with
//	fresh target resolution, rate-limited per job by recreateBackoff. Throttle
//	bookkeeping (lastRecreateAt/recreateCount) is carried forward onto the fresh
//	job struct. Recreate is gated on the job's last cycle having seen healthy
//	targets: a zero-healthy-targets outage is not
//	recreated, since recreation cannot conjure targets.
//
//	Step 2 (pod restart, LAST RESORT): if in-process recreate has already been
//	attempted (recreateCount >= 1) and the job is STILL stalled beyond
//	podRestartGracePeriod, restart the pod by exiting the process with a
//	non-zero code. Escalation is checked BEFORE recreate so a job that recreate
//	could not fix is restarted rather than recreated yet again
//	(escalate-only-after-recreate-failed). It is leader-only (c.isLeader) and
//	gated by the --self-heal-pod-restart flag, and throttled to at most one
//	attempt per podRestartCooldown via lastSelfRestartAt.
//
// It holds c.mu for the whole sweep, consistent with reconcileRemoteWriteJobs,
// and uses isJobStalledLocked to avoid re-entering the RWMutex.
func (c *Controller) evaluateSelfHeal(ctx context.Context, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Collect stalled keys first so we do not recreate while ranging the map we
	// are about to mutate (stop deletes, create re-adds the same key).
	var stalledKeys []string
	for key, job := range c.jobs {
		if c.isJobStalledLocked(job, now) {
			stalledKeys = append(stalledKeys, key)
		}
	}

	for _, key := range stalledKeys {
		job, ok := c.jobs[key]
		if !ok {
			continue
		}

		// Record the first moment this job was observed continuously
		// stalled. Every job in stalledKeys is stalled right now, so seed
		// stalledSince if unset. It PERSISTS across recreates (carried forward
		// below) and clears only on a successful collection, so it — not the
		// recreate-resetting createdAt — is the escalation window baseline.
		if job.stalledSince.IsZero() {
			job.stalledSince = now
		}

		// Stall duration: measured from the per-job baseline (last clean cycle,
		// else creation time). This resets on recreate and is used only for
		// recreate logging; the escalation window uses stalledSince.
		baseline := job.lastCleanCycleTime
		if baseline.IsZero() {
			baseline = job.createdAt
		}
		stallDuration := now.Sub(baseline)

		// Step 2: LAST-RESORT pod restart. Checked before recreate.
		if c.shouldEscalatePodRestart(job, now) {
			// Throttle: at most one self-restart per podRestartCooldown.
			if c.lastSelfRestartAt.IsZero() || now.Sub(c.lastSelfRestartAt) >= c.podRestartCooldown() {
				c.lastSelfRestartAt = now
				c.selfRestartTotal++
				logrus.WithFields(logrus.Fields{
					"tenant_key":     key,
					"stall_duration": now.Sub(job.stalledSince).String(),
					"recreate_count": job.recreateCount,
				}).Warn("self-heal: in-process recreate exhausted; restarting pod via non-zero exit (last resort)")

				// Trigger the pod restart. In production c.exit is os.Exit and
				// does not return, so nothing below runs. In tests the injected
				// seam records the code and DOES return; return immediately so we
				// do not fall through to recreate or process further jobs. The
				// deferred c.mu.Unlock still runs on return.
				c.exit(1)
				return
			}
			// Within cooldown: an escalation already happened recently. Do not
			// exit again and do not recreate this job — wait out the cooldown.
			continue
		}

		// Target-presence gate: recreate cannot fix "no
		// targets". Only recreate a job whose last cycle actually saw healthy
		// targets — an upstream outage (zero healthy targets across the cycle) must
		// not drive recreate churn or a restart storm.
		if !job.lastCycleHadHealthyTargets {
			continue
		}

		// Step 1 (recreate): rate limit — recreate a given job at most once per
		// recreateBackoff.
		if !job.lastRecreateAt.IsZero() && now.Sub(job.lastRecreateAt) < c.recreateBackoff() {
			continue
		}

		// Carry throttle bookkeeping forward across the stop/recreate: the new
		// job is a fresh struct, so capture the counters before stopping. The
		// continuous-stall baseline (stalledSince) is carried forward too so the
		// pod-restart escalation window is NOT reset by an in-process recreate, and so is
		// lastCycleHadHealthyTargets so the escalation gate stays satisfied until
		// the fresh job's own goroutine records its first cycle.
		metricAccess := job.MetricAccess
		nextRecreateCount := job.recreateCount + 1
		carriedStalledSince := job.stalledSince
		carriedHadHealthyTargets := job.lastCycleHadHealthyTargets

		c.stopRemoteWriteJobLocked(key, job)
		if err := c.createRemoteWriteJobLocked(ctx, metricAccess); err != nil {
			logrus.WithFields(logrus.Fields{
				"tenant_key":     key,
				"stall_duration": stallDuration.String(),
				"recreate_count": nextRecreateCount,
			}).Errorf("self-heal: failed to recreate stalled collection job: %v", err)
			continue
		}

		// Set throttle state on the freshly created job struct.
		newJob := c.jobs[key]
		newJob.lastRecreateAt = now
		newJob.recreateCount = nextRecreateCount
		newJob.stalledSince = carriedStalledSince
		newJob.lastCycleHadHealthyTargets = carriedHadHealthyTargets

		logrus.WithFields(logrus.Fields{
			"tenant_key":     key,
			"stall_duration": stallDuration.String(),
			"recreate_count": newJob.recreateCount,
			"attempt":        newJob.recreateCount,
		}).Warn("self-heal: recreated stalled collection job")
	}
}

// hasConfigurationChanged checks if the MetricAccess configuration has changed in ways that require job restart
func (c *Controller) hasConfigurationChanged(oldMA, newMA *v1alpha1.MetricAccess) bool {
	// Check if metricIsolation setting changed
	if oldMA.Spec.MetricIsolation != newMA.Spec.MetricIsolation {
		logrus.WithFields(logrus.Fields{
			"namespace":           newMA.Namespace,
			"name":               newMA.Name,
			"old_metric_isolation": oldMA.Spec.MetricIsolation,
			"new_metric_isolation": newMA.Spec.MetricIsolation,
		}).Info("DIAGNOSTIC: metricIsolation setting changed")
		return true
	}
	
	// Check if metrics patterns changed
	if len(oldMA.Spec.Metrics) != len(newMA.Spec.Metrics) {
		logrus.WithFields(logrus.Fields{
			"namespace":    newMA.Namespace,
			"name":        newMA.Name,
			"old_count":   len(oldMA.Spec.Metrics),
			"new_count":   len(newMA.Spec.Metrics),
		}).Info("DIAGNOSTIC: metrics patterns count changed")
		return true
	}
	
	// Check individual metric patterns
	oldMetrics := make(map[string]bool)
	for _, metric := range oldMA.Spec.Metrics {
		oldMetrics[metric] = true
	}
	
	for _, metric := range newMA.Spec.Metrics {
		if !oldMetrics[metric] {
			logrus.WithFields(logrus.Fields{
				"namespace":   newMA.Namespace,
				"name":       newMA.Name,
				"new_metric": metric,
			}).Info("DIAGNOSTIC: new metric pattern detected")
			return true
		}
	}
	
	// Check if remote write configuration changed
	if oldMA.Spec.RemoteWrite != nil && newMA.Spec.RemoteWrite != nil {
		// Check interval
		if oldMA.Spec.RemoteWrite.Interval != newMA.Spec.RemoteWrite.Interval {
			logrus.WithFields(logrus.Fields{
				"namespace":     newMA.Namespace,
				"name":         newMA.Name,
				"old_interval": oldMA.Spec.RemoteWrite.Interval,
				"new_interval": newMA.Spec.RemoteWrite.Interval,
			}).Info("DIAGNOSTIC: remote write interval changed")
			return true
		}
		
		// Check target configuration
		if oldMA.Spec.RemoteWrite.Target.Type != newMA.Spec.RemoteWrite.Target.Type {
			logrus.WithFields(logrus.Fields{
				"namespace":       newMA.Namespace,
				"name":           newMA.Name,
				"old_target_type": oldMA.Spec.RemoteWrite.Target.Type,
				"new_target_type": newMA.Spec.RemoteWrite.Target.Type,
			}).Info("DIAGNOSTIC: remote write target type changed")
			return true
		}
		
		// Check extraLabels
		if !c.compareExtraLabels(oldMA.Spec.RemoteWrite.ExtraLabels, newMA.Spec.RemoteWrite.ExtraLabels) {
			logrus.WithFields(logrus.Fields{
				"namespace": newMA.Namespace,
				"name":     newMA.Name,
			}).Info("DIAGNOSTIC: extraLabels configuration changed")
			return true
		}
		
		// Check metricRelabelings
		if len(oldMA.Spec.RemoteWrite.MetricRelabelings) != len(newMA.Spec.RemoteWrite.MetricRelabelings) {
			logrus.WithFields(logrus.Fields{
				"namespace": newMA.Namespace,
				"name":     newMA.Name,
			}).Info("DIAGNOSTIC: metricRelabelings count changed")
			return true
		}
		
		// Check prometheus target replicas/statefulset changes
		if oldMA.Spec.RemoteWrite.Prometheus != nil && newMA.Spec.RemoteWrite.Prometheus != nil {
			oldP := oldMA.Spec.RemoteWrite.Prometheus
			newP := newMA.Spec.RemoteWrite.Prometheus
			if oldP.Replicas != newP.Replicas || oldP.StatefulSetName != newP.StatefulSetName {
				logrus.WithFields(logrus.Fields{
					"namespace": newMA.Namespace,
					"name":     newMA.Name,
				}).Info("DIAGNOSTIC: prometheus target replica config changed")
				return true
			}
		}
	}
	
	return false
}

// compareExtraLabels compares two extraLabels maps for equality
func (c *Controller) compareExtraLabels(old, new map[string]string) bool {
	if len(old) != len(new) {
		return false
	}
	
	for key, oldValue := range old {
		if newValue, exists := new[key]; !exists || oldValue != newValue {
			return false
		}
	}
	
	return true
}

// runRemoteWriteJob runs a remote write job until stopped
func (c *Controller) runRemoteWriteJob(job *RemoteWriteJob) {
	logrus.WithFields(logrus.Fields{
		"namespace": job.MetricAccess.Namespace,
		"name":     job.MetricAccess.Name,
	}).Info("Starting remote write job")

	// Get collection interval
	interval := 30 * time.Second
	if job.MetricAccess.Spec.RemoteWrite.Interval.Duration > 0 {
		interval = job.MetricAccess.Spec.RemoteWrite.Interval.Duration
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Initial collection
	c.collectMetrics(job)

	for {
		select {
		case <-job.StopCh:
			logrus.WithFields(logrus.Fields{
				"namespace": job.MetricAccess.Namespace,
				"name":     job.MetricAccess.Name,
			}).Info("Stopping remote write job")
			return
		case <-ticker.C:
			c.collectMetrics(job)
		}
	}
}

// collectMetrics collects metrics from all available healthy targets for a job
func (c *Controller) collectMetrics(job *RemoteWriteJob) {
	logrus.WithFields(logrus.Fields{
		"namespace": job.MetricAccess.Namespace,
		"name":     job.MetricAccess.Name,
	}).Debug("Collecting metrics for remote write job")

	// Get fresh targets from service discovery each time
	allTargets := c.serviceDiscovery.GetTargets()
	
	// Filter for healthy targets only
	var healthyTargets []discovery.Target
	for _, target := range allTargets {
		if target.Healthy {
			healthyTargets = append(healthyTargets, target)
		}
	}
	
	logrus.WithFields(logrus.Fields{
		"namespace":        job.MetricAccess.Namespace,
		"name":             job.MetricAccess.Name,
		"all_targets":      len(allTargets),
		"healthy_targets":  len(healthyTargets),
	}).Debug("DIAGNOSTIC: Got fresh targets for metric collection")
	
	// If no healthy targets, log and return
	if len(healthyTargets) == 0 {
		logrus.WithFields(logrus.Fields{
			"namespace": job.MetricAccess.Namespace,
			"name":      job.MetricAccess.Name,
		}).Warn("DIAGNOSTIC: No healthy targets available for metric collection")
		
		// Store empty metrics and record a FAILURE cycle: zero healthy targets is a
		// genuine can't-collect condition, so it does NOT refresh lastCleanCycleTime
		// and the watchdog will see the job as stalled. But the escalations are
		// gated on lastCycleHadHealthyTargets, so an upstream outage neither
		// recreates nor restarts the pod (neither can fix "no targets").
		// LastError/FailureCount are deliberately unchanged here to preserve the
		// existing silent-empty-cycle semantics; stall tracking lives on the new
		// fields.
		key := fmt.Sprintf("%s/%s", job.MetricAccess.Namespace, job.MetricAccess.Name)
		c.mu.Lock()
		c.collectedMetrics[key] = []Metric{}
		job.lastCycleHadHealthyTargets = false
		job.consecutiveEmptyCollections++
		job.lastCollectionSucceeded = false
		job.LastRun = time.Now()
		c.mu.Unlock()

		logrus.WithFields(logrus.Fields{
			"namespace": job.MetricAccess.Namespace,
			"name":     job.MetricAccess.Name,
			"count":    0,
		}).Debug("Stored collected metrics")

		c.updateJobStatus(job)
		return
	}

	var allMetrics []Metric
	// targetErr records whether at least one healthy target returned a collection
	// error this cycle. Together with the series count it separates a genuine
	// FAILURE from a legitimate CLEAN-EMPTY cycle.
	targetErr := false
	for i, target := range healthyTargets {
		logrus.WithFields(logrus.Fields{
			"target_index": i,
			"target_url":   target.URL,
			"namespace":    job.MetricAccess.Namespace,
			"name":         job.MetricAccess.Name,
		}).Debug("DIAGNOSTIC: Collecting from target")

		metrics, err := c.collectFromTarget(job, target)
		if err != nil {
			logrus.WithError(err).Errorf("Failed to collect metrics from target %s", target.URL)
			c.mu.Lock()
			job.LastError = err
			job.FailureCount++
			c.mu.Unlock()
			targetErr = true
			continue
		}
		allMetrics = append(allMetrics, metrics...)
	}

	// Deduplicate metrics by (name, label fingerprint): when multiple infra Prometheus
	// targets are cluster-wide (not node-sharded), they all return the same series.
	// Writing 13 copies with slightly different timestamps corrupts rate() calculations.
	// Keep the sample with the most recent timestamp for each unique series.
	allMetrics = deduplicateMetrics(allMetrics)

	// Store the latest collected metrics for this job. Healthy targets existed this
	// cycle, so record that for the self-heal escalation gate.
	key := fmt.Sprintf("%s/%s", job.MetricAccess.Namespace, job.MetricAccess.Name)
	c.mu.Lock()
	c.collectedMetrics[key] = allMetrics
	job.lastCycleHadHealthyTargets = true
	c.mu.Unlock()

	logrus.WithFields(logrus.Fields{
		"namespace": job.MetricAccess.Namespace,
		"name":     job.MetricAccess.Name,
		"count":    len(allMetrics),
		"targets_used": len(healthyTargets),
	}).Info("DIAGNOSTIC: Stored collected metrics from all targets")

	// Classify the cycle:
	//   SUCCESS     — >=1 series stored AND send returned no error.
	//   FAILURE     — a send error, OR one or more per-target collection errors.
	//   CLEAN-EMPTY — healthy targets queried WITHOUT error but 0 series returned;
	//                 legitimately nothing to collect (NOT a failure).
	if len(allMetrics) > 0 {
		if err := c.sendMetrics(job, allMetrics); err != nil {
			logrus.WithError(err).Error("Failed to send metrics")
			// FAILURE: a send error is a genuine can't-collect condition. Do NOT
			// refresh lastCleanCycleTime; the watchdog may recreate (targets exist).
			c.mu.Lock()
			job.LastError = err
			job.FailureCount++
			job.consecutiveEmptyCollections++
			job.lastCollectionSucceeded = false
			c.mu.Unlock()
		} else {
			// SUCCESS: >=1 series stored AND remote-write send returned no error.
			c.mu.Lock()
			job.SuccessCount++
			job.MetricsCount += int64(len(allMetrics))
			job.lastSuccessfulCollectionTime = c.now()
			job.lastCollectedSeriesCount = len(allMetrics)
			job.consecutiveEmptyCollections = 0
			job.lastCollectionSucceeded = true
			// An error-free reachable cycle refreshes the self-heal stall baseline.
			job.lastCleanCycleTime = c.now()
			// Clear the self-heal recreate throttle baseline so a future
			// stall is handled fresh. recreateCount is a lifetime counter and is
			// intentionally left intact.
			job.lastRecreateAt = time.Time{}
			// A clean cycle ends the continuous-stall window, so clear the
			// escalation baseline. A future stall starts a fresh window.
			job.stalledSince = time.Time{}
			c.mu.Unlock()
		}
	} else if targetErr {
		// FAILURE: healthy targets were queried but at least one returned a
		// collection error and no series were produced. Treat as can't-collect: do
		// NOT refresh lastCleanCycleTime.
		c.mu.Lock()
		job.consecutiveEmptyCollections++
		job.lastCollectionSucceeded = false
		c.mu.Unlock()
	} else {
		// CLEAN-EMPTY: healthy targets queried WITHOUT error, 0 series returned —
		// legitimately nothing to collect. This is NOT a failure: refresh
		// lastCleanCycleTime so the watchdog never classifies this tenant as
		// stalled, and clear the continuous-stall baseline. It still reports
		// collection_up=0 (lastCollectionSucceeded stays STRICT success) and so
		// still fires the stall ALERT.
		c.mu.Lock()
		job.consecutiveEmptyCollections++
		job.lastCollectionSucceeded = false
		job.lastCleanCycleTime = c.now()
		job.stalledSince = time.Time{}
		c.mu.Unlock()
	}

	c.mu.Lock()
	job.LastRun = time.Now()
	c.mu.Unlock()
	c.updateJobStatus(job)
}

// collectFromTarget collects metrics from a single target
func (c *Controller) collectFromTarget(job *RemoteWriteJob, target discovery.Target) ([]Metric, error) {
	// Log target details for metric collection
	logrus.WithFields(logrus.Fields{
		"target_url": target.URL,
		"job":        fmt.Sprintf("%s/%s", job.MetricAccess.Namespace, job.MetricAccess.Name),
		"healthy":    target.Healthy,
		"labels":     fmt.Sprintf("%v", target.Labels),
	}).Debug("DIAGNOSTIC: Starting metric collection from target") 

	// Skip unhealthy targets
	if !target.Healthy {
		logrus.WithFields(logrus.Fields{
			"target_url": target.URL,
		}).Warn("DIAGNOSTIC: Skipping unhealthy target")
		return nil, fmt.Errorf("target is unhealthy")
	}

	var metrics []Metric
	client := &http.Client{
		Timeout: 30 * time.Second,
	}

	// Log all metric patterns we're going to query
	logrus.WithFields(logrus.Fields{
		"target_url":     target.URL,
		"metric_count":   len(job.MetricAccess.Spec.Metrics),
		"metric_patterns": job.MetricAccess.Spec.Metrics,
	}).Debug("DIAGNOSTIC: Metric patterns to query")

	// Query each metric pattern
	for _, pattern := range job.MetricAccess.Spec.Metrics {
		logrus.WithFields(logrus.Fields{
			"pattern":    pattern,
			"target_url": target.URL,
		}).Debug("DIAGNOSTIC: Processing metric pattern") 

		// Handle label selector patterns
		var queryPattern string
		if strings.HasPrefix(pattern, "{") && strings.HasSuffix(pattern, "}") {
			// This is already a PromQL selector pattern
			queryPattern = pattern
		} else {
			// Simple metric name pattern
			// First try with just the metric name
			queryPattern = pattern
			
			// Check if we have any label selectors to apply
			if len(job.MetricAccess.Spec.LabelSelectors) > 0 {
				// Add label selectors
				labels := []string{}
				for k, v := range job.MetricAccess.Spec.LabelSelectors {
					labels = append(labels, fmt.Sprintf("%s=\"%s\"", k, v))
				}
				queryPattern = fmt.Sprintf("{__name__=\"%s\",%s}", pattern, strings.Join(labels, ","))
			}
		}

		// Try the regular query first
		logrus.WithFields(logrus.Fields{
			"query_pattern": queryPattern,
			"target_url":    target.URL,
		}).Debug("DIAGNOSTIC: Querying with standard pattern")
		
		metricResults, err := c.queryPromQL(client, target.URL, queryPattern, job)
		if err != nil {
			logrus.WithFields(logrus.Fields{
				"error":        err,
				"pattern":      queryPattern,
				"target_url":   target.URL,
				"error_type":   fmt.Sprintf("%T", err),
				"error_details": err.Error(),
			}).Error("DIAGNOSTIC: Failed to query metrics")
			continue
		}

		// If we got metrics, add them to the result
		if len(metricResults) > 0 {
			metrics = append(metrics, metricResults...)
			logrus.WithFields(logrus.Fields{
				"count":        len(metricResults),
				"pattern":      queryPattern,
				"target_url":   target.URL,
				"first_metric": metricResults[0].Name,
			}).Info("DIAGNOSTIC: Found metrics with standard pattern")
			continue
		} else {
			logrus.WithFields(logrus.Fields{
				"pattern":    queryPattern,
				"target_url": target.URL,
			}).Debug("DIAGNOSTIC: No metrics found with standard pattern")
		}

		// If no metrics found and this is a simple metric name, try different strategies
		if !strings.HasPrefix(pattern, "{") && strings.Count(pattern, "\"") == 0 {
			// For node metrics (starting with "node_"), try with job="node-exporter"
			if strings.HasPrefix(pattern, "node_") {
				nodeQueryPattern := fmt.Sprintf("{__name__=\"%s\",job=\"node-exporter\"}", pattern)
				logrus.WithFields(logrus.Fields{
					"query_pattern": nodeQueryPattern,
					"target_url":    target.URL,
				}).Debug("DIAGNOSTIC: Trying node-exporter specific pattern")
				
				nodeMetrics, err := c.queryPromQL(client, target.URL, nodeQueryPattern, job)
				if err != nil {
					logrus.WithFields(logrus.Fields{
						"error":        err,
						"pattern":      nodeQueryPattern,
						"target_url":   target.URL,
						"error_details": err.Error(),
					}).Error("DIAGNOSTIC: Failed to query with node-exporter pattern")
				} else if len(nodeMetrics) > 0 {
					metrics = append(metrics, nodeMetrics...)
					logrus.WithFields(logrus.Fields{
						"count":        len(nodeMetrics),
						"pattern":      nodeQueryPattern,
						"target_url":   target.URL,
						"first_metric": nodeMetrics[0].Name,
					}).Info("DIAGNOSTIC: Found metrics with node-exporter pattern")
					continue
				} else {
					logrus.WithFields(logrus.Fields{
						"pattern":    nodeQueryPattern,
						"target_url": target.URL,
					}).Debug("DIAGNOSTIC: No metrics found with node-exporter pattern")
				}
			}
			
			// Try with no label selectors as last resort for any metric type
			flexibleQueryPattern := fmt.Sprintf("{__name__=\"%s\"}", pattern)
			logrus.WithFields(logrus.Fields{
				"query_pattern": flexibleQueryPattern,
				"target_url":    target.URL,
			}).Debug("DIAGNOSTIC: Trying generic pattern without selectors")
			
			flexibleMetrics, err := c.queryPromQL(client, target.URL, flexibleQueryPattern, job)
			if err != nil {
				logrus.WithFields(logrus.Fields{
					"error":        err,
					"pattern":      flexibleQueryPattern,
					"target_url":   target.URL,
					"error_details": err.Error(),
				}).Error("DIAGNOSTIC: Failed to query with generic pattern")
			} else if len(flexibleMetrics) > 0 {
				metrics = append(metrics, flexibleMetrics...)
				logrus.WithFields(logrus.Fields{
					"count":        len(flexibleMetrics),
					"pattern":      flexibleQueryPattern,
					"target_url":   target.URL,
					"first_metric": flexibleMetrics[0].Name,
				}).Info("DIAGNOSTIC: Found metrics with generic pattern")
			} else {
				logrus.WithFields(logrus.Fields{
					"pattern":    flexibleQueryPattern,
					"target_url": target.URL,
				}).Warn("DIAGNOSTIC: No metrics found for pattern using any query strategy")
			}
		}
	}

	logrus.WithFields(logrus.Fields{
		"count":      len(metrics),
		"target_url": target.URL,
		"job":        fmt.Sprintf("%s/%s", job.MetricAccess.Namespace, job.MetricAccess.Name),
	}).Info("DIAGNOSTIC: Completed metric collection from target") 

	return metrics, nil
}

// queryPromQL queries the Prometheus instance for the given PromQL query
func (c *Controller) queryPromQL(client *http.Client, targetURL, query string, job *RemoteWriteJob) ([]Metric, error) {
	var queryURL string
	var req *http.Request
	var err error
	
	// Check if metric isolation is enabled for this specific tenant
	if job.MetricAccess.Spec.MetricIsolation {
		// Use prom-label-proxy for namespace-filtered queries
		// Construct URL to prom-label-proxy (port 8082) instead of direct target
		proxyURL := "http://localhost:8082/api/v1/query?query=" + url.QueryEscape(query)
		queryURL = proxyURL
		
		// Create request with namespace header for prom-label-proxy
		req, err = http.NewRequest("GET", queryURL, nil)
		if err != nil {
			logrus.WithFields(logrus.Fields{
				"error": err,
				"url":   queryURL,
			}).Error("DIAGNOSTIC: Failed to create HTTP request for prom-label-proxy")
			return nil, err
		}
		
		// Add the tenant namespace header for prom-label-proxy filtering
		req.Header.Set("X-Tenant-Namespace", job.MetricAccess.Namespace)
		req.Header.Set("User-Agent", "prometheus-multi-tenant-proxy/remote-write-controller")
		
		logrus.WithFields(logrus.Fields{
			"query_url": queryURL,
			"query": query,
			"target_url": targetURL,
			"using_proxy": true,
			"tenant_namespace": job.MetricAccess.Namespace,
		}).Debug("DIAGNOSTIC: Making Prometheus query through prom-label-proxy with namespace filtering")
		
	} else {
		// Query the target directly (original behavior)
		queryURL = fmt.Sprintf("%s/api/v1/query?query=%s", targetURL, url.QueryEscape(query))
		
		// Create request
		req, err = http.NewRequest("GET", queryURL, nil)
		if err != nil {
			logrus.WithFields(logrus.Fields{
				"error": err,
				"url":   queryURL,
			}).Error("DIAGNOSTIC: Failed to create HTTP request")
			return nil, err
		}
		
		// Add user agent for identification
		req.Header.Set("User-Agent", "prometheus-multi-tenant-proxy/remote-write-controller")
		
		logrus.WithFields(logrus.Fields{
			"query_url": queryURL,
			"query": query,
			"target_url": targetURL,
			"using_proxy": false,
		}).Debug("DIAGNOSTIC: Making Prometheus query directly to target")
	}
	
	logrus.WithFields(logrus.Fields{
		"headers": req.Header,
		"url":     queryURL,
	}).Debug("DIAGNOSTIC: Request headers set for direct target query")
	
	// Make the request
	resp, err := client.Do(req)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"error": err,
			"url":   queryURL,
			"error_type": fmt.Sprintf("%T", err),
			"error_details": err.Error(),
		}).Error("DIAGNOSTIC: HTTP request failed")
		return nil, err
	}
	defer resp.Body.Close()
	
	// Parse the response
	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		logrus.WithFields(logrus.Fields{
			"status_code": resp.StatusCode,
			"url":         queryURL,
			"response":    string(bodyBytes),
		}).Error("DIAGNOSTIC: Prometheus query returned non-200 status code")
		return nil, fmt.Errorf("prometheus query failed with status %d: %s", resp.StatusCode, string(bodyBytes))
	}
	
	// Read the response body
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"error": err,
			"url":   queryURL,
		}).Error("DIAGNOSTIC: Failed to read response body")
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}
	
	// Log the raw response for debugging
	logrus.WithFields(logrus.Fields{
		"url":      queryURL,
		"response_size": len(bodyBytes),
		"response_preview": string(bodyBytes[:min(len(bodyBytes), 500)]), // Log first 500 chars max
	}).Debug("DIAGNOSTIC: Received Prometheus response")
	
	// Parse the response
	var promResp PrometheusResponse
	if err := json.Unmarshal(bodyBytes, &promResp); err != nil {
		logrus.WithFields(logrus.Fields{
			"error": err,
			"url":   queryURL,
			"body":  string(bodyBytes),
		}).Error("DIAGNOSTIC: Failed to parse Prometheus response")
		return nil, fmt.Errorf("failed to parse prometheus response: %w", err)
	}
	
	// Check the response status
	if promResp.Status != "success" {
		logrus.WithFields(logrus.Fields{
			"url":    queryURL,
			"status": promResp.Status,
			"body":   string(bodyBytes),
		}).Error("DIAGNOSTIC: Prometheus returned non-success status")
		return nil, fmt.Errorf("prometheus returned non-success status: %s", promResp.Status)
	}
	
	// Convert the results to metrics
	var metrics []Metric
	for _, result := range promResp.Data.Result {
		// Extract the metric name
		metricName := result.Metric["__name__"]
		
		// Extract labels
		labels := model.LabelSet{}
		for name, value := range result.Metric {
			if name != "__name__" {
				labels[model.LabelName(name)] = model.LabelValue(value)
			}
		}
		
		// Extract value and timestamp
		if len(result.Value) < 2 {
			logrus.WithFields(logrus.Fields{
				"url":    queryURL,
				"metric": metricName,
			}).Error("DIAGNOSTIC: Invalid value format in Prometheus response")
			continue
		}
		
		// Parse timestamp
		var timestamp time.Time
		if ts, ok := result.Value[0].(float64); ok {
			timestamp = time.Unix(int64(ts), 0)
		} else {
			logrus.WithFields(logrus.Fields{
				"url":    queryURL,
				"metric": metricName,
				"value":  fmt.Sprintf("%v", result.Value[0]),
			}).Error("DIAGNOSTIC: Invalid timestamp format in Prometheus response")
			timestamp = time.Now()
		}
		
		// Parse value
		var value float64
		switch v := result.Value[1].(type) {
		case string:
			parsedValue, err := strconv.ParseFloat(v, 64)
		if err != nil {
				logrus.WithFields(logrus.Fields{
					"url":    queryURL,
					"metric": metricName,
					"value":  v,
					"error":  err,
				}).Error("DIAGNOSTIC: Failed to parse metric value")
				continue
			}
			value = parsedValue
		case float64:
			value = v
		default:
			logrus.WithFields(logrus.Fields{
				"url":    queryURL,
				"metric": metricName,
				"value":  fmt.Sprintf("%v", result.Value[1]),
				"type":   fmt.Sprintf("%T", result.Value[1]),
			}).Error("DIAGNOSTIC: Unexpected value type in Prometheus response")
			continue
		}
		
		// Create the metric
		metric := Metric{
			Name:      metricName,
			Labels:    labels,
			Value:     value,
			Timestamp: timestamp,
		}
		
		metrics = append(metrics, metric)
	}
	
	logrus.WithFields(logrus.Fields{
		"url":           queryURL,
		"metrics_count": len(metrics),
		"query":         query,
	}).Debug("DIAGNOSTIC: Processed Prometheus query response")
	
	return metrics, nil
}

// Helper function to get the minimum of two integers
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// sendMetrics sends collected metrics to the remote write target, splitting
// them into batches to avoid exceeding Prometheus's 32 MiB decoded body limit.
// Each batch is sent as a separate HTTP request; a per-batch timeout of 30 s
// is used so a slow receiver does not block all remaining batches.
// All batches are attempted even if an earlier one fails; errors are aggregated
// and returned so the caller can record the failure accurately.
func (c *Controller) sendMetrics(job *RemoteWriteJob, metrics []Metric) error {
	// Select collector based on target type.
	var collector Collector
	switch job.MetricAccess.Spec.RemoteWrite.Target.Type {
	case "prometheus":
		collector = NewPrometheusCollector(c.client)
	case "pushgateway":
		collector = NewPushgatewayCollector(c.client)
	case "remote_write":
		collector = NewRemoteWriteCollector(c.client)
	default:
		return fmt.Errorf("unsupported remote write target type: %s",
			job.MetricAccess.Spec.RemoteWrite.Target.Type)
	}

	batches := splitIntoBatches(metrics, c.config.BatchSize)

	logrus.WithFields(logrus.Fields{
		"namespace":       job.MetricAccess.Namespace,
		"name":            job.MetricAccess.Name,
		"total_metrics":   len(metrics),
		"total_batches":   len(batches),
		"batch_size":      c.config.BatchSize,
		"metric_isolation": job.MetricAccess.Spec.MetricIsolation,
	}).Info("Sending collected metrics in batches")

	var errs []string
	for i, batch := range batches {
		logrus.WithFields(logrus.Fields{
			"namespace":     job.MetricAccess.Namespace,
			"name":          job.MetricAccess.Name,
			"batch":         i + 1,
			"total_batches": len(batches),
			"batch_size":    len(batch),
		}).Debug("Sending metrics batch")

		// Fresh 30 s context per batch so a slow receiver on batch N does not
		// starve the timeout budget for batches N+1, N+2, …
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := collector.Send(ctx, job.MetricAccess, batch)
		cancel()

		if err != nil {
			logrus.WithFields(logrus.Fields{
				"namespace":     job.MetricAccess.Namespace,
				"name":          job.MetricAccess.Name,
				"batch":         i + 1,
				"total_batches": len(batches),
				"error":         err,
			}).Error("Failed to send metrics batch")
			errs = append(errs, fmt.Sprintf("batch %d/%d: %v", i+1, len(batches), err))
			// Continue: attempt remaining batches even after a partial failure.
			// Metrics from failed batches will be retried on the next collection cycle.
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("remote write errors (%d/%d batches failed): %s",
			len(errs), len(batches), strings.Join(errs, "; "))
	}
	return nil
}

// updateJobStatus updates the status of a remote write job.
//
// It mutates job.MetricAccess.Status, which the self-heal watchdog reads via
// MetricAccess.DeepCopy() (createRemoteWriteJobLocked) while holding c.mu. A
// just-stopped job's goroutine can still be finishing a collection cycle when
// evaluateSelfHeal recreates the job, so this write MUST be serialized under
// c.mu to avoid a data race with that DeepCopy. Callers invoke
// this without holding c.mu.
func (c *Controller) updateJobStatus(job *RemoteWriteJob) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Update the MetricAccess status
	status := &v1alpha1.RemoteWriteStatus{
		Active:           true,
		LastCollection:   metav1.NewTime(job.LastRun),
		MetricsCollected: int32(job.MetricsCount),
	}

	if job.LastError != nil {
		status.LastError = job.LastError.Error()
	}

	job.MetricAccess.Status.RemoteWrite = status

	// TODO: Update the status in Kubernetes
	// This would require using the client to patch the MetricAccess resource
}

// GetActiveJobs returns information about active remote write jobs
func (c *Controller) GetActiveJobs() map[string]*RemoteWriteJob {
	c.mu.RLock()
	defer c.mu.RUnlock()
	
	jobs := make(map[string]*RemoteWriteJob)
	for k, v := range c.jobs {
		jobs[k] = v
	}
	
	return jobs
}

// GetAllCollectedMetrics returns all collected metrics for all jobs
func (c *Controller) GetAllCollectedMetrics() map[string][]Metric {
	c.mu.RLock()
	defer c.mu.RUnlock()
	
	// Log diagnostic information if no metrics are collected
	if len(c.collectedMetrics) == 0 {
		logrus.WithFields(logrus.Fields{
			"active_jobs": len(c.jobs),
		}).Warning("No collected metrics found in remote write controller")
		
		// Log information about active jobs
		if len(c.jobs) > 0 {
			for key, job := range c.jobs {
				logrus.WithFields(logrus.Fields{
					"job_key":       key,
					"metrics_count": job.MetricsCount,
					"last_run":      job.LastRun,
					"has_error":     job.LastError != nil,
				}).Info("Active job status")
				
				if job.LastError != nil {
					logrus.WithFields(logrus.Fields{
						"job_key": key,
						"error":   job.LastError.Error(),
					}).Error("Job encountered an error")
				}
			}
		} else {
			logrus.Warning("No active remote write jobs found")
		}
	}
	
	// Make a copy to avoid concurrent map access
	metrics := make(map[string][]Metric)
	for k, v := range c.collectedMetrics {
		metrics[k] = append([]Metric{}, v...)
	}
	
	return metrics
} 