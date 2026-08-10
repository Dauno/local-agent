package contextcompiler

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"strings"
	"unicode/utf8"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

const (
	projectionReferenceShape = 64
	projectionMarkerKey      = "_local_agent_context_projection"
	toolProjectionMarkerKey  = "_tool_local_agent_context_projection"
)

// projectionMutation contains deterministic work prepared before storage.
// It is private so untrusted input cannot provide a ready-to-materialize marker.
type projectionMutation struct {
	contentIndex int
	partIndex    int
	fullJSON     []byte
	digest       string
	reason       string
	excerpt      string
	keepExcerpt  bool
	response     *domain.FunctionResponse
	sourceCost   int
}

func dryRunProjectionMarker(reason string, originalBytes int) domain.ContextProjectionMarker {
	return domain.ContextProjectionMarker{
		Reason:        reason,
		ResultRef:     strings.Repeat("r", projectionReferenceShape),
		SHA256:        strings.Repeat("a", projectionReferenceShape),
		OriginalBytes: originalBytes,
		InlineBytes:   0,
		Complete:      false,
	}
}

func projectionResponse(response *domain.FunctionResponse, marker domain.ContextProjectionMarker, excerpt string, keepExcerpt bool) *domain.FunctionResponse {
	result := &domain.FunctionResponse{ID: response.ID, Name: response.Name, WillContinue: response.WillContinue}
	if keepExcerpt {
		result.Response = maps.Clone(response.Response)
		if result.Response == nil {
			result.Response = make(map[string]any, 1)
		}
		reduceResponseFields(result.Response, excerpt)
	} else {
		result.Response = make(map[string]any, 1)
	}
	result.Response[projectionMarkerKey] = marker
	return result
}

func dryRunActiveContents(active []domain.Content, parts []reduciblePart) []domain.Content {
	result := domain.CloneContents(active)
	for _, part := range parts {
		marker := dryRunProjectionMarker("late_externalization", len(part.canonicalJSON))
		response := part.part.FunctionResponse
		result[part.contentIndex].Parts[part.partIndex] = domain.ContentPart{FunctionResponse: projectionResponse(response, marker, "", false)}
	}
	return result
}

// lateExternalize is the single coarse fallback after optional context and
// existing excerpts are removed. It reuses the initial response analysis.
func (c *Compiler) lateExternalize(ctx context.Context, req domain.CompileRequest, active []domain.Content, analyzed []reduciblePart, planned []projectionMutation, hard int) ([]domain.Content, int, int, error) {
	plannedIndexes := make(map[projectionIndex]struct{}, len(planned))
	for _, projection := range planned {
		plannedIndexes[projectionIndex{contentIndex: projection.contentIndex, partIndex: projection.partIndex}] = struct{}{}
	}
	parts := make([]reduciblePart, 0, len(analyzed))
	for _, part := range analyzed {
		if _, alreadyProjected := plannedIndexes[projectionIndex{contentIndex: part.contentIndex, partIndex: part.partIndex}]; alreadyProjected {
			continue
		}
		parts = append(parts, part)
	}
	if len(parts) == 0 {
		return nil, 0, 0, nil
	}

	minimum := domain.CloneContents(active)
	for _, part := range parts {
		marker := dryRunProjectionMarker("late_externalization", len(part.canonicalJSON))
		response := part.part.FunctionResponse
		minimum[part.contentIndex].Parts[part.partIndex] = domain.ContentPart{FunctionResponse: projectionResponse(response, marker, "", false)}
	}
	if err := validateProjectedContents(assembleContents(nil, nil, nil, minimum), req.OpenInvocationIDs); err != nil {
		return nil, 0, 0, fmt.Errorf("context compiler: validate late projection: %w", err)
	}
	minimumCount, err := c.countProjection(ctx, minimum, req.FixedRequestTokens)
	if err != nil {
		return nil, 0, 0, err
	}
	if minimumCount.Tokens > hard {
		return nil, minimumCount.Tokens, 0, &domain.IrreducibleContextError{MinimumTokens: minimumCount.Tokens, HardTokens: hard}
	}

	projections := make([]projectionMutation, 0, len(parts))
	for _, part := range parts {
		projection, planErr := newProjectionMutation(part, 0, "late_externalization")
		if planErr != nil {
			return nil, 0, 0, planErr
		}
		projections = append(projections, projection)
	}
	return c.materializeProjections(ctx, req, active, projections)
}

func newProjectionMutation(part reduciblePart, budget int, reason string) (projectionMutation, error) {
	response := part.part.FunctionResponse
	if response == nil {
		return projectionMutation{}, errors.New("context compiler: projection response is required")
	}
	fullJSON := append([]byte(nil), part.canonicalJSON...)
	projection := projectionMutation{
		contentIndex: part.contentIndex,
		partIndex:    part.partIndex,
		fullJSON:     fullJSON,
		digest:       part.digest,
		reason:       reason,
		keepExcerpt:  budget > 0,
		response:     response,
		sourceCost:   part.cost,
	}

	inlineBudget := budget - part.minimumCost
	if inlineBudget < 0 {
		inlineBudget = 0
	}
	projection.excerpt = truncateToCodePoints(extractResponseText(response.Response), inlineBudget)
	return projection, nil
}

