package remote_write

import (
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/prometheus-multi-tenant-proxy/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ---------------------------------------------------------------------------
// resolveTargetNamespace
// ---------------------------------------------------------------------------

func TestResolveTargetNamespace_NoOverride(t *testing.T) {
	target := &v1alpha1.PrometheusTarget{
		ServiceName: "prometheus-operated",
	}
	got := resolveTargetNamespace(target, "ns-app")
	if got != "ns-app" {
		t.Errorf("expected CR namespace, got %q", got)
	}
}

func TestResolveTargetNamespace_WithOverride(t *testing.T) {
	target := &v1alpha1.PrometheusTarget{
		ServiceName:     "prometheus-operated",
		TargetNamespace: "ns-monitoring",
	}
	got := resolveTargetNamespace(target, "ns-app")
	if got != "ns-monitoring" {
		t.Errorf("expected targetNamespace override, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// buildPrometheusURLs — single replica (service URL)
// ---------------------------------------------------------------------------

func TestBuildPrometheusURLs_SingleReplica_SameNamespace(t *testing.T) {
	target := &v1alpha1.PrometheusTarget{
		ServiceName: "prometheus-operated",
		ServicePort: 9090,
	}
	urls := buildPrometheusURLs(target, "ns-monitoring")
	if len(urls) != 1 {
		t.Fatalf("expected 1 URL, got %d", len(urls))
	}
	want := "http://prometheus-operated.ns-monitoring.svc.cluster.local:9090/api/v1/write"
	if urls[0] != want {
		t.Errorf("got %q, want %q", urls[0], want)
	}
}

func TestBuildPrometheusURLs_DefaultPort(t *testing.T) {
	target := &v1alpha1.PrometheusTarget{
		ServiceName: "prometheus-operated",
		// ServicePort intentionally zero — should default to 9090
	}
	urls := buildPrometheusURLs(target, "ns-monitoring")
	want := "http://prometheus-operated.ns-monitoring.svc.cluster.local:9090/api/v1/write"
	if urls[0] != want {
		t.Errorf("got %q, want %q", urls[0], want)
	}
}

// ---------------------------------------------------------------------------
// buildPrometheusURLs — multi-replica (pod DNS fan-out)
// ---------------------------------------------------------------------------

func TestBuildPrometheusURLs_MultiReplica_SameNamespace(t *testing.T) {
	target := &v1alpha1.PrometheusTarget{
		ServiceName:     "prometheus-operated",
		ServicePort:     9090,
		Replicas:        2,
		StatefulSetName: "prometheus-ha",
	}
	urls := buildPrometheusURLs(target, "ns-monitoring")
	if len(urls) != 2 {
		t.Fatalf("expected 2 URLs, got %d", len(urls))
	}
	wants := []string{
		"http://prometheus-ha-0.prometheus-operated.ns-monitoring.svc.cluster.local:9090/api/v1/write",
		"http://prometheus-ha-1.prometheus-operated.ns-monitoring.svc.cluster.local:9090/api/v1/write",
	}
	for i, want := range wants {
		if urls[i] != want {
			t.Errorf("url[%d]: got %q, want %q", i, urls[i], want)
		}
	}
}

// ---------------------------------------------------------------------------
// Cross-namespace: CR in tenant namespace pushes to Prometheus in a shared
// monitoring namespace. This is the primary use case for targetNamespace —
// a MetricAccess CR created in ns-app must push to Prometheus in ns-monitoring.
// ---------------------------------------------------------------------------

func TestBuildPrometheusURLs_CrossNamespace_ServiceURL(t *testing.T) {
	target := &v1alpha1.PrometheusTarget{
		ServiceName:     "prometheus-operated",
		ServicePort:     9090,
		TargetNamespace: "ns-monitoring",
	}
	targetNS := resolveTargetNamespace(target, "ns-app")
	urls := buildPrometheusURLs(target, targetNS)

	if len(urls) != 1 {
		t.Fatalf("expected 1 URL, got %d", len(urls))
	}
	want := "http://prometheus-operated.ns-monitoring.svc.cluster.local:9090/api/v1/write"
	if urls[0] != want {
		t.Errorf("got %q, want %q", urls[0], want)
	}
}

func TestBuildPrometheusURLs_CrossNamespace_MultiReplica(t *testing.T) {
	target := &v1alpha1.PrometheusTarget{
		ServiceName:     "prometheus-operated",
		ServicePort:     9090,
		Replicas:        2,
		StatefulSetName: "prometheus-ha",
		TargetNamespace: "ns-monitoring",
	}
	targetNS := resolveTargetNamespace(target, "ns-app")
	urls := buildPrometheusURLs(target, targetNS)

	if len(urls) != 2 {
		t.Fatalf("expected 2 URLs, got %d", len(urls))
	}
	wants := []string{
		"http://prometheus-ha-0.prometheus-operated.ns-monitoring.svc.cluster.local:9090/api/v1/write",
		"http://prometheus-ha-1.prometheus-operated.ns-monitoring.svc.cluster.local:9090/api/v1/write",
	}
	for i, want := range wants {
		if urls[i] != want {
			t.Errorf("url[%d]: got %q, want %q", i, urls[i], want)
		}
	}
}

// Setting targetNamespace == CR namespace must be identical to omitting it.
func TestBuildPrometheusURLs_TargetNamespaceSameAsCR(t *testing.T) {
	withOverride := &v1alpha1.PrometheusTarget{
		ServiceName:     "prometheus-operated",
		ServicePort:     9090,
		TargetNamespace: "ns-monitoring",
	}
	withoutOverride := &v1alpha1.PrometheusTarget{
		ServiceName: "prometheus-operated",
		ServicePort: 9090,
	}
	ns := "ns-monitoring"
	u1 := buildPrometheusURLs(withOverride, resolveTargetNamespace(withOverride, ns))
	u2 := buildPrometheusURLs(withoutOverride, resolveTargetNamespace(withoutOverride, ns))
	if u1[0] != u2[0] {
		t.Errorf("URLs differ: %q vs %q", u1[0], u2[0])
	}
}

// ---------------------------------------------------------------------------
// applyMetricRelabelings
// ---------------------------------------------------------------------------

func makeMetric(name string, labels map[string]string) Metric {
	ls := model.LabelSet{}
	for k, v := range labels {
		ls[model.LabelName(k)] = model.LabelValue(v)
	}
	return Metric{Name: name, Labels: ls, Value: 1.0, Timestamp: time.Now()}
}

func TestApplyMetricRelabelings_Replace(t *testing.T) {
	metrics := []Metric{makeMetric("container_cpu_usage_seconds_total", nil)}
	rules := []v1alpha1.MetricRelabelConfig{
		{
			SourceLabels: []string{"__name__"},
			Regex:        "container_(.*)",
			TargetLabel:  "metrics_path",
			Replacement:  "/metrics/cadvisor",
			Action:       "replace",
		},
	}
	result := applyMetricRelabelings(metrics, rules)
	if len(result) != 1 {
		t.Fatalf("expected 1 metric, got %d", len(result))
	}
	got := string(result[0].Labels["metrics_path"])
	if got != "/metrics/cadvisor" {
		t.Errorf("metrics_path: got %q, want %q", got, "/metrics/cadvisor")
	}
}

func TestApplyMetricRelabelings_Keep(t *testing.T) {
	metrics := []Metric{
		makeMetric("container_cpu_usage_seconds_total", nil),
		makeMetric("kube_pod_info", nil),
	}
	rules := []v1alpha1.MetricRelabelConfig{
		{SourceLabels: []string{"__name__"}, Regex: "container_(.*)", Action: "keep"},
	}
	result := applyMetricRelabelings(metrics, rules)
	if len(result) != 1 {
		t.Fatalf("expected 1 metric after keep filter, got %d", len(result))
	}
	if result[0].Name != "container_cpu_usage_seconds_total" {
		t.Errorf("wrong metric kept: %q", result[0].Name)
	}
}

func TestApplyMetricRelabelings_Drop(t *testing.T) {
	metrics := []Metric{
		makeMetric("container_cpu_usage_seconds_total", nil),
		makeMetric("kube_pod_info", nil),
	}
	rules := []v1alpha1.MetricRelabelConfig{
		{SourceLabels: []string{"__name__"}, Regex: "container_(.*)", Action: "drop"},
	}
	result := applyMetricRelabelings(metrics, rules)
	if len(result) != 1 {
		t.Fatalf("expected 1 metric after drop filter, got %d", len(result))
	}
	if result[0].Name != "kube_pod_info" {
		t.Errorf("wrong metric remaining: %q", result[0].Name)
	}
}

func TestApplyMetricRelabelings_NoRules(t *testing.T) {
	metrics := []Metric{makeMetric("up", nil), makeMetric("kube_pod_info", nil)}
	result := applyMetricRelabelings(metrics, nil)
	if len(result) != 2 {
		t.Errorf("expected all metrics preserved, got %d", len(result))
	}
}

// ---------------------------------------------------------------------------
// MetricAccess spec wiring: verify TargetNamespace flows end-to-end through
// the full spec — CR in tenant namespace, Prometheus in monitoring namespace.
// ---------------------------------------------------------------------------

func TestMetricAccessSpec_TargetNamespace(t *testing.T) {
	ma := &v1alpha1.MetricAccess{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "tenant-metricaccess",
			Namespace: "ns-app",
		},
		Spec: v1alpha1.MetricAccessSpec{
			Source:  "ns-app",
			Metrics: []string{"container_cpu_usage_seconds_total"},
			RemoteWrite: &v1alpha1.RemoteWriteConfig{
				Enabled: true,
				Target:  v1alpha1.RemoteWriteTarget{Type: "prometheus"},
				Prometheus: &v1alpha1.PrometheusTarget{
					ServiceName:     "prometheus-operated",
					ServicePort:     9090,
					Replicas:        2,
					StatefulSetName: "prometheus-ha",
					TargetNamespace: "ns-monitoring",
				},
			},
		},
	}

	target := ma.Spec.RemoteWrite.Prometheus
	targetNS := resolveTargetNamespace(target, ma.Namespace)
	if targetNS != "ns-monitoring" {
		t.Errorf("targetNS: got %q, want ns-monitoring", targetNS)
	}

	urls := buildPrometheusURLs(target, targetNS)
	if len(urls) != 2 {
		t.Fatalf("expected 2 pod URLs, got %d", len(urls))
	}
	for _, u := range urls {
		if !contains(u, "ns-monitoring") {
			t.Errorf("URL %q does not contain target namespace", u)
		}
		if contains(u, "ns-app") {
			t.Errorf("URL %q must not contain the CR's own namespace", u)
		}
	}
}

// ---------------------------------------------------------------------------
// deduplicateMetrics
// ---------------------------------------------------------------------------

func TestDeduplicateMetrics_NoDuplicates(t *testing.T) {
	metrics := []Metric{
		makeMetric("up", map[string]string{"job": "a"}),
		makeMetric("up", map[string]string{"job": "b"}),
	}
	result := deduplicateMetrics(metrics)
	if len(result) != 2 {
		t.Errorf("expected 2 unique metrics, got %d", len(result))
	}
}

func TestDeduplicateMetrics_KeepsHighestValue(t *testing.T) {
	low := Metric{Name: "cpu", Labels: model.LabelSet{"pod": "p1"}, Value: 100.0, Timestamp: time.Now().Add(-10 * time.Second)}
	high := Metric{Name: "cpu", Labels: model.LabelSet{"pod": "p1"}, Value: 101.0, Timestamp: time.Now()}
	// high-value sample arrives second
	result := deduplicateMetrics([]Metric{low, high})
	if len(result) != 1 {
		t.Fatalf("expected 1 deduplicated metric, got %d", len(result))
	}
	if result[0].Value != 101.0 {
		t.Errorf("expected highest value 101.0, got %f", result[0].Value)
	}
}

func TestDeduplicateMetrics_KeepsHighestWhenHighArrivesFirst(t *testing.T) {
	high := Metric{Name: "cpu", Labels: model.LabelSet{"pod": "p1"}, Value: 101.0, Timestamp: time.Now()}
	low := Metric{Name: "cpu", Labels: model.LabelSet{"pod": "p1"}, Value: 100.0, Timestamp: time.Now().Add(-10 * time.Second)}
	// high-value sample arrives first in slice — should still keep it
	result := deduplicateMetrics([]Metric{high, low})
	if len(result) != 1 {
		t.Fatalf("expected 1 deduplicated metric, got %d", len(result))
	}
	if result[0].Value != 101.0 {
		t.Errorf("expected highest value 101.0, got %f", result[0].Value)
	}
}

// Regression test: source with a newer timestamp but marginally lower counter value
// (caused by sub-second scrape timing differences between infra Prometheus instances)
// must NOT win the dedup — the higher value must always be kept.
func TestDeduplicateMetrics_NewerTimestampLowerValueLoses(t *testing.T) {
	// Simulates the production bug: target A scraped cAdvisor at T+0.1s got 9020.6257,
	// target B scraped at T+0.3s got 9020.5701 (marginally lower due to timing).
	higherValue := Metric{Name: "container_cpu_usage_seconds_total", Labels: model.LabelSet{"pod": "p1"}, Value: 9020.6257, Timestamp: time.Now()}
	lowerValueNewerTS := Metric{Name: "container_cpu_usage_seconds_total", Labels: model.LabelSet{"pod": "p1"}, Value: 9020.5701, Timestamp: time.Now().Add(200 * time.Millisecond)}
	result := deduplicateMetrics([]Metric{higherValue, lowerValueNewerTS})
	if len(result) != 1 {
		t.Fatalf("expected 1 metric, got %d", len(result))
	}
	if result[0].Value != 9020.6257 {
		t.Errorf("must keep higher value 9020.6257, got %f — this would cause a counter reset spike", result[0].Value)
	}
}

func TestDeduplicateMetrics_13Copies(t *testing.T) {
	// Simulates 13 infra Prometheus targets all returning the same series.
	base := time.Now().Add(-time.Minute)
	var metrics []Metric
	for i := 0; i < 13; i++ {
		metrics = append(metrics, Metric{
			Name:      "container_cpu_usage_seconds_total",
			Labels:    model.LabelSet{"pod": "alertmanager-0", "container": "alertmanager"},
			Value:     float64(1000 + i),
			Timestamp: base.Add(time.Duration(i) * time.Second),
		})
	}
	result := deduplicateMetrics(metrics)
	if len(result) != 1 {
		t.Fatalf("expected 1 metric after dedup of 13 copies, got %d", len(result))
	}
	// Should keep the one with the highest value (index 12 = value 1012)
	if result[0].Value != 1012.0 {
		t.Errorf("expected highest value 1012.0, got %f", result[0].Value)
	}
}

func TestDeduplicateMetrics_DifferentMetricNamesSameLabels(t *testing.T) {
	metrics := []Metric{
		{Name: "container_cpu_usage_seconds_total", Labels: model.LabelSet{"pod": "p1"}, Value: 1.0, Timestamp: time.Now()},
		{Name: "container_memory_usage_bytes", Labels: model.LabelSet{"pod": "p1"}, Value: 2.0, Timestamp: time.Now()},
	}
	result := deduplicateMetrics(metrics)
	if len(result) != 2 {
		t.Errorf("different metric names with same labels must not be deduped, got %d", len(result))
	}
}

func contains(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// splitIntoBatches
// ---------------------------------------------------------------------------

func TestSplitIntoBatches_ExactDivision(t *testing.T) {
	metrics := make([]Metric, 10)
	batches := splitIntoBatches(metrics, 5)
	if len(batches) != 2 {
		t.Fatalf("expected 2 batches, got %d", len(batches))
	}
	for i, b := range batches {
		if len(b) != 5 {
			t.Errorf("batch[%d]: expected size 5, got %d", i, len(b))
		}
	}
}

func TestSplitIntoBatches_Remainder(t *testing.T) {
	metrics := make([]Metric, 11)
	batches := splitIntoBatches(metrics, 5)
	if len(batches) != 3 {
		t.Fatalf("expected 3 batches, got %d", len(batches))
	}
	if len(batches[0]) != 5 {
		t.Errorf("batch[0]: expected 5, got %d", len(batches[0]))
	}
	if len(batches[1]) != 5 {
		t.Errorf("batch[1]: expected 5, got %d", len(batches[1]))
	}
	if len(batches[2]) != 1 {
		t.Errorf("batch[2] (remainder): expected 1, got %d", len(batches[2]))
	}
}

func TestSplitIntoBatches_FewerThanBatchSize(t *testing.T) {
	metrics := make([]Metric, 3)
	batches := splitIntoBatches(metrics, 5)
	if len(batches) != 1 {
		t.Fatalf("expected 1 batch, got %d", len(batches))
	}
	if len(batches[0]) != 3 {
		t.Errorf("expected batch size 3, got %d", len(batches[0]))
	}
}

func TestSplitIntoBatches_ExactlyBatchSize(t *testing.T) {
	metrics := make([]Metric, 5)
	batches := splitIntoBatches(metrics, 5)
	if len(batches) != 1 {
		t.Fatalf("expected exactly 1 batch when len == batchSize, got %d", len(batches))
	}
	if len(batches[0]) != 5 {
		t.Errorf("expected batch size 5, got %d", len(batches[0]))
	}
}

// batchSize <= 0 disables batching: all metrics in a single batch (legacy behaviour).
func TestSplitIntoBatches_ZeroBatchSize_SingleBatch(t *testing.T) {
	metrics := make([]Metric, 10)
	batches := splitIntoBatches(metrics, 0)
	if len(batches) != 1 {
		t.Fatalf("batchSize=0 must return 1 batch (no-op), got %d", len(batches))
	}
	if len(batches[0]) != 10 {
		t.Errorf("expected all 10 metrics in single batch, got %d", len(batches[0]))
	}
}

func TestSplitIntoBatches_NegativeBatchSize_SingleBatch(t *testing.T) {
	metrics := make([]Metric, 7)
	batches := splitIntoBatches(metrics, -1)
	if len(batches) != 1 {
		t.Fatalf("batchSize<0 must return 1 batch (no-op), got %d", len(batches))
	}
}

// Verify that batching preserves the original order and no metrics are dropped.
func TestSplitIntoBatches_PreservesOrderAndCount(t *testing.T) {
	const n = 17
	metrics := make([]Metric, n)
	for i := range metrics {
		metrics[i] = makeMetric(fmt.Sprintf("metric_%02d", i), nil)
	}

	batches := splitIntoBatches(metrics, 5)
	// 17 / 5 = 3 full + 1 remainder = 4 batches
	if len(batches) != 4 {
		t.Fatalf("expected 4 batches for 17 metrics with batchSize=5, got %d", len(batches))
	}

	idx := 0
	for bi, batch := range batches {
		for mi, m := range batch {
			want := fmt.Sprintf("metric_%02d", idx)
			if m.Name != want {
				t.Errorf("batch[%d][%d]: got %q, want %q — order not preserved", bi, mi, m.Name, want)
			}
			idx++
		}
	}
	if idx != n {
		t.Errorf("total metrics after batching: got %d, want %d — metrics dropped or duplicated", idx, n)
	}
}

// Verify that batches are sub-slices (no copy) — mutations to the batch slice
// reflect in the original. This is intentional (avoids allocation) and must not
// regress.
func TestSplitIntoBatches_SubSliceNoCopy(t *testing.T) {
	metrics := []Metric{
		makeMetric("a", nil),
		makeMetric("b", nil),
		makeMetric("c", nil),
	}
	batches := splitIntoBatches(metrics, 2)
	if len(batches) != 2 {
		t.Fatalf("expected 2 batches, got %d", len(batches))
	}
	// Mutating batch[0][0] must be visible in the original slice.
	batches[0][0].Name = "mutated"
	if metrics[0].Name != "mutated" {
		t.Error("batch is not a sub-slice of the original: mutation did not propagate")
	}
}

// Regression test: with metricIsolation=false a large metric collection
// previously produced a single >32 MiB remote write body that Prometheus 3.x
// rejected. Verify that splitIntoBatches with the default production batch size
// (5000) correctly partitions a large collection into multiple manageable batches.
func TestSplitIntoBatches_LargeCollection_StaysUnder32MiB(t *testing.T) {
	const defaultBatchSize = 5000

	// Simulate a large metric collection similar to what metricIsolation=false
	// can return when cluster-scoped metrics have many label combinations.
	const totalMetrics = 482441
	metrics := make([]Metric, totalMetrics)
	for i := range metrics {
		metrics[i] = makeMetric("keda_scaler_active", map[string]string{
			"namespace":      fmt.Sprintf("ns-%04d", i%200),
			"scaledObject":   fmt.Sprintf("obj-%04d", i%1000),
			"scaler":         "scaler-type",
			"metric":         "metric-name",
			"scaledResource": "deployment",
		})
	}

	batches := splitIntoBatches(metrics, defaultBatchSize)

	expectedBatches := (totalMetrics + defaultBatchSize - 1) / defaultBatchSize
	if len(batches) != expectedBatches {
		t.Fatalf("expected %d batches, got %d", expectedBatches, len(batches))
	}

	// Every batch must be at most defaultBatchSize metrics.
	for i, b := range batches {
		if len(b) > defaultBatchSize {
			t.Errorf("batch[%d] exceeds batchSize: %d > %d", i, len(b), defaultBatchSize)
		}
	}

	// Last batch may be smaller (remainder).
	lastBatch := batches[len(batches)-1]
	expectedLastSize := totalMetrics % defaultBatchSize
	if expectedLastSize == 0 {
		expectedLastSize = defaultBatchSize
	}
	if len(lastBatch) != expectedLastSize {
		t.Errorf("last batch size: got %d, want %d", len(lastBatch), expectedLastSize)
	}

	// Total metric count across batches must equal the original count.
	total := 0
	for _, b := range batches {
		total += len(b)
	}
	if total != totalMetrics {
		t.Errorf("total metrics across batches: got %d, want %d", total, totalMetrics)
	}
}
