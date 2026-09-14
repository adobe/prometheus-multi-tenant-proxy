package remote_write

// Reproduce and characterize the per-tenant collection
// wedge against the HEAD image.
//
// These tests are hermetic: they use a controller-runtime fake client and a
// fake discovery.Discovery — no live cluster, no envtest binaries. They drive
// the controller's reconcile path directly (the test lives in-package) instead
// of sleeping on the 30s reconcile ticker, so the whole suite runs in
// milliseconds.
//
// What each scenario characterizes:
//   - Runtime-add: does HEAD create a collection job for a MetricAccess CR that
//     appears AFTER Start() has already run? (Controller.Start only calls
//     loadExistingRemoteWriteJobs once; the fix hinges on the reconcile loop.)
//   - Churn: does collectMetrics pick up a mutated discovery target set
//     (backend pod-IP churn) on its next run? (dynamic target resolution)
//   - Residual gap: when discovery reports zero healthy targets, collectMetrics
//     stores zero series with NO error and NO failure count — the job looks
//     "running" but silently produces nothing.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/prometheus-multi-tenant-proxy/api/v1alpha1"
	"github.com/prometheus-multi-tenant-proxy/internal/config"
	"github.com/prometheus-multi-tenant-proxy/internal/discovery"
)

// ---------------------------------------------------------------------------
// Test doubles
// ---------------------------------------------------------------------------

// fakeDiscovery is a thread-safe, in-memory discovery.Discovery whose target
// set can be mutated at runtime to simulate backend pod-IP churn.
type fakeDiscovery struct {
	mu      sync.RWMutex
	targets []discovery.Target
}

func (f *fakeDiscovery) Start(ctx context.Context) error { return nil }

func (f *fakeDiscovery) GetTargets() []discovery.Target {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make([]discovery.Target, len(f.targets))
	copy(out, f.targets)
	return out
}

func (f *fakeDiscovery) Subscribe() <-chan []discovery.Target {
	ch := make(chan []discovery.Target)
	return ch
}

func (f *fakeDiscovery) setTargets(targets []discovery.Target) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.targets = targets
}

// fakePromServer is an httptest.Server that answers /api/v1/query with an empty
// (but successful) Prometheus vector response and counts how many times it was
// queried. Returning an empty result keeps collectMetrics from calling
// sendMetrics (which would try to reach a cluster.local DNS name), so the test
// stays fully offline while still proving which targets were contacted.
type fakePromServer struct {
	server *httptest.Server
	hits   int64
}

func newFakePromServer() *fakePromServer {
	fp := &fakePromServer{}
	fp.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&fp.hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	return fp
}

func (fp *fakePromServer) hitCount() int64 { return atomic.LoadInt64(&fp.hits) }
func (fp *fakePromServer) close()          { fp.server.Close() }

// newTestController builds a controller wired to a fake k8s client (with the
// v1alpha1 scheme) and the supplied fake discovery.
func newTestController(t *testing.T, disc discovery.Discovery, objs ...runtime.Object) *Controller {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add v1alpha1 to scheme: %v", err)
	}
	builder := fake.NewClientBuilder().WithScheme(scheme)
	if len(objs) > 0 {
		builder = builder.WithRuntimeObjects(objs...)
	}
	return NewController(builder.Build(), config.RemoteWriteConfig{BatchSize: 5000}, disc)
}

// newMetricAccess builds a minimal remote-write-enabled MetricAccess CR.
func newMetricAccess(namespace, name string) *v1alpha1.MetricAccess {
	return &v1alpha1.MetricAccess{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: v1alpha1.MetricAccessSpec{
			Source:  namespace,
			Metrics: []string{"container_cpu_usage_seconds_total"},
			RemoteWrite: &v1alpha1.RemoteWriteConfig{
				Enabled: true,
				Target:  v1alpha1.RemoteWriteTarget{Type: "prometheus"},
				Prometheus: &v1alpha1.PrometheusTarget{
					ServiceName: "prometheus-operated",
					ServicePort: 9090,
				},
			},
		},
	}
}

// ---------------------------------------------------------------------------
// Scenario 1: runtime-add of a MetricAccess CR after Start().
// ---------------------------------------------------------------------------

func TestController_RuntimeAddCR_PickedUpByReconcile(t *testing.T) {
	// Discovery reports no healthy targets so the job's initial collection takes
	// the zero-target branch and never touches the network.
	disc := &fakeDiscovery{}
	c := newTestController(t, disc)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := c.Start(ctx); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}
	defer c.Stop()

	// No CRs existed at Start(), so no jobs yet.
	if got := len(c.GetActiveJobs()); got != 0 {
		t.Fatalf("expected 0 jobs after Start with empty cluster, got %d", got)
	}

	// Create a MetricAccess CR AFTER Start() has run.
	ma := newMetricAccess("tenant-a", "ma-runtime")
	if err := c.client.Create(ctx, ma); err != nil {
		t.Fatalf("failed to create MetricAccess: %v", err)
	}

	// Before any reconcile, the startup-only load path has NOT created a job.
	// This is the shipped-image bug: Start->loadExistingRemoteWriteJobs enumerates
	// tenants exactly once.
	if got := len(c.GetActiveJobs()); got != 0 {
		t.Fatalf("expected 0 jobs immediately after runtime CR create (before reconcile), got %d", got)
	}

	// Drive one reconcile directly (equivalent to one 30s tick on HEAD).
	if err := c.reconcileRemoteWriteJobs(ctx); err != nil {
		t.Fatalf("reconcileRemoteWriteJobs failed: %v", err)
	}

	// HEAD FIX: the reconcile loop must now have a collection job for the
	// runtime-added CR.
	jobs := c.GetActiveJobs()
	if _, ok := jobs["tenant-a/ma-runtime"]; !ok {
		t.Fatalf("expected reconcile to create a collection job for runtime-added CR; jobs=%v", keysOf(jobs))
	}
}

