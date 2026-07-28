package app

import (
	"context"
	"testing"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

func TestForegroundFacadeUsesDurableEngineAndJobIDBypassesRecursion(t *testing.T) {
	direct := &fakeExternalRuntime{result: domain.AcpInvocationResult{Text: "direct"}}
	jobs := &fakeSynchronousJobRunner{result: domain.AcpInvocationResult{Text: "durable"}}
	facade := newForegroundExternalAgentRuntime(direct, jobs)
	result, err := facade.Run(t.Context(), domain.AcpInvocationRequest{PrimaryPath: t.TempDir(), Task: "task", Actor: "U123", ConversationKey: "slack:T:dm:C"})
	if err != nil || result.Text != "durable" || jobs.calls != 1 || direct.runs != 0 {
		t.Fatalf("foreground result=%#v err=%v jobs=%d direct=%d", result, err, jobs.calls, direct.runs)
	}
	result, err = facade.Run(t.Context(), domain.AcpInvocationRequest{JobID: "job-1", PrimaryPath: t.TempDir(), Task: "worker"})
	if err != nil || result.Text != "direct" || direct.runs != 1 || jobs.calls != 1 {
		t.Fatalf("worker result=%#v err=%v jobs=%d direct=%d", result, err, jobs.calls, direct.runs)
	}
}

type fakeSynchronousJobRunner struct {
	result domain.AcpInvocationResult
	calls  int
}

func (f *fakeSynchronousJobRunner) StartAndWait(context.Context, domain.ExternalAgentJobRequest) (domain.AcpInvocationResult, error) {
	f.calls++
	return f.result, nil
}
