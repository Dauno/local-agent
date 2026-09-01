package bot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

var _ port.ExternalAgentJobCompletionHandler = (*Service)(nil)

var (
	errAssistantExchangeUnavailable = errors.New("assistant exchange writer is unavailable")
	errActivationStoreUnavailable   = errors.New("external-agent activation store is unavailable")
	errActivationJobIdentityInvalid = errors.New("external-agent activation job identity is invalid")
)

func activationStateConflict(err error) error {
	return port.NewActivationProcessError("activation_state_conflict", true, err)
}

type modelRequestDiagnostic interface {
	ModelRequestStage() string
	ModelRequestCode() string
}

func activationModelDiagnostics(err error) (string, string, string) {
	stage, code := "runtime", "unclassified"
	var diagnostic modelRequestDiagnostic
	if errors.As(err, &diagnostic) {
		stage = diagnostic.ModelRequestStage()
		code = diagnostic.ModelRequestCode()
	} else if errors.Is(err, context.DeadlineExceeded) {
		stage, code = "runtime", "deadline_exceeded"
	} else if errors.Is(err, context.Canceled) {
		stage, code = "runtime", "canceled"
	}
	types := make([]string, 0, 6)
	for current := err; current != nil && len(types) < cap(types); current = errors.Unwrap(current) {
		types = append(types, fmt.Sprintf("%T", current))
	}
	return stage, code, strings.Join(types, " > ")
}

// HandleJobCompletion runs the root turn for one already-published terminal
// notification. The activation is the only source of actor and destination
// identity; no Slack event is accepted at this boundary.
func (s *Service) HandleJobCompletion(ctx context.Context, activation domain.ExternalAgentJobActivation) error {
	if s == nil || s.activationStore == nil {
		return port.NewActivationProcessError("activation_store_unavailable", true, errActivationStoreUnavailable)
	}
	current, err := s.authoritativeActivation(ctx, activation)
	if err != nil {
		return err
	}
	switch current.State {
	case domain.ActivationResponsePrepared, domain.ActivationCompleted, domain.ActivationModelStarted:
		return s.ReconcileJobCompletion(ctx, *current)
	case domain.ActivationProcessing:
		return s.runJobCompletion(ctx, current)
	case domain.ActivationPending:
		return port.NewActivationProcessError("activation_not_claimed", true, errors.New("external-agent activation is not claimed"))
	case domain.ActivationCompletionUnknown, domain.ActivationFailed:
		return nil
	default:
		return port.NewActivationProcessError("activation_identity_invalid", false, errors.New("external-agent activation state is invalid"))
	}
}

// PublishActivationFallback publishes the host-owned terminal update for an
// activation whose integrated response could not be produced or published.
// The fallback assistant exchange is staged durably with a deterministic
// intent/correlation identity before Slack is contacted, so a crash between
// publish and persist reconciles the already-published message by identity
// instead of republishing it. It is claimed and reconciled through the
// activation worker like any other outbox work.
func (s *Service) PublishActivationFallback(ctx context.Context, supplied domain.ExternalAgentJobActivation) error {
	if s == nil || s.activationStore == nil {
		return port.NewActivationProcessError("activation_store_unavailable", true, errActivationStoreUnavailable)
	}
	fallbackStore, ok := s.activationStore.(port.ExternalAgentJobActivationFallbackStore)
	if !ok {
		return port.NewActivationProcessError("activation_store_unavailable", true, errors.New("external-agent activation fallback store is unavailable"))
	}
	current, err := s.authoritativeActivation(ctx, supplied)
	if err != nil {
		return err
	}
	if current.FallbackSlackTS != "" || !current.FallbackRequired || !domain.ActivationFallbackRequired(current.LastErrorCode) {
		return nil
	}
	releaseConversation, acquired := s.limiter.TryAcquire(string(current.ConversationKey))
	if !acquired {
		return port.NewActivationProcessError("conversation_busy", true, errConversationBusy)
	}
	defer releaseConversation()
	metadata, channelKind, err := activationMetadata(*current)
	if err != nil {
		return err
	}
	if s.exchange == nil {
		return port.NewActivationProcessError("activation_exchange_unavailable", true, errAssistantExchangeUnavailable)
	}
	fallback := fmt.Sprintf(
		"External-agent job `%s` finished, but the integrated root response is unavailable (error code `%s`). The job result remains available through normal job status and result reads.",
		current.JobID,
		current.LastErrorCode,
	)
	message := domain.Message{Role: domain.RoleAssistant, Source: domain.MessageSourceAssistant, Content: fallback, CreatedAt: s.clock.Now().UTC()}
	intent, err := fallbackStore.PrepareActivationFallbackExchange(ctx, current, metadata, message, s.cfg.RetainMessages, s.clock.Now().UTC())
	if err != nil {
		return port.NewActivationProcessError("activation_fallback_prepare_retryable", true, err)
	}
	finderIntent := port.AssistantExchangeIntent{
		ID: intent.ID, ChannelID: metadata.ChannelID, ChannelKind: channelKind,
		RootTS: metadata.RootTS, Content: fallback, CorrelationID: intent.CorrelationID,
	}
	assistantTS, found, err := s.findPublishedAssistantExchange(ctx, finderIntent)
	if err != nil {
		return port.NewActivationProcessError("activation_fallback_reconcile_retryable", true, err)
	}
	if !found {
		published, publishErr := s.publisher.Publish(ctx, domain.ReplyTarget{
			ChannelID: metadata.ChannelID, ThreadTS: metadata.RootTS, CorrelationID: intent.CorrelationID,
		}, fallback)
		if publishErr != nil {
			return port.NewActivationProcessError("activation_fallback_publish_retryable", true, publishErr)
		}
		assistantTS = published.LastMessageTS
	}
	if strings.TrimSpace(assistantTS) == "" {
		return port.NewActivationProcessError("activation_fallback_publish_retryable", true, errors.New("assistant publisher returned no timestamp"))
	}
	if err := s.exchange.MarkAssistantExchangePublished(ctx, intent.ID, assistantTS); err != nil {
		return port.NewActivationProcessError("activation_exchange_retryable", true, err)
	}
	// Message persistence, intent consumption, and the fallback_slack_ts CAS
	// are one transaction. A crash at any earlier step reconciles through the
	// deterministic exchange identity without duplicating the message row.
	if err := fallbackStore.CompleteActivationFallback(ctx, current, intent.ID, assistantTS, s.clock.Now().UTC()); err != nil {
		return activationStateConflict(err)
	}
	return nil
}

