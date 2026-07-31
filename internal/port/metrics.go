package port

import "sort"

// MetricLabels contains only bounded, non-sensitive dimensions. Implementations
// must ignore keys that are not part of the allowlist.
type MetricLabels map[string]string

type MetricKind string

const (
	MetricKindCounter     MetricKind = "counter"
	MetricKindGauge       MetricKind = "gauge"
	MetricKindObservation MetricKind = "observation"
)

type MetricSample struct {
	Name   string
	Kind   MetricKind
	Value  float64
	Labels MetricLabels
}

// MetricRecorder is deliberately provider-neutral. Recording is observational
// and never returns an error to the primary operation.
type MetricRecorder interface {
	AddCounter(name string, delta int64, labels MetricLabels)
	SetGauge(name string, value int64, labels MetricLabels)
	Observe(name string, value float64, labels MetricLabels)
	Snapshot() []MetricSample
}

// NoopMetricRecorder is useful when instrumentation is disabled or unavailable.
type NoopMetricRecorder struct{}

func (NoopMetricRecorder) AddCounter(string, int64, MetricLabels) {}
func (NoopMetricRecorder) SetGauge(string, int64, MetricLabels)   {}
func (NoopMetricRecorder) Observe(string, float64, MetricLabels)  {}
func (NoopMetricRecorder) Snapshot() []MetricSample               { return nil }

// CloneMetricLabels returns a stable copy suitable for snapshots.
func CloneMetricLabels(labels MetricLabels) MetricLabels {
	if len(labels) == 0 {
		return nil
	}
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make(MetricLabels, len(keys))
	for _, key := range keys {
		result[key] = labels[key]
	}
	return result
}
