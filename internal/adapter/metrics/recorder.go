// Package metrics provides the bounded in-process metric recorder used by the
// application composition root.
package metrics

import (
	"math"
	"sort"
	"strings"
	"sync"

	"github.com/Dauno/slack-local-agent/internal/port"
)

const (
	maxLabelValueLength   = 64
	maxObservationSamples = 4096
)

var allowedLabelKeys = map[string]struct{}{
	"counter_strategy":   {},
	"guard_outcome":      {},
	"reduction_reason":   {},
	"result_kind":        {},
	"continuity_outcome": {},
	"profile_id":         {},
	"lsp_server_id":      {},
	"language":           {},
	"engine_id":          {},
	"failure_category":   {},
	"query_id":           {},
}

type key struct {
	name   string
	kind   port.MetricKind
	labels string
}

type sample struct {
	name   string
	kind   port.MetricKind
	labels port.MetricLabels
	value  float64
}

type Recorder struct {
	mu              sync.RWMutex
	counters        map[key]*sample
	gauges          map[key]*sample
	observed        []sample
	nextObservation int
}

var _ port.MetricRecorder = (*Recorder)(nil)

func NewRecorder() *Recorder {
	return &Recorder{counters: make(map[key]*sample), gauges: make(map[key]*sample)}
}

func (r *Recorder) AddCounter(name string, delta int64, labels port.MetricLabels) {
	if r == nil || strings.TrimSpace(name) == "" || delta < 0 {
		return
	}
	clean := sanitizeLabels(labels)
	k := metricKey(name, port.MetricKindCounter, clean)
	r.mu.Lock()
	defer r.mu.Unlock()
	entry := r.counters[k]
	if entry == nil {
		entry = &sample{name: name, kind: port.MetricKindCounter, labels: clean}
		r.counters[k] = entry
	}
	entry.value += float64(delta)
}

func (r *Recorder) SetGauge(name string, value int64, labels port.MetricLabels) {
	if r == nil || strings.TrimSpace(name) == "" {
		return
	}
	clean := sanitizeLabels(labels)
	k := metricKey(name, port.MetricKindGauge, clean)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gauges[k] = &sample{name: name, kind: port.MetricKindGauge, labels: clean, value: float64(value)}
}

func (r *Recorder) Observe(name string, value float64, labels port.MetricLabels) {
	if r == nil || strings.TrimSpace(name) == "" || math.IsNaN(value) || math.IsInf(value, 0) {
		return
	}
	clean := sanitizeLabels(labels)
	r.mu.Lock()
	if len(r.observed) < maxObservationSamples {
		r.observed = append(r.observed, sample{name: name, kind: port.MetricKindObservation, labels: clean, value: value})
	} else {
		r.observed[r.nextObservation] = sample{name: name, kind: port.MetricKindObservation, labels: clean, value: value}
		r.nextObservation = (r.nextObservation + 1) % maxObservationSamples
	}
	r.mu.Unlock()
}

func (r *Recorder) Snapshot() []port.MetricSample {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	samples := make([]sample, 0, len(r.counters)+len(r.gauges)+len(r.observed))
	for _, entry := range r.counters {
		samples = append(samples, cloneSample(*entry))
	}
	for _, entry := range r.gauges {
		samples = append(samples, cloneSample(*entry))
	}
	samples = append(samples, r.observed...)
	sort.SliceStable(samples, func(i, j int) bool {
		if samples[i].name != samples[j].name {
			return samples[i].name < samples[j].name
		}
		if samples[i].kind != samples[j].kind {
			return samples[i].kind < samples[j].kind
		}
		leftLabels, rightLabels := labelsKey(samples[i].labels), labelsKey(samples[j].labels)
		if leftLabels != rightLabels {
			return leftLabels < rightLabels
		}
		return samples[i].value < samples[j].value
	})
	result := make([]port.MetricSample, 0, len(samples))
	for _, entry := range samples {
		result = append(result, port.MetricSample{Name: entry.name, Kind: entry.kind, Value: entry.value, Labels: port.CloneMetricLabels(entry.labels)})
	}
	return result
}

func sanitizeLabels(labels port.MetricLabels) port.MetricLabels {
	if len(labels) == 0 {
		return nil
	}
	clean := make(port.MetricLabels)
	for key, value := range labels {
		if _, ok := allowedLabelKeys[key]; !ok {
			continue
		}
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if len(value) > maxLabelValueLength {
			value = value[:maxLabelValueLength]
		}
		clean[key] = value
	}
	return clean
}

func metricKey(name string, kind port.MetricKind, labels port.MetricLabels) key {
	return key{name: name, kind: kind, labels: labelsKey(labels)}
}

func labelsKey(labels port.MetricLabels) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var builder strings.Builder
	for _, key := range keys {
		builder.WriteString(key)
		builder.WriteByte('=')
		builder.WriteString(labels[key])
		builder.WriteByte('\x00')
	}
	return builder.String()
}

func cloneSample(value sample) sample {
	value.labels = port.CloneMetricLabels(value.labels)
	return value
}