// ReconcileJobCompletion never invokes the model. It only materializes a
// response already proven durable, or closes an ambiguous model boundary as
// completion_unknown.
func (s *Service) ReconcileJobCompletion(ctx context.Context, activation domain.ExternalAgentJobActivation) error {
	if s == nil || s.activationStore == nil {
		return port.NewActivationProcessError("activation_store_unavailable", true, errActivationStoreUnavailable)
	}
	current, err := s.authoritativeActivation(ctx, activation)
	if err != nil {
		return err
	}
	releaseConversation, acquired := s.limiter.TryAcquire(string(current.ConversationKey))
	if !acquired {
		return port.NewActivationProcessError("conversation_busy", true, errConversationBusy)
	}
	defer releaseConversation()
	// The activation may have changed while waiting for the conversation
	// coordinator. Re-read it before publishing or finalizing anything.
	current, err = s.authoritativeActivation(ctx, activation)
	if err != nil {
		return err
	}
	switch current.State {
	case domain.ActivationCompleted, domain.ActivationFailed, domain.ActivationCompletionUnknown:
		return nil
	case domain.ActivationModelStarted:
		return s.reconcileModelStarted(ctx, current)
	case domain.ActivationResponsePrepared:
		return s.publishPreparedActivation(ctx, current)
	default:
		return activationStateConflict(fmt.Errorf("cannot reconcile activation in state %q", current.State))
	}
}

