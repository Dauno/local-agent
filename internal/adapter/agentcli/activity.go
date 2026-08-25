package agentcli

import "context"

// ActivityKind classifies one observable step of an agent CLI run. The set is
// host-owned and content-free. Nothing the CLI writes reaches a reporter,
// except a type name the descriptor already declared in `report_types`.
type ActivityKind string

const (
	// ActivityProcessStarted is reported once, after the process exists.
	ActivityProcessStarted ActivityKind = "process_started"
	// ActivityStep is reported for each event the descriptor selects through
	// `stream.activity.report_types`.
	ActivityStep ActivityKind = "step"
)

// Activity is one reported step of a run. Step holds the descriptor-declared
// type name and is empty unless Kind is ActivityStep. PID is set only for
// ActivityProcessStarted.
type Activity struct {
	Kind ActivityKind
	Step string
	PID  int
}

// ActivityReporter receives activity from a running agent CLI. It runs on the
// stdout reader goroutine, so it must not block: a slow reporter stalls the
// CLI itself.
type ActivityReporter func(Activity)

type activityKey struct{}

// WithActivityReporter returns a context that reports agent CLI activity to
// report. A durable job uses it to keep its progress projection fresh. An
// in-session call leaves it unset, and the adapter only writes a debug log.
//
// The reporter travels in the context because model.LLM fixes the call
// signature, and an agent CLI is reached through that interface like any other
// model.
func WithActivityReporter(ctx context.Context, report ActivityReporter) context.Context {
	if report == nil {
		return ctx
	}
	return context.WithValue(ctx, activityKey{}, report)
}

func activityReporterFrom(ctx context.Context) ActivityReporter {
	report, _ := ctx.Value(activityKey{}).(ActivityReporter)
	return report
}
