package remote_write

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gogo/protobuf/proto"
	"github.com/golang/snappy"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/prompb"
	"github.com/sirupsen/logrus"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/prometheus-multi-tenant-proxy/api/v1alpha1"
)

// Metric represents a single metric data point
type Metric struct {
	Name      string
	Labels    model.LabelSet
	Value     float64
	Timestamp time.Time
}

// Collector interface defines how metrics are sent to remote write targets
type Collector interface {
	// Send sends metrics to the remote write target
	Send(ctx context.Context, metricAccess *v1alpha1.MetricAccess, metrics []Metric) error
}

// PrometheusCollector sends metrics to a Prometheus instance via remote write
type PrometheusCollector struct {
	client client.Client
}

// NewPrometheusCollector creates a new Prometheus collector
func NewPrometheusCollector(client client.Client) Collector {
	return &PrometheusCollector{
		client: client,
	}
}

// Send implements the Collector interface for Prometheus targets
func (c *PrometheusCollector) Send(ctx context.Context, metricAccess *v1alpha1.MetricAccess, metrics []Metric) error {
	target := metricAccess.Spec.RemoteWrite.Prometheus
	if target == nil {
		return fmt.Errorf("prometheus target configuration is missing")
	}

	port := target.ServicePort
	if port == 0 {
		port = 9090
	}

	// Apply metric relabeling before building timeseries
	metrics = applyMetricRelabelings(metrics, metricAccess.Spec.RemoteWrite.MetricRelabelings)

	timeseries := buildTimeseries(metrics, metricAccess)

	req := &prompb.WriteRequest{
		Timeseries: timeseries,
	}
	data, err := proto.Marshal(req)
	if err != nil {
		return fmt.Errorf("failed to marshal metrics: %w", err)
	}
	compressed := snappy.Encode(nil, data)

	// Build target URLs: if Replicas > 0 and StatefulSetName is set, write to each pod
	var urls []string
	if target.Replicas > 0 && target.StatefulSetName != "" {
		for i := int32(0); i < target.Replicas; i++ {
			podURL := fmt.Sprintf("http://%s-%d.%s.%s.svc.cluster.local:%d/api/v1/write",
				target.StatefulSetName, i, target.ServiceName,
				metricAccess.Namespace, port)
			urls = append(urls, podURL)
		}
		logrus.WithFields(logrus.Fields{
			"namespace":       metricAccess.Namespace,
			"replicas":        target.Replicas,
			"statefulset":     target.StatefulSetName,
			"target_count":    len(urls),
		}).Info("Sending metrics to multiple Prometheus replicas")
	} else {
		svcURL := fmt.Sprintf("http://%s.%s.svc.cluster.local:%d/api/v1/write",
			target.ServiceName, metricAccess.Namespace, port)
		urls = append(urls, svcURL)
	}

	// Send to all target URLs concurrently
	var wg sync.WaitGroup
	errCh := make(chan error, len(urls))

	for _, targetURL := range urls {
		wg.Add(1)
		go func(u string) {
			defer wg.Done()
			if err := sendRemoteWrite(ctx, u, compressed, len(metrics)); err != nil {
				errCh <- fmt.Errorf("%s: %w", u, err)
			}
		}(targetURL)
	}

	wg.Wait()
	close(errCh)

	var errs []string
	for err := range errCh {
		errs = append(errs, err.Error())
	}
	if len(errs) > 0 {
		return fmt.Errorf("remote write failures: %s", strings.Join(errs, "; "))
	}

	logrus.WithFields(logrus.Fields{
		"namespace":     metricAccess.Namespace,
		"metric_count":  len(metrics),
		"targets":       len(urls),
	}).Info("Successfully sent metrics via remote write to all targets")
	return nil
}

// PushgatewayCollector sends metrics to a Pushgateway instance
type PushgatewayCollector struct {
	client client.Client
}

// NewPushgatewayCollector creates a new Pushgateway collector
func NewPushgatewayCollector(client client.Client) Collector {
	return &PushgatewayCollector{
		client: client,
	}
}