func (s *Service) runJobCompletion(ctx context.Context, activation *domain.ExternalAgentJobActivation) error {
	metadata, channelKind, err := s.activationIdentity(ctx, activation)
	if err != nil {
		return err
	}
	authorization := s.cfg.AccessPolicy.Authorize(domain.Invocation{
		TeamID: activation.TeamID, ChannelID: metadata.ChannelID, ChannelKind: channelKind, UserID: activation.Actor,
	})
	if !authorization.Allowed {
		code := "activation_access_denied"
		if authorization.Reason == "user_not_allowed" {
			code = "actor_revoked"
		}
		return s.failActivation(ctx, activation, code, false, errors.New(authorization.Reason))
	}

	// The conversation coordinator is acquired before the frame snapshot so a
	// human workstream command cannot mutate the workstream while the frame is
	// read, the model runs, or the response is published.
	releaseConversation, acquired := s.limiter.TryAcquire(string(activation.ConversationKey))
	if !acquired {
		return port.NewActivationProcessError("conversation_busy", true, errConversationBusy)
	}
	defer releaseConversation()

	frame, frameErr := s.activationFrame(ctx, *activation)
	if frameErr != nil {
		code := "activation_result_unavailable"
		if errors.Is(frameErr, errActivationJobIdentityInvalid) {
			code = "activation_identity_invalid"
		}
		return s.failActivation(ctx, activation, code, false, frameErr)
	}
	if frame.Representation == domain.ActivationResultUnavailable {
		return s.failActivation(ctx, activation, "activation_result_unavailable", false, errors.New("activation result representation is unavailable"))
	}
	frameText, frameErr := frame.Render()
	if frameErr != nil {
		return s.failActivation(ctx, activation, "activation_frame_invalid", false, frameErr)
	}
	delegation, delegationErr := s.activationDelegationMessage(ctx, *activation)
	if delegationErr != nil {
		return s.failActivation(ctx, activation, "activation_frame_invalid", false, delegationErr)
	}

	// The activation session receives only the root-created task for this job.
	// The worker frame uses separate ephemeral model-role evidence and never
	// enters either durable conversation session.
	modelContext := []domain.Message{delegation}

	s.logger.Info("external-agent activation model request",
		"job_id", activation.JobID,
		"activation_id", activation.ActivationID,
		"activation_scope", activation.ActivationScope,
		"delegation_bytes", len([]byte(delegation.Content)),
		"internal_event_bytes", len([]byte(frameText)),
		"result_representation", frame.Representation,
		"result_bytes", frame.ResultBytes,
	)

	modelRelease, modelAcquired := s.modelCalls.TryAcquire()
	if !modelAcquired {
		return port.NewActivationProcessError("model_busy", true, errors.New("shared model call limit is exhausted"))
	}
	modelCtx := ctx
	cancel := func() {}
	if s.cfg.ModelTimeout > 0 {
		modelCtx, cancel = context.WithTimeout(ctx, s.cfg.ModelTimeout)
	}
	modelStarted := false
	var modelStartErr error
	turn, runErr := func() (port.AgentTurn, error) {
		defer modelRelease()
		return s.runtime.Run(modelCtx, port.AgentRequest{
			ConversationKey: activation.ConversationKey,
			Origin: port.AgentTurnOrigin{
				Kind:            port.AgentTurnOriginJobCompletion,
				Actor:           activation.Actor,
				ActivationID:    activation.ActivationID,
				ActivationScope: activation.ActivationScope,
			},
			Messages:      modelContext,
			InternalEvent: frameText,
			Activation:    activation,
			BeforeModel: func(markCtx context.Context) error {
				if err := s.activationStore.MarkActivationModelStarted(markCtx, activation, s.clock.Now().UTC()); err != nil {
					modelStartErr = err
					return err
				}
				modelStarted = true
				activation.State = domain.ActivationModelStarted
				return nil
			},
		})
	}()
	cancel()
	if runErr != nil {
		stage, code, errorTypes := activationModelDiagnostics(runErr)
		s.logger.Error("external-agent activation model failed",
			"job_id", activation.JobID,
			"activation_id", activation.ActivationID,
			"activation_scope", activation.ActivationScope,
			"model_started", modelStarted,
			"model_error_stage", stage,
			"model_error_code", code,
			"error_types", errorTypes,
		)
		if modelStartErr != nil {
			return activationStateConflict(modelStartErr)
		}
		if !modelStarted {
			// An irreducible frame admission failure is terminal for this
			// activation: it can never fit the provider-shaped budget and must
			// not retry the producing external-agent job.
			if errors.Is(runErr, domain.ErrIrreducibleContext) {
				return s.failActivation(ctx, activation, "activation_frame_invalid", false, runErr)
			}
			return port.NewActivationProcessError("activation_frame_retryable", true, runErr)
		}
		return port.NewActivationProcessError("activation_model_started", false, runErr)
	}
	if !modelStarted {
		return port.NewActivationProcessError("activation_model_boundary_missing", false, errors.New("activation runtime returned without crossing the model boundary"))
	}
	if turn.PendingConfirmation != nil {
		return s.markUnknown(ctx, activation, "activation_confirmation_not_allowed")
	}
	response := turn.Text
	if strings.TrimSpace(response) == "" && turn.Presentation != nil {
		response = turn.Presentation.FallbackMarkdown
	}
	response = s.sanitize(response)
	if strings.TrimSpace(response) == "" {
		s.logger.Error("external-agent activation model returned empty response",
			"job_id", activation.JobID, "activation_id", activation.ActivationID)
		return s.markUnknown(ctx, activation, "activation_empty_response")
	}
	responseAllowed, responsePolicyCode := activationResponsePolicy(response, activation.ActivationScope)
	s.logger.Info("external-agent activation model response",
		"job_id", activation.JobID,
		"activation_id", activation.ActivationID,
		"activation_scope", activation.ActivationScope,
		"response_bytes", len([]byte(response)),
		"response_runes", utf8.RuneCountInString(response),
		"response_policy_allowed", responseAllowed,
		"response_policy_code", responsePolicyCode,
	)
	if !responseAllowed {
		return s.markUnknown(ctx, activation, "activation_response_policy_invalid")
	}
	message := domain.Message{Role: domain.RoleAssistant, Source: domain.MessageSourceAssistant, Content: response, CreatedAt: s.clock.Now().UTC()}
	prepared, prepareErr := s.prepareActivationResponse(ctx, activation, metadata, message)
	if prepareErr != nil {
		return port.NewActivationProcessError("activation_response_prepare_retryable", true, prepareErr)
	}
	activation.State = domain.ActivationResponsePrepared
	activation.ResponseBody = response
	activation.ExchangeIntentID = prepared.ID
	activation.CorrelationID = prepared.CorrelationID
	return s.publishPreparedActivation(ctx, activation)
}

