package metrics

import (
	"strings"
	"testing"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

func TestRecorderAggregatesCountersAndDropsForbiddenLabels(t *testing.T) {
	recorder := NewRecorder()
	recorder.AddCounter(domain.MetricModelRequestTokens, 3, port.MetricLabels{
		"counter_strategy": "byte_bound",
		"path":             "secret.go",
	})
	recorder.AddCounter(domain.MetricModelRequestTokens, 2, port.MetricLabels{"counter_strategy": "byte_bound"})

	snapshot := recorder.Snapshot()
	if len(snapshot) != 1 {
		t.Fatalf("snapshot length = %d, want 1", len(snapshot))
	}
	if snapshot[0].Name != domain.MetricModelRequestTokens || snapshot[0].Value != 5 {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	if _, ok := snapshot[0].Labels["path"]; ok {
		t.Fatal("forbidden path label was recorded")
	}
}

func TestRecorderSnapshotsAreDeterministicAndConcurrent(t *testing.T) {
	recorder := NewRecorder()
	const workers = 8
	const increments = 100
	done := make(chan struct{}, workers)
	for range workers {
		go func() {
			for range increments {
				recorder.AddCounter(domain.MetricSyntaxQueryTotal, 1, port.MetricLabels{"language": "go"})
			}
			done <- struct{}{}
		}()
	}
	for range workers {
		<-done
	}

	snapshot := recorder.Snapshot()
	if len(snapshot) != 1 || snapshot[0].Value != workers*increments {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	if got := recorder.Snapshot(); len(got) != len(snapshot) || got[0].Value != snapshot[0].Value {
		t.Fatalf("snapshot is not stable: %#v then %#v", snapshot, got)
	}
}

func TestNoopRecorderAcceptsEveryOperation(t *testing.T) {
	var recorder port.NoopMetricRecorder
	recorder.AddCounter("metric", 1, nil)
	recorder.SetGauge("metric", 1, nil)
	recorder.Observe("metric", 1, nil)
	if got := recorder.Snapshot(); got != nil {
		t.Fatalf("noop snapshot = %#v, want nil", got)
	}
}

func TestRecorderBoundsAndSortsObservations(t *testing.T) {
	recorder := NewRecorder()
	for value := maxObservationSamples + 100; value > 0; value-- {
		recorder.Observe(domain.MetricContextCompileDuration, float64(value), nil)
	}
	snapshot := recorder.Snapshot()
	if len(snapshot) != maxObservationSamples {
		t.Fatalf("snapshot length = %d, want %d", len(snapshot), maxObservationSamples)
	}
	for index := 1; index < len(snapshot); index++ {
		if snapshot[index-1].Value > snapshot[index].Value {
			t.Fatalf("observations are not deterministically sorted at %d", index)
		}
	}
	if snapshot[0].Value != 1 || snapshot[len(snapshot)-1].Value != maxObservationSamples {
		t.Fatalf("bounded observations retained stale values: first=%v last=%v", snapshot[0].Value, snapshot[len(snapshot)-1].Value)
	}
}

// TestIdentityAndSuppressionCountersAreAllowlistedAndSensitiveFree pins the
// P2-02 contract: both new counters are forced label-free at the recorder
// boundary (per-metric empty allowlist), so even globally-allowed label keys
// such as failure_category or delivery_mode are rejected, and their names
// carry no job, actor, conversation, digest, reference, or content value.
func TestIdentityAndSuppressionCountersAreAllowlistedAndSensitiveFree(t *testing.T) {
	recorder := NewRecorder()
	recorder.AddCounter(domain.MetricExternalAgentResultIdentityInvalidTotal, 3, port.MetricLabels{
		"job_id":           "secret-job",
		"actor":            "secret-actor",
		"conversation":     "secret-conversation",
		"result_sha256":    "secret-digest",
		"artifact_ref":     "secret-ref",
		"failure_category": "validation",
		"delivery_mode":    "markdown",
	})
	recorder.AddCounter(domain.MetricExternalAgentActivationSuppressionTotal, 2, port.MetricLabels{
		"job_id": "secret-job",
	})

	snapshot := recorder.Snapshot()
	byName := make(map[string]float64, len(snapshot))
	for _, sample := range snapshot {
		byName[sample.Name] += sample.Value
		for key, value := range sample.Labels {
			t.Fatalf("label key %q=%q survived in %s; identity counters must be label-free", key, value, sample.Name)
		}
		if strings.Contains(sample.Name, "secret") {
			t.Fatalf("metric name carries a sensitive value: %q", sample.Name)
		}
	}
	if byName[domain.MetricExternalAgentResultIdentityInvalidTotal] != 3 {
		t.Fatalf("identity counter = %v, want 3", byName[domain.MetricExternalAgentResultIdentityInvalidTotal])
	}
	if byName[domain.MetricExternalAgentActivationSuppressionTotal] != 2 {
		t.Fatalf("suppression counter = %v, want 2", byName[domain.MetricExternalAgentActivationSuppressionTotal])
	}
	for _, name := range []string{domain.MetricExternalAgentResultIdentityInvalidTotal, domain.MetricExternalAgentActivationSuppressionTotal} {
		for _, token := range []string{"job", "actor", "conversation", "digest", "sha256", "ref", "content", "slack:"} {
			if strings.Contains(name, token) {
				t.Fatalf("metric name %q contains sensitive token %q", name, token)
			}
		}
	}
}

// TestIdentityCountersRejectAnyLabelAtTheBoundary pins CR8: the empty
// per-metric allowlist is enforced by the recorder itself, so an attempt to
// record labels on the P2-02 counters (including keys allowed globally for
// other metrics) is rejected and the counter stays label-free.
func TestIdentityCountersRejectAnyLabelAtTheBoundary(t *testing.T) {
	recorder := NewRecorder()
	for _, name := range []string{domain.MetricExternalAgentResultIdentityInvalidTotal, domain.MetricExternalAgentActivationSuppressionTotal} {
		recorder.AddCounter(name, 1, port.MetricLabels{"failure_category": "validation"})
		recorder.AddCounter(name, 1, port.MetricLabels{"delivery_mode": "file"})
		recorder.AddCounter(name, 1, port.MetricLabels{"future_reason": "integrity"})
	}
	snapshot := recorder.Snapshot()
	if len(snapshot) != 2 {
		t.Fatalf("snapshot length = %d, want 2 label-free samples; snapshot = %#v", len(snapshot), snapshot)
	}
	for _, sample := range snapshot {
		if len(sample.Labels) != 0 {
			t.Fatalf("sample %s kept labels %#v; the per-metric allowlist must be empty", sample.Name, sample.Labels)
		}
	}
}