// Send implements the Collector interface for Pushgateway targets
func (c *PushgatewayCollector) Send(ctx context.Context, metricAccess *v1alpha1.MetricAccess, metrics []Metric) error {
	// Implementation would:
	// 1. Convert metrics to Pushgateway format
	// 2. Push to the Pushgateway instance in the tenant namespace
	// 3. Handle job naming and grouping
	
	// Placeholder implementation
	return nil
}

// RemoteWriteCollector sends metrics to a remote write endpoint
type RemoteWriteCollector struct {
	client client.Client
}

// NewRemoteWriteCollector creates a new remote write collector
func NewRemoteWriteCollector(client client.Client) Collector {
	return &RemoteWriteCollector{
		client: client,
	}
}

// Send implements the Collector interface for remote write targets
func (c *RemoteWriteCollector) Send(ctx context.Context, metricAccess *v1alpha1.MetricAccess, metrics []Metric) error {
	endpoint := metricAccess.Spec.RemoteWrite.RemoteWrite
	if endpoint == nil {
		return fmt.Errorf("remote write endpoint configuration is missing")
	}

	metrics = applyMetricRelabelings(metrics, metricAccess.Spec.RemoteWrite.MetricRelabelings)

	timeseries := buildTimeseries(metrics, metricAccess)

	req := &prompb.WriteRequest{
		Timeseries: timeseries,
	}
	data, err := proto.Marshal(req)
	if err != nil {
		return fmt.Errorf("failed to marshal metrics: %w", err)
	}
	compressed := snappy.Encode(nil, data)

	if err := sendRemoteWrite(ctx, endpoint.URL, compressed, len(metrics)); err != nil {
		return fmt.Errorf("remote write to %s failed: %w", endpoint.URL, err)
	}

	logrus.WithFields(logrus.Fields{
		"url":          endpoint.URL,
		"namespace":    metricAccess.Namespace,
		"metric_count": len(metrics),
	}).Info("Successfully sent metrics to remote write endpoint")
	return nil
}

// buildTimeseries converts Metric slice to prompb.TimeSeries applying extraLabels.
func buildTimeseries(metrics []Metric, metricAccess *v1alpha1.MetricAccess) []prompb.TimeSeries {
	var timeseries []prompb.TimeSeries
	for _, m := range metrics {
		ts := prompb.TimeSeries{
			Labels: make([]prompb.Label, 0, len(m.Labels)+1),
			Samples: []prompb.Sample{{
				Value:     m.Value,
				Timestamp: m.Timestamp.UnixNano() / int64(time.Millisecond),
			}},
		}

		ts.Labels = append(ts.Labels, prompb.Label{
			Name:  "__name__",
			Value: m.Name,
		})

		labelMap := make(map[string]string)
		for name, value := range m.Labels {
			labelMap[string(name)] = string(value)
		}

		if metricAccess.Spec.RemoteWrite.ExtraLabels != nil {
			honorLabels := metricAccess.Spec.RemoteWrite.HonorLabels
			for name, value := range metricAccess.Spec.RemoteWrite.ExtraLabels {
				if _, exists := labelMap[name]; exists && honorLabels {
					continue
				}
				labelMap[name] = value
			}
		}

		for name, value := range labelMap {
			ts.Labels = append(ts.Labels, prompb.Label{
				Name:  name,
				Value: value,
			})
		}

		timeseries = append(timeseries, ts)
	}
	return timeseries
}