func (s *Service) activationDelegationMessage(ctx context.Context, activation domain.ExternalAgentJobActivation) (domain.Message, error) {
	if s.completionReader == nil {
		return domain.Message{}, errors.New("activation delegation reader is unavailable")
	}
	job, err := s.completionReader.Status(ctx, activation.JobID, activation.Actor, activation.ConversationKey)
	if err != nil || job == nil || job.ID != activation.JobID || job.Actor != activation.Actor ||
		job.ConversationKey != activation.ConversationKey || strings.TrimSpace(job.Task) == "" {
		return domain.Message{}, errors.New("activation delegation is unavailable")
	}
	return domain.Message{
		Role: domain.RoleUser, Source: domain.MessageSourceJobCompletion,
		Content: job.Task, UserID: activation.Actor,
		ExternalTS: activation.ActivationID, CreatedAt: s.clock.Now().UTC(),
	}, nil
}

func (s *Service) activationFrame(ctx context.Context, activation domain.ExternalAgentJobActivation) (domain.ActivationFrame, error) {
	job, err := s.activationFrameJob(ctx, activation)
	if err != nil {
		return domain.ActivationFrame{}, err
	}
	executionTask, err := domain.ExternalAgentExecutionTask(job.Task)
	if err != nil {
		return domain.ActivationFrame{}, err
	}
	excerpt, taskDigest, truncated, err := domain.BuildDelegatedTaskExcerpt(executionTask)
	if err != nil {
		return domain.ActivationFrame{}, err
	}
	frame := domain.ActivationFrame{
		ActivationID: activation.ActivationID, JobID: activation.JobID, ActivationScope: activation.ActivationScope,
		Actor: activation.Actor, TeamID: activation.TeamID, ConversationKey: activation.ConversationKey,
		TerminalStatus: activation.TerminalStatus, PrimaryProject: job.PrimaryProject,
		DelegatedTaskExcerpt: excerpt, DelegatedTaskSHA256: taskDigest, DelegatedTaskTruncated: truncated,
		WorkstreamID: activation.WorkstreamID, TaskID: activation.TaskID, ExecutionIdentity: activation.ExecutionIdentity,
		AdmissionRevision: activation.AdmissionRevision, ResultSHA256: activation.ResultSHA256, ResultBytes: activation.ContentBytes,
		Representation: domain.ActivationResultUnavailable,
	}
	switch activation.ActivationScope {
	case domain.ExternalAgentActivationWorkstream:
		if err := s.populateActivationWorkstreamFrame(ctx, activation, job, &frame); err != nil {
			return domain.ActivationFrame{}, err
		}
	case domain.ExternalAgentActivationConversation:
	default:
		return domain.ActivationFrame{}, errors.New("activation scope is invalid")
	}
	if err := s.populateActivationResult(ctx, activation, &frame); err != nil {
		return domain.ActivationFrame{}, err
	}
	return frame, nil
}

func (s *Service) activationFrameJob(ctx context.Context, activation domain.ExternalAgentJobActivation) (*domain.ExternalAgentJob, error) {
	if s.completionReader == nil {
		return nil, errors.New("activation job reader is unavailable")
	}
	job, err := s.completionReader.Status(ctx, activation.JobID, activation.Actor, activation.ConversationKey)
	if err != nil {
		return nil, errActivationJobIdentityInvalid
	}
	if job == nil || job.ID != activation.JobID || job.Actor != activation.Actor || job.ConversationKey != activation.ConversationKey ||
		job.Status != domain.JobCompleted || activation.TerminalStatus != domain.JobCompleted ||
		job.Status != activation.TerminalStatus || job.StatusRevision != activation.StatusRevision || job.PrimaryProject == "" {
		return nil, errActivationJobIdentityInvalid
	}
	return job, nil
}

