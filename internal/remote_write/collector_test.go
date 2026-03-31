package remote_write

import (
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
	got := resolveTargetNamespace(target, "ns-team-etel-integration")
	if got != "ns-team-etel-integration" {
		t.Errorf("expected CR namespace, got %q", got)
	}
}

func TestResolveTargetNamespace_WithOverride(t *testing.T) {
	target := &v1alpha1.PrometheusTarget{
		ServiceName:     "prometheus-operated",
		TargetNamespace: "ns-team-enm-integration",
	}
	got := resolveTargetNamespace(target, "ns-team-etel-integration")
	if got != "ns-team-enm-integration" {
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
	urls := buildPrometheusURLs(target, "ns-team-enm-integration")
	if len(urls) != 1 {
		t.Fatalf("expected 1 URL, got %d", len(urls))
	}
	want := "http://prometheus-operated.ns-team-enm-integration.svc.cluster.local:9090/api/v1/write"
	if urls[0] != want {
		t.Errorf("got %q, want %q", urls[0], want)
	}
}

func TestBuildPrometheusURLs_DefaultPort(t *testing.T) {
	target := &v1alpha1.PrometheusTarget{
		ServiceName: "prometheus-operated",
		// ServicePort intentionally zero — should default to 9090
	}
	urls := buildPrometheusURLs(target, "ns-team-enm-integration")
	want := "http://prometheus-operated.ns-team-enm-integration.svc.cluster.local:9090/api/v1/write"
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
		StatefulSetName: "prometheus-enm-promoperator-prometheus",
	}
	urls := buildPrometheusURLs(target, "ns-team-enm-integration")
	if len(urls) != 2 {
		t.Fatalf("expected 2 URLs, got %d", len(urls))
	}
	wants := []string{
		"http://prometheus-enm-promoperator-prometheus-0.prometheus-operated.ns-team-enm-integration.svc.cluster.local:9090/api/v1/write",
		"http://prometheus-enm-promoperator-prometheus-1.prometheus-operated.ns-team-enm-integration.svc.cluster.local:9090/api/v1/write",
	}
	for i, want := range wants {
		if urls[i] != want {
			t.Errorf("url[%d]: got %q, want %q", i, urls[i], want)
		}
	}
}

// ---------------------------------------------------------------------------
// Cross-namespace: secondary NS CR → primary NS Prometheus
// This is the exact scenario Alex reported: etel-integration CR must push
// to Prometheus in enm-integration.
// ---------------------------------------------------------------------------

func TestBuildPrometheusURLs_CrossNamespace_ServiceURL(t *testing.T) {
	target := &v1alpha1.PrometheusTarget{
		ServiceName:     "prometheus-operated",
		ServicePort:     9090,
		TargetNamespace: "ns-team-enm-integration",
	}
	// targetNS resolved by resolveTargetNamespace (already tested above)
	targetNS := resolveTargetNamespace(target, "ns-team-etel-integration")
	urls := buildPrometheusURLs(target, targetNS)

	if len(urls) != 1 {
		t.Fatalf("expected 1 URL, got %d", len(urls))
	}
	want := "http://prometheus-operated.ns-team-enm-integration.svc.cluster.local:9090/api/v1/write"
	if urls[0] != want {
		t.Errorf("got %q, want %q", urls[0], want)
	}
}

func TestBuildPrometheusURLs_CrossNamespace_MultiReplica(t *testing.T) {
	target := &v1alpha1.PrometheusTarget{
		ServiceName:     "prometheus-operated",
		ServicePort:     9090,
		Replicas:        2,
		StatefulSetName: "prometheus-enm-promoperator-prometheus",
		TargetNamespace: "ns-team-enm-integration",
	}
	targetNS := resolveTargetNamespace(target, "ns-team-etel-integration")
	urls := buildPrometheusURLs(target, targetNS)

	if len(urls) != 2 {
		t.Fatalf("expected 2 URLs, got %d", len(urls))
	}
	wants := []string{
		"http://prometheus-enm-promoperator-prometheus-0.prometheus-operated.ns-team-enm-integration.svc.cluster.local:9090/api/v1/write",
		"http://prometheus-enm-promoperator-prometheus-1.prometheus-operated.ns-team-enm-integration.svc.cluster.local:9090/api/v1/write",
	}
	for i, want := range wants {
		if urls[i] != want {
			t.Errorf("url[%d]: got %q, want %q", i, urls[i], want)
		}
	}
}

// Ensure that setting TargetNamespace == CR namespace is identical to not setting it.
func TestBuildPrometheusURLs_TargetNamespaceSameAsCR(t *testing.T) {
	withOverride := &v1alpha1.PrometheusTarget{
		ServiceName:     "prometheus-operated",
		ServicePort:     9090,
		TargetNamespace: "ns-team-enm-integration",
	}
	withoutOverride := &v1alpha1.PrometheusTarget{
		ServiceName: "prometheus-operated",
		ServicePort: 9090,
	}
	ns := "ns-team-enm-integration"
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
// MetricAccess spec wiring: verify TargetNamespace flows through the type
// ---------------------------------------------------------------------------

func TestMetricAccessSpec_TargetNamespace(t *testing.T) {
	ma := &v1alpha1.MetricAccess{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "enm-ns-team-etel-integration",
			Namespace: "ns-team-etel-integration",
		},
		Spec: v1alpha1.MetricAccessSpec{
			Source:  "ns-team-etel-integration",
			Metrics: []string{"container_cpu_usage_seconds_total"},
			RemoteWrite: &v1alpha1.RemoteWriteConfig{
				Enabled: true,
				Target:  v1alpha1.RemoteWriteTarget{Type: "prometheus"},
				Prometheus: &v1alpha1.PrometheusTarget{
					ServiceName:     "prometheus-operated",
					ServicePort:     9090,
					Replicas:        2,
					StatefulSetName: "prometheus-enm-promoperator-prometheus",
					TargetNamespace: "ns-team-enm-integration",
				},
			},
		},
	}

	target := ma.Spec.RemoteWrite.Prometheus
	targetNS := resolveTargetNamespace(target, ma.Namespace)
	if targetNS != "ns-team-enm-integration" {
		t.Errorf("targetNS: got %q, want ns-team-enm-integration", targetNS)
	}

	urls := buildPrometheusURLs(target, targetNS)
	if len(urls) != 2 {
		t.Fatalf("expected 2 pod URLs, got %d", len(urls))
	}
	for _, u := range urls {
		if !contains(u, "ns-team-enm-integration") {
			t.Errorf("URL %q does not contain primary namespace", u)
		}
		if contains(u, "ns-team-etel-integration") {
			t.Errorf("URL %q must not contain secondary CR namespace", u)
		}
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && containsHelper(s, sub))
}

func containsHelper(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