// sendRemoteWrite sends a compressed protobuf payload to a remote write URL with retries.
func sendRemoteWrite(ctx context.Context, targetURL string, compressed []byte, metricCount int) error {
	httpClient := &http.Client{Timeout: 30 * time.Second}
	var lastErr error

	for retries := 0; retries < 3; retries++ {
		httpReq, err := http.NewRequestWithContext(ctx, "POST", targetURL, bytes.NewReader(compressed))
		if err != nil {
			return fmt.Errorf("failed to create request: %w", err)
		}
		httpReq.Header.Set("Content-Type", "application/x-protobuf")
		httpReq.Header.Set("Content-Encoding", "snappy")
		httpReq.Header.Set("X-Prometheus-Remote-Write-Version", "0.1.0")

		resp, err := httpClient.Do(httpReq)
		if err != nil {
			lastErr = fmt.Errorf("request failed: %w", err)
			logrus.WithFields(logrus.Fields{
				"url":   targetURL,
				"retry": retries + 1,
				"error": err,
			}).Warning("Remote write request failed, retrying")
			time.Sleep(time.Second * time.Duration(retries+1))
			continue
		}
		defer resp.Body.Close()

		if resp.StatusCode/100 != 2 {
			body, _ := io.ReadAll(resp.Body)
			lastErr = fmt.Errorf("status %d: %s", resp.StatusCode, string(body))
			logrus.WithFields(logrus.Fields{
				"url":      targetURL,
				"status":   resp.StatusCode,
				"response": string(body),
				"retry":    retries + 1,
			}).Warning("Remote write response error, retrying")
			time.Sleep(time.Second * time.Duration(retries+1))
			continue
		}

		return nil
	}

	return fmt.Errorf("failed after retries: %w", lastErr)
}

// applyMetricRelabelings applies MetricRelabelConfig rules to a set of metrics.
// Supports actions: replace (default), keep, drop, labeldrop, labelkeep.
func applyMetricRelabelings(metrics []Metric, rules []v1alpha1.MetricRelabelConfig) []Metric {
	if len(rules) == 0 {
		return metrics
	}

	var result []Metric
	for _, m := range metrics {
		labels := make(map[string]string)
		labels["__name__"] = m.Name
		for k, v := range m.Labels {
			labels[string(k)] = string(v)
		}

		keep := true
		for _, rule := range rules {
			action := rule.Action
			if action == "" {
				action = "replace"
			}

			separator := rule.Separator
			if separator == "" {
				separator = ";"
			}

			// Concatenate source label values
			var sourceValues []string
			for _, sl := range rule.SourceLabels {
				sourceValues = append(sourceValues, labels[sl])
			}
			concatenated := strings.Join(sourceValues, separator)

			regexStr := rule.Regex
			if regexStr == "" {
				regexStr = "(.*)"
			}
			re, err := regexp.Compile("^(?:" + regexStr + ")$")
			if err != nil {
				logrus.WithFields(logrus.Fields{
					"regex": regexStr,
					"error": err,
				}).Warning("Invalid relabeling regex, skipping rule")
				continue
			}

			switch action {
			case "replace":
				if re.MatchString(concatenated) {
					replacement := rule.Replacement
					if replacement == "" {
						replacement = "$1"
					}
					newValue := re.ReplaceAllString(concatenated, replacement)
					if rule.TargetLabel != "" {
						labels[rule.TargetLabel] = newValue
					}
				}
			case "keep":
				if !re.MatchString(concatenated) {
					keep = false
				}
			case "drop":
				if re.MatchString(concatenated) {
					keep = false
				}
			case "labeldrop":
				for lk := range labels {
					if re.MatchString(lk) {
						delete(labels, lk)
					}
				}
			case "labelkeep":
				for lk := range labels {
					if !re.MatchString(lk) && lk != "__name__" {
						delete(labels, lk)
					}
				}
			}

			if !keep {
				break
			}
		}

		if !keep {
			continue
		}

		newMetric := Metric{
			Name:      labels["__name__"],
			Value:     m.Value,
			Timestamp: m.Timestamp,
			Labels:    model.LabelSet{},
		}
		for k, v := range labels {
			if k != "__name__" {
				newMetric.Labels[model.LabelName(k)] = model.LabelValue(v)
			}
		}
		result = append(result, newMetric)
	}

	if len(result) != len(metrics) {
		logrus.WithFields(logrus.Fields{
			"original_count":  len(metrics),
			"relabeled_count": len(result),
			"rules_applied":   len(rules),
		}).Info("Metric relabeling filtered metrics")
	}

	return result
} 