func (s *Service) populateActivationWorkstreamFrame(
	ctx context.Context,
	activation domain.ExternalAgentJobActivation,
	job *domain.ExternalAgentJob,
	frame *domain.ActivationFrame,
) error {
	if s.workstreams == nil || !domain.CompletionBindingPresent(activation.WorkstreamID, activation.TaskID, activation.ExecutionIdentity, activation.AdmissionRevision) {
		return errors.New("activation workstream binding is unavailable")
	}
	snapshot, snapshotErr := s.workstreams.SnapshotForActivation(ctx, activation.WorkstreamID, activation.Actor, activation.ConversationKey)
	if snapshotErr != nil {
		return fmt.Errorf("read activation workstream snapshot: %w", snapshotErr)
	}
	if snapshot.ID != activation.WorkstreamID || snapshot.OwnerActor != activation.Actor || snapshot.ConversationKey != activation.ConversationKey ||
		snapshot.Project != job.PrimaryProject || snapshot.Status != domain.WorkstreamActive || snapshot.Revision < activation.AdmissionRevision {
		return errors.New("activation workstream snapshot identity is invalid")
	}
	frame.Workstream = snapshot
	for _, task := range snapshot.Tasks {
		if task.ID == activation.TaskID {
			frame.Task = task
			break
		}
	}
	if frame.Task.ID == "" || frame.Task.JobID != job.ID || frame.Task.Project != job.PrimaryProject ||
		!activationTaskStatusMatches(frame.Task.Status, activation.TerminalStatus) || frame.Task.ExecutionIdentity != activation.ExecutionIdentity {
		return errors.New("activation task binding is invalid")
	}
	return nil
}

func activationTaskStatusMatches(status domain.TaskStatus, terminal domain.ExternalAgentJobStatus) bool {
	if status == domain.TaskRunning {
		return true
	}
	switch terminal {
	case domain.JobCompleted:
		return status == domain.TaskCompleted
	case domain.JobFailed:
		return status == domain.TaskFailed
	case domain.JobCancelled:
		return status == domain.TaskCancelled
	case domain.JobCompletionUnknown, domain.JobAbandoned:
		return status == domain.TaskCompletionUnknown
	default:
		return false
	}
}

func (s *Service) populateActivationResult(ctx context.Context, activation domain.ExternalAgentJobActivation, frame *domain.ActivationFrame) error {
	result, err := s.completionReader.ReadResult(ctx, activation.JobID, activation.Actor, activation.ConversationKey)
	if err != nil {
		return err
	}
	if result.JobID != activation.JobID || result.StatusRevision != activation.StatusRevision || result.ContentBytes <= 0 || result.ContentBytes != int64(len([]byte(result.Text))) ||
		result.ContentSHA256 == "" {
		return errors.New("activation result identity does not match terminal snapshot")
	}
	if activation.ContentBytes != result.ContentBytes {
		return errors.New("activation result byte count does not match terminal snapshot")
	}
	computedSHA256 := sha256Hex(result.Text)
	if !strings.EqualFold(result.ContentSHA256, computedSHA256) {
		return errors.New("activation result digest does not match result bytes")
	}
	if activation.ResultSHA256 == "" || !strings.EqualFold(activation.ResultSHA256, result.ContentSHA256) {
		return errors.New("activation result digest does not match terminal snapshot")
	}
	frame.ResultSHA256 = computedSHA256
	frame.ResultBytes = result.ContentBytes
	if s.directInlineAdmitted(result.ContentBytes) && utf8.RuneCountInString(result.Text) <= domain.MaxActivationFrameRunes {
		frame.Representation = domain.ActivationResultDirectInline
		frame.ResultText = result.Text
		return nil
	}
	found, err := s.populateNativeActivationResult(ctx, activation, frame)
	if err != nil {
		return err
	}
	if found {
		return nil
	}
	if result.DeliveryMode == domain.JobResultDeliveryFile {
		frame.Representation = domain.ActivationResultArtifactOnly
		frame.ResultMediaType = "text/markdown"
		frame.ResultAvailability = []domain.ResultAvailability{domain.ResultAvailabilityPrivateArtifact}
		return nil
	}
	frame.Representation = domain.ActivationResultUnavailable
	return nil
}

func (s *Service) populateNativeActivationResult(ctx context.Context, activation domain.ExternalAgentJobActivation, frame *domain.ActivationFrame) (bool, error) {
	nativeReader, ok := s.completionReader.(port.ExternalAgentJobNativeResultReader)
	if !ok {
		return false, nil
	}
	handle, found, err := nativeReader.NativeResultHandleForJob(ctx, activation.JobID, activation.Actor, activation.ConversationKey)
	if err != nil {
		return false, fmt.Errorf("read activation native result handle: %w", err)
	}
	if !found {
		return false, nil
	}
	if handle.SHA256 != frame.ResultSHA256 || handle.Bytes != frame.ResultBytes {
		return false, errors.New("activation native result handle identity does not match terminal result")
	}
	frame.Representation = domain.ActivationResultNativeHandle
	frame.ResultID = handle.ResultID
	frame.ResultMediaType = handle.MediaType
	frame.ResultAvailability = append([]domain.ResultAvailability(nil), handle.Availability...)
	frame.RepresentationIDs = append([]string(nil), handle.RepresentationIDs...)
	return true, nil
}

