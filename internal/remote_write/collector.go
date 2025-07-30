package remote_write

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
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
	// Get the Prometheus target configuration
	target := metricAccess.Spec.RemoteWrite.Prometheus
	if target == nil {
		return fmt.Errorf("prometheus target configuration is missing")
	}
	
	// Construct the remote write URL
	port := target.ServicePort
	if port == 0 {
		port = 9090 // Default Prometheus port
	}
	
	url := fmt.Sprintf("http://%s.%s.svc.cluster.local:%d/api/v1/write", 
		target.ServiceName, 
		metricAccess.Namespace, 
		port)
	
	logrus.WithFields(logrus.Fields{
		"url":           url,
		"namespace":     metricAccess.Namespace,
		"service":       target.ServiceName,
		"port":          port,
		"metric_count":  len(metrics),
	}).Info("DIAGNOSTIC: Sending metrics via remote write to Prometheus")
	
	// Convert metrics to Prometheus remote write format
	var timeseries []prompb.TimeSeries
	for _, m := range metrics {
		ts := prompb.TimeSeries{
			Labels: make([]prompb.Label, 0, len(m.Labels)+1),
			Samples: []prompb.Sample{{
				Value:     m.Value,
				Timestamp: m.Timestamp.UnixNano() / int64(time.Millisecond),
			}},
		}

		// Add metric name
		ts.Labels = append(ts.Labels, prompb.Label{
			Name:  "__name__",
			Value: m.Name,
		})

		// Add all labels
		for name, value := range m.Labels {
			ts.Labels = append(ts.Labels, prompb.Label{
				Name:  string(name),
				Value: string(value),
			})
		}

		// Add extra labels from MetricAccess if specified
		if metricAccess.Spec.RemoteWrite.ExtraLabels != nil {
			for name, value := range metricAccess.Spec.RemoteWrite.ExtraLabels {
				ts.Labels = append(ts.Labels, prompb.Label{
					Name:  name,
					Value: value,
				})
			}
		}

		timeseries = append(timeseries, ts)
	}

	// Create WriteRequest
	req := &prompb.WriteRequest{
		Timeseries: timeseries,
	}

	// Marshal and compress
	data, err := proto.Marshal(req)
	if err != nil {
		return fmt.Errorf("failed to marshal metrics: %w", err)
	}
	compressed := snappy.Encode(nil, data)

	// Create HTTP request
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(compressed))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	// Set headers
	httpReq.Header.Set("Content-Type", "application/x-protobuf")
	httpReq.Header.Set("Content-Encoding", "snappy")
	httpReq.Header.Set("X-Prometheus-Remote-Write-Version", "0.1.0")

	// Send request with retries
	client := &http.Client{
		Timeout: 30 * time.Second,
	}

	var lastErr error
	for retries := 0; retries < 3; retries++ {
		resp, err := client.Do(httpReq)
		if err != nil {
			lastErr = fmt.Errorf("failed to send request: %w", err)
			logrus.WithFields(logrus.Fields{
				"url":     url,
				"retry":   retries + 1,
				"error":   err,
			}).Warning("DIAGNOSTIC: Remote write request failed, retrying")
			time.Sleep(time.Second * time.Duration(retries+1))
			continue
		}
		defer resp.Body.Close()

		if resp.StatusCode/100 != 2 {
			body, _ := io.ReadAll(resp.Body)
			lastErr = fmt.Errorf("remote write failed with status %d: %s", resp.StatusCode, string(body))
			logrus.WithFields(logrus.Fields{
				"url":        url,
				"status":     resp.StatusCode,
				"response":   string(body),
				"retry":      retries + 1,
			}).Warning("DIAGNOSTIC: Remote write response error, retrying")
			time.Sleep(time.Second * time.Duration(retries+1))
			continue
		}

		// Success
		logrus.WithFields(logrus.Fields{
			"url":           url,
			"namespace":     metricAccess.Namespace,
			"service":       target.ServiceName,
			"metric_count": len(metrics),
			"status":        resp.StatusCode,
		}).Info("DIAGNOSTIC: Successfully sent metrics via remote write")
		return nil
	}

	logrus.WithFields(logrus.Fields{
		"url":           url,
		"namespace":     metricAccess.Namespace,
		"service":       target.ServiceName,
		"metric_count":  len(metrics),
		"final_error":   lastErr,
	}).Error("DIAGNOSTIC: Failed to send metrics after all retries")

	return fmt.Errorf("failed to send metrics after retries: %w", lastErr)
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
	// Implementation would:
	// 1. Convert metrics to Prometheus remote write format
	// 2. Send to the specified remote write endpoint
	// 3. Handle authentication, headers, and retries
	
	// Placeholder implementation
	return nil
} 