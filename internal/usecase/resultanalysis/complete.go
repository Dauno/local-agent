package resultanalysis

import (
	"context"
	"fmt"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

// CompletionInput assembles everything CompleteTerminalAnalysis needs from
// the root reduction step's already-validated terminal packet plus the
// analysis identity. The packet's payload is not re-derived here: the
// caller (the reduction runner that just completed the root step) already
// wrote it durably; CompleteTerminalAnalysis only re-encodes it once more to
// recompute its digest and byte count for the completion transaction, so
// the two writers can never disagree about what the root step actually
// stored.
type CompletionInput struct {
	AnalysisID     string
	Scope          domain.ResultScope
	SourceResultID string
	SourceBytes    int64
	PromptVersion  string
	Packet         domain.AnalysisPacket
}

// CompleteTerminalAnalysis enforces every completion precondition TRD 07's
// Completion section requires before committing the atomic completion
// transaction: every declared step must be domain.AnalysisStepCompleted (no
// step in an unknown or still-pending state), the terminal packet's own
// coverage must be complete, and the packet must pass
// domain.AnalysisPacket.Validate() in full. Any of these failing returns a
// typed error wrapping domain.ErrAnalysisValidation and never calls the
// completion store, so an incomplete analysis is never marked completed.
func CompleteTerminalAnalysis(ctx context.Context, store port.AnalysisCompletionStore, steps []port.AnalysisStep, input CompletionInput, now time.Time) error {
	for _, step := range steps {
		if step.State != domain.AnalysisStepCompleted {
			return fmt.Errorf("%w: %s: step %s is not completed", domain.ErrAnalysisValidation, domain.AnalysisFailureCoverageIncomplete, step.StepID)
		}
	}
	if !input.Packet.Coverage.Complete {
		return fmt.Errorf("%w: %s: terminal packet coverage is incomplete", domain.ErrAnalysisValidation, domain.AnalysisFailureCoverageIncomplete)
	}
	if err := input.Packet.Validate(); err != nil {
		return err
	}
	payload, err := EncodePacketPayload(input.Packet)
	if err != nil {
		return fmt.Errorf("%w: encode terminal packet for completion: %v", domain.ErrAnalysisValidation, err)
	}
	if len(payload) > 65536 {
		return fmt.Errorf("%w: %s: terminal packet payload exceeds the step output bound", domain.ErrAnalysisValidation, domain.AnalysisFailureReductionIrreducible)
	}

	completion := port.AnalysisCompletionInput{
		AnalysisID:     input.AnalysisID,
		Scope:          input.Scope,
		SourceResultID: input.SourceResultID,
		SourceSHA256:   input.Packet.SourceSHA256,
		SourceBytes:    input.SourceBytes,
		PacketSHA256:   hexSHA256(payload),
		PacketBytes:    int64(len(payload)),
		PromptVersion:  input.PromptVersion,
	}
	return store.CompleteAnalysis(ctx, completion, now)
}