// directInlineAdmitted applies the TRD 02 per-profile inline admission. While
// the result-handles gate is enabled, a positive declared admission is
// required; no declaration means zero direct-inline bytes. While the gate is
// disabled, the legacy rune-cap-only selection remains for legacy external-agent
// delivery.
func (s *Service) directInlineAdmitted(bytes int64) bool {
	if !s.cfg.ResultHandlesEnabled {
		return true
	}
	return s.cfg.MaxDirectInlineBytes > 0 && bytes > 0 &&
		bytes <= s.cfg.MaxDirectInlineBytes && bytes <= domain.HardMaxDirectInlineResultBytes
}

func (s *Service) prepareActivationResponse(
	ctx context.Context,
	activation *domain.ExternalAgentJobActivation,
	metadata domain.ConversationMetadata,
	message domain.Message,
) (port.PreparedAssistantExchange, error) {
	if atomicStore, ok := s.activationStore.(port.ExternalAgentJobActivationExchangeStore); ok {
		return atomicStore.PrepareActivationResponseWithExchange(ctx, activation, metadata, message, s.cfg.RetainMessages, s.clock.Now().UTC())
	}
	if s.exchange == nil {
		return port.PreparedAssistantExchange{}, errAssistantExchangeUnavailable
	}
	prepared, err := s.exchange.PrepareAssistantExchange(ctx, metadata, message, s.cfg.RetainMessages)
	if err != nil {
		return port.PreparedAssistantExchange{}, fmt.Errorf("prepare assistant exchange: %w", err)
	}
	digest := sha256Hex(message.Content)
	if err := s.activationStore.PrepareActivationResponse(ctx, activation, message.Content, digest, prepared.ID, prepared.CorrelationID, s.clock.Now().UTC()); err != nil {
		return port.PreparedAssistantExchange{}, fmt.Errorf("prepare activation response: %w", err)
	}
	return prepared, nil
}

func (s *Service) publishPreparedActivation(ctx context.Context, activation *domain.ExternalAgentJobActivation) error {
	metadata, channelKind, err := s.activationIdentity(ctx, activation)
	if err != nil {
		return err
	}
	authorization := s.cfg.AccessPolicy.Authorize(domain.Invocation{
		TeamID: activation.TeamID, ChannelID: metadata.ChannelID, ChannelKind: channelKind, UserID: activation.Actor,
	})
	if !authorization.Allowed {
		return s.failActivation(ctx, activation, "actor_revoked", false, errors.New(authorization.Reason))
	}
	if strings.TrimSpace(activation.ResponseBody) == "" || strings.TrimSpace(activation.ExchangeIntentID) == "" || strings.TrimSpace(activation.CorrelationID) == "" {
		return port.NewActivationProcessError("activation_response_invalid", false, errors.New("prepared activation response is incomplete"))
	}
	if s.exchange == nil {
		return port.NewActivationProcessError("activation_exchange_unavailable", true, errAssistantExchangeUnavailable)
	}

	intent := port.AssistantExchangeIntent{
		ID: activation.ExchangeIntentID, ChannelID: metadata.ChannelID, ChannelKind: channelKind,
		RootTS: metadata.RootTS, Content: activation.ResponseBody, CorrelationID: activation.CorrelationID,
	}
	assistantTS, found, err := s.findPublishedAssistantExchange(ctx, intent)
	if err != nil {
		return port.NewActivationProcessError("activation_response_reconcile_retryable", true, err)
	}
	if !found {
		published, publishErr := s.publisher.Publish(ctx, domain.ReplyTarget{
			ChannelID: metadata.ChannelID, ThreadTS: metadata.RootTS, CorrelationID: activation.CorrelationID,
		}, activation.ResponseBody)
		if publishErr != nil {
			return port.NewActivationProcessError("activation_response_publish_retryable", true, publishErr)
		}
		assistantTS = published.LastMessageTS
	}
	if strings.TrimSpace(assistantTS) == "" {
		return port.NewActivationProcessError("activation_response_publish_retryable", true, errors.New("assistant publisher returned no timestamp"))
	}
	if err := s.exchange.MarkAssistantExchangePublished(ctx, activation.ExchangeIntentID, assistantTS); err != nil {
		return port.NewActivationProcessError("activation_exchange_retryable", true, err)
	}
	if err := s.exchange.FinalizeAssistantExchange(ctx, activation.ExchangeIntentID); err != nil {
		return port.NewActivationProcessError("activation_exchange_retryable", true, err)
	}
	if err := s.activationStore.CompleteActivation(ctx, activation, assistantTS, s.clock.Now().UTC()); err != nil {
		return activationStateConflict(err)
	}
	return nil
}