// materializeProjections is the only phase that writes recoverable results.
func (c *Compiler) materializeProjections(ctx context.Context, req domain.CompileRequest, active []domain.Content, projections []projectionMutation) ([]domain.Content, int, int, error) {
	if len(projections) == 0 {
		return domain.CloneContents(active), 0, 0, nil
	}
	if c.resultStore == nil {
		return nil, 0, 0, errors.New("context compiler: recoverable result store is required for projection materialization")
	}
	if strings.TrimSpace(req.Actor) == "" {
		return nil, 0, 0, errors.New("context compiler: actor is required before projection storage")
	}
	if strings.TrimSpace(req.ConversationKey) == "" {
		return nil, 0, 0, errors.New("context compiler: conversation key is required before projection storage")
	}
	for _, projection := range projections {
		if projection.response == nil || projection.reason == "" || projection.digest == "" || len(projection.fullJSON) == 0 {
			return nil, 0, 0, errors.New("context compiler: invalid projection mutation")
		}
		if projection.contentIndex < 0 || projection.contentIndex >= len(active) || projection.partIndex < 0 || projection.partIndex >= len(active[projection.contentIndex].Parts) {
			return nil, 0, 0, errors.New("context compiler: projection mutation index is out of range")
		}
	}

	refs := make([]string, len(projections))
	for i, projection := range projections {
		stored, err := c.resultStore.Put(ctx, port.PutResultRequest{
			Actor: req.Actor, ConversationKey: req.ConversationKey,
			Kind: "context_projection", Content: string(projection.fullJSON),
		})
		if err != nil {
			return nil, 0, 0, fmt.Errorf("context compiler: store result for %s: %w", projection.response.ID, err)
		}
		if stored.Ref == "" {
			return nil, 0, 0, fmt.Errorf("context compiler: store result for %s returned an empty reference", projection.response.ID)
		}
		refs[i] = stored.Ref
	}

	result := domain.CloneContents(active)
	removed := 0
	for i, projection := range projections {
		marker := domain.ContextProjectionMarker{
			Reason:        projection.reason,
			ResultRef:     refs[i],
			SHA256:        projection.digest,
			OriginalBytes: len(projection.fullJSON),
			InlineBytes:   utf8.RuneCountInString(projection.excerpt),
			Complete:      false,
		}
		projected := projectionResponse(projection.response, marker, projection.excerpt, projection.keepExcerpt)
		result[projection.contentIndex].Parts[projection.partIndex] = domain.ContentPart{FunctionResponse: projected}
		projectedCost, err := result[projection.contentIndex].Parts[projection.partIndex].Cost()
		if err != nil {
			return nil, 0, 0, fmt.Errorf("context compiler: measure projected response %s: %w", projection.response.ID, err)
		}
		if projection.sourceCost > projectedCost {
			removed += projection.sourceCost - projectedCost
		}
	}
	if err := validateProjectedContents(result, req.OpenInvocationIDs); err != nil {
		return nil, 0, 0, fmt.Errorf("context compiler: validate projection materialization: %w", err)
	}
	return result, removed, len(projections), nil
}

func stripProjectedExcerpts(contents []domain.Content) {
	for contentIndex := range contents {
		for partIndex := range contents[contentIndex].Parts {
			response := contents[contentIndex].Parts[partIndex].FunctionResponse
			if response == nil {
				continue
			}
			if _, projected := response.Response[projectionMarkerKey]; !projected {
				continue
			}
			for key, value := range response.Response {
				if key == projectionMarkerKey {
					continue
				}
				if text, ok := value.(string); ok && len(text) > 256 {
					response.Response[key] = ""
					continue
				}
				reflected := reflect.ValueOf(value)
				if reflected.IsValid() && (reflected.Kind() == reflect.Map || reflected.Kind() == reflect.Slice || reflected.Kind() == reflect.Array) {
					response.Response[key] = nil
				}
			}
		}
	}
}

// extractResponseText returns the primary text payload from a response map.
func extractResponseText(response map[string]any) string {
	for _, key := range []string{"text", "content", "response"} {
		if value, ok := response[key]; ok {
			if text, ok := value.(string); ok && text != "" {
				return text
			}
		}
	}
	encoded, err := domain.CanonicalJSON(response)
	if err != nil {
		return ""
	}
	return string(encoded)
}

// reduceResponseFields preserves scalar metadata while removing bulk values
// that have already been persisted behind the projection marker.
func reduceResponseFields(values map[string]any, excerpt string) {
	const maxMetaLen = 256
	metaKeys := map[string]bool{
		"path": true, "sha256": true, "sha": true, "digest": true,
		"status": true, "kind": true, "type": true,
		"id": true, "name": true,
		projectionMarkerKey: true,
	}
	for key, value := range values {
		if metaKeys[strings.ToLower(key)] {
			continue
		}
		if text, ok := value.(string); ok {
			if len(text) > maxMetaLen {
				values[key] = excerpt
			}
			continue
		}
		reflected := reflect.ValueOf(value)
		if reflected.IsValid() && (reflected.Kind() == reflect.Map || reflected.Kind() == reflect.Slice || reflected.Kind() == reflect.Array) {
			values[key] = nil
		}
	}
}

// truncateToCodePoints returns the first maxCodePoints Unicode code points.
func truncateToCodePoints(text string, maxCodePoints int) string {
	if maxCodePoints <= 0 || text == "" {
		return ""
	}
	runes := []rune(text)
	if len(runes) <= maxCodePoints {
		return text
	}
	return string(runes[:maxCodePoints])
}
