package app

import (
	"testing"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

func TestRuntimeModelsConstructsMetricRecorder(t *testing.T) {
	models := newRuntimeModels()
	if models.metrics == nil {
		t.Fatal("runtime metric recorder is nil")
	}
	models.metrics.AddCounter(domain.MetricSyntaxQueryTotal, 1, nil)
	if snapshot := models.metrics.Snapshot(); len(snapshot) != 1 || snapshot[0].Value != 1 {
		t.Fatalf("metric snapshot = %#v", snapshot)
	}
}