func (s *Service) findPublishedAssistantExchange(ctx context.Context, intent port.AssistantExchangeIntent) (string, bool, error) {
	if s.exchangeFinder == nil {
		return "", false, nil
	}
	return s.exchangeFinder.FindPublishedAssistantExchange(ctx, intent)
}

func (s *Service) reconcileModelStarted(ctx context.Context, activation *domain.ExternalAgentJobActivation) error {
	recovery, ok := s.runtime.(port.AgentActivationRecovery)
	if !ok {
		return s.markUnknown(ctx, activation, "completion_unknown")
	}
	turn, found, err := recovery.RecoverActivation(ctx, activation.ConversationKey, activation.ActivationID)
	if err != nil {
		return port.NewActivationProcessError("activation_recovery_retryable", true, err)
	}
	if !found {
		return s.markUnknown(ctx, activation, "completion_unknown")
	}
	if turn.PendingConfirmation != nil {
		return s.markUnknown(ctx, activation, "activation_confirmation_not_allowed")
	}
	response := s.sanitize(turn.Text)
	if strings.TrimSpace(response) == "" {
		return s.markUnknown(ctx, activation, "activation_empty_response")
	}
	if !activationResponseAllowed(response, activation.ActivationScope) {
		return s.markUnknown(ctx, activation, "activation_response_policy_invalid")
	}
	metadata, _, err := activationMetadata(*activation)
	if err != nil {
		return s.markUnknown(ctx, activation, "completion_unknown")
	}
	message := domain.Message{Role: domain.RoleAssistant, Source: domain.MessageSourceAssistant, Content: response, CreatedAt: s.clock.Now().UTC()}
	prepared, prepareErr := s.prepareActivationResponse(ctx, activation, metadata, message)
	if prepareErr != nil {
		return port.NewActivationProcessError("activation_response_prepare_retryable", true, prepareErr)
	}
	activation.State = domain.ActivationResponsePrepared
	activation.ResponseBody = response
	activation.ExchangeIntentID = prepared.ID
	activation.CorrelationID = prepared.CorrelationID
	return s.publishPreparedActivation(ctx, activation)
}

func (s *Service) markUnknown(ctx context.Context, activation *domain.ExternalAgentJobActivation, code string) error {
	if err := s.activationStore.MarkActivationCompletionUnknown(ctx, activation, code, s.clock.Now().UTC()); err != nil {
		return activationStateConflict(err)
	}
	return nil
}

func (s *Service) failActivation(ctx context.Context, activation *domain.ExternalAgentJobActivation, code string, retryable bool, cause error) error {
	if err := s.activationStore.FailActivation(ctx, activation, code, s.clock.Now().UTC()); err != nil {
		return activationStateConflict(err)
	}
	return port.NewActivationProcessError(code, retryable, cause)
}

func (s *Service) authoritativeActivation(ctx context.Context, supplied domain.ExternalAgentJobActivation) (*domain.ExternalAgentJobActivation, error) {
	if strings.TrimSpace(supplied.ActivationID) == "" {
		return nil, port.NewActivationProcessError("activation_identity_invalid", false, errors.New("activation ID is required"))
	}
	current, err := s.activationStore.GetActivation(ctx, supplied.ActivationID)
	if err != nil {
		return nil, port.NewActivationProcessError("activation_lookup_retryable", true, err)
	}
	if current == nil {
		return nil, port.NewActivationProcessError("activation_not_found", false, errors.New("external-agent activation was not found"))
	}
	if !activationIdentityMatches(*current, supplied) || current.ActivationID != domain.ExternalAgentJobActivationID(current.JobID, current.StatusRevision, current.Kind) {
		return nil, port.NewActivationProcessError("activation_identity_invalid", false, errors.New("external-agent activation binding does not match durable identity"))
	}
	if err := current.Validate(); err != nil {
		return nil, port.NewActivationProcessError("activation_identity_invalid", false, err)
	}
	return current, nil
}