// ---------------------------------------------------------------------------
// Scenario 2: backend pod-IP churn — collectMetrics must resolve targets
// freshly on each run.
// ---------------------------------------------------------------------------

func TestController_ChurnTargets_DynamicResolution(t *testing.T) {
	serverA := newFakePromServer()
	defer serverA.close()
	serverB := newFakePromServer()
	defer serverB.close()

	disc := &fakeDiscovery{}
	disc.setTargets([]discovery.Target{
		{URL: serverA.server.URL, Healthy: true, LastSeen: time.Now()},
	})

	c := newTestController(t, disc)

	job := &RemoteWriteJob{
		MetricAccess: newMetricAccess("tenant-b", "ma-churn"),
		StopCh:       make(chan struct{}),
	}

	// First collection hits serverA.
	c.collectMetrics(job)
	if serverA.hitCount() == 0 {
		t.Fatalf("expected serverA to be queried on first collection, got 0 hits")
	}
	if serverB.hitCount() != 0 {
		t.Fatalf("serverB should not be queried before churn, got %d hits", serverB.hitCount())
	}
	hitsAAfterFirst := serverA.hitCount()

	// Simulate churn: the old backend pod is gone, a new pod-IP appears.
	disc.setTargets([]discovery.Target{
		{URL: serverB.server.URL, Healthy: true, LastSeen: time.Now()},
	})

	// Second collection must pick up the NEW target set.
	c.collectMetrics(job)

	if serverB.hitCount() == 0 {
		t.Fatalf("expected serverB to be queried after churn (dynamic target resolution), got 0 hits")
	}
	if serverA.hitCount() != hitsAAfterFirst {
		t.Fatalf("serverA (removed target) must not be queried after churn: before=%d after=%d",
			hitsAAfterFirst, serverA.hitCount())
	}
}

// ---------------------------------------------------------------------------
// Scenario 3: residual gap — zero healthy targets stores zero series with
// NO error, NO failure count. The job looks "running" but silently produces
// nothing. Nothing self-heals this state.
// ---------------------------------------------------------------------------

func TestCollectMetrics_NoHealthyTargets_SilentZeroSeriesNoError(t *testing.T) {
	// Discovery returns a target, but it is marked unhealthy — the healthyTargets
	// slice ends up empty.
	disc := &fakeDiscovery{}
	disc.setTargets([]discovery.Target{
		{URL: "http://10.0.0.1:9090", Healthy: false, LastSeen: time.Now()},
	})

	c := newTestController(t, disc)

	job := &RemoteWriteJob{
		MetricAccess: newMetricAccess("tenant-c", "ma-wedge"),
		StopCh:       make(chan struct{}),
	}

	c.collectMetrics(job)

	stored := c.GetAllCollectedMetrics()["tenant-c/ma-wedge"]
	if len(stored) != 0 {
		t.Fatalf("expected zero series stored with no healthy targets, got %d", len(stored))
	}

	// The core of the residual wedge: no error is recorded and no failure is
	// counted, so external observers see a healthy, "active" job that emits
	// nothing.
	if job.LastError != nil {
		t.Fatalf("residual gap check: expected no error recorded on zero-target collection, got %v", job.LastError)
	}
	if job.FailureCount != 0 {
		t.Fatalf("residual gap check: expected FailureCount==0 on zero-target collection, got %d", job.FailureCount)
	}
	if job.LastRun.IsZero() {
		t.Fatalf("expected LastRun to be updated even on zero-target collection")
	}
}

func keysOf(m map[string]*RemoteWriteJob) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// ---------------------------------------------------------------------------
// Shared helpers for the FAILURE-with-healthy-targets model.
// The watchdog recreates/escalates ONLY on a genuine
// can't-collect failure while healthy targets are present — never on a
// legitimately-empty tenant or a zero-target outage. These helpers build that
// state hermetically and FAST: a healthy data source paired with a send that
// fails immediately (no HTTP retry/backoff sleeps).
// ---------------------------------------------------------------------------

// healthyDataDiscovery returns a discovery with a single healthy target that
// serves one series, so collectMetrics resolves healthy targets and reaches the
// send path. The caller must Close the returned server.
func healthyDataDiscovery(t *testing.T) (*fakeDiscovery, *httptest.Server) {
	t.Helper()
	src := newDataSource()
	disc := &fakeDiscovery{}
	disc.setTargets([]discovery.Target{
		{URL: src.URL, Healthy: true, LastSeen: time.Now()},
	})
	return disc, src
}

// failingSinkMetricAccess builds a remote-write-enabled MetricAccess whose SEND
// always fails immediately: the target type is the valid "remote_write" (so job
// creation/validation succeeds) but the endpoint is nil, so the collector returns
// a config error WITHOUT the retry/backoff sleeps a bad HTTP sink would incur.
// Paired with healthyDataDiscovery this yields a genuine can't-collect FAILURE
// with healthy targets present — the only state the fixed watchdog acts on.
func failingSinkMetricAccess(namespace, name string) *v1alpha1.MetricAccess {
	return &v1alpha1.MetricAccess{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: v1alpha1.MetricAccessSpec{
			Source:  namespace,
			Metrics: []string{"container_cpu_usage_seconds_total"},
			RemoteWrite: &v1alpha1.RemoteWriteConfig{
				Enabled: true,
				Target:  v1alpha1.RemoteWriteTarget{Type: "remote_write"},
				// RemoteWrite endpoint intentionally nil -> Send fails immediately.
			},
		},
	}
}