func activationIdentityMatches(left, right domain.ExternalAgentJobActivation) bool {
	return left.ActivationID == right.ActivationID && left.JobID == right.JobID &&
		left.ActivationScope == right.ActivationScope && left.StatusRevision == right.StatusRevision && left.Kind == right.Kind &&
		left.TerminalStatus == right.TerminalStatus && left.NotificationSHA256 == right.NotificationSHA256 && left.ResultSHA256 == right.ResultSHA256 &&
		left.Actor == right.Actor && left.TeamID == right.TeamID && left.ConversationKey == right.ConversationKey &&
		left.WorkstreamID == right.WorkstreamID && left.TaskID == right.TaskID && left.ExecutionIdentity == right.ExecutionIdentity && left.AdmissionRevision == right.AdmissionRevision &&
		left.OriginalCallID == right.OriginalCallID && left.DeliveryMode == right.DeliveryMode &&
		left.ContentBytes == right.ContentBytes && left.SlackMessageTS == right.SlackMessageTS && left.PublishedAt.Equal(right.PublishedAt)
}

func (s *Service) activationIdentity(ctx context.Context, activation *domain.ExternalAgentJobActivation) (domain.ConversationMetadata, domain.ChannelKind, error) {
	metadata, channelKind, err := activationMetadata(*activation)
	if err != nil {
		return domain.ConversationMetadata{}, "", s.failActivation(ctx, activation, "activation_identity_invalid", false, err)
	}
	if !domain.PlausibleUserID(activation.Actor) || !domain.PlausibleTeamID(activation.TeamID) || !domain.PlausibleChannelID(metadata.ChannelID) {
		return domain.ConversationMetadata{}, "", s.failActivation(ctx, activation, "activation_identity_invalid", false, errors.New("external-agent activation binding is invalid"))
	}
	return metadata, channelKind, nil
}

func activationMetadata(activation domain.ExternalAgentJobActivation) (domain.ConversationMetadata, domain.ChannelKind, error) {
	parts := strings.Split(string(activation.ConversationKey), ":")
	if len(parts) < 4 || parts[0] != "slack" || parts[1] == "" || parts[3] == "" {
		return domain.ConversationMetadata{}, "", errors.New("external-agent activation conversation key is malformed")
	}
	if activation.TeamID != parts[1] {
		return domain.ConversationMetadata{}, "", errors.New("external-agent activation team does not match conversation")
	}
	var channelKind domain.ChannelKind
	switch parts[2] {
	case "dm":
		channelKind = domain.ChannelDM
	case "channel":
		channelKind = domain.ChannelPublic
	case "group":
		channelKind = domain.ChannelPrivate
	default:
		return domain.ConversationMetadata{}, "", errors.New("external-agent activation channel kind is invalid")
	}
	target, err := domain.ConversationReplyTarget(activation.ConversationKey)
	if err != nil {
		return domain.ConversationMetadata{}, "", err
	}
	return domain.ConversationMetadata{
		Key: activation.ConversationKey, TeamID: activation.TeamID, ChannelID: target.ChannelID,
		ChannelKind: channelKind, RootTS: target.ThreadTS, LastTS: activation.SlackMessageTS,
	}, channelKind, nil
}

// activationResponseAllowed enforces the host-owned completion policy at the
// publication boundary. A conversation activation never permits a proposal.
func activationResponseAllowed(response string, scopes ...domain.ExternalAgentActivationScope) bool {
	allowed, _ := activationResponsePolicy(response, scopes...)
	return allowed
}

func activationResponsePolicy(response string, scopes ...domain.ExternalAgentActivationScope) (bool, string) {
	if strings.TrimSpace(response) == "" {
		return false, "empty_response"
	}
	scope := domain.ExternalAgentActivationWorkstream
	if len(scopes) > 0 && scopes[0] != "" {
		scope = scopes[0]
	}
	if scope != domain.ExternalAgentActivationWorkstream && scope != domain.ExternalAgentActivationConversation {
		return false, "invalid_scope"
	}
	lower := strings.ToLower(response)
	markers := []struct {
		text string
		code string
	}{
		{text: "workstream-human ", code: "workstream_command"},
		{text: "adk_request_confirmation", code: "confirmation_protocol"},
		{text: "toolconfirmation", code: "confirmation_protocol"},
		{text: "<function_call", code: "function_protocol"},
		{text: "function_call", code: "function_protocol"},
	}
	for _, marker := range markers {
		if strings.Contains(lower, marker.text) {
			return false, marker.code
		}
	}
	proposals := countProposalLabels(response)
	if scope == domain.ExternalAgentActivationConversation && proposals > 0 {
		return false, "proposal_not_allowed"
	}
	if proposals > 1 {
		return false, "multiple_proposals"
	}
	return true, "allowed"
}

// countProposalLabels counts the machine-recognizable proposal labels: lines
// whose trimmed lowercase prefix is exactly "proposal:". Any other phrasing
// is informational prose and is not counted.
func countProposalLabels(response string) int {
	count := 0
	for line := range strings.SplitSeq(response, "\n") {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(line)), "proposal:") {
			count++
		}
	}
	return count
}

func sha256Hex(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}
