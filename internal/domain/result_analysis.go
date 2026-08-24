package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Hard maxima for TRD 07 objective-bound result analysis. Every configurable
// bound in orchestration.result_analysis is validated against one of these
// ceilings; a configuration value above its ceiling is rejected at startup,
// never silently clamped.
const (
	// HardMaxAnalysisSegmentBytes bounds the nominal byte length of one
	// segment produced by a segmenter.
	HardMaxAnalysisSegmentBytes = 65536
	// HardMaxAnalysisOverlapBasisPoints bounds the overlap fraction of the
	// nominal segment size, in basis points (10000 = 100%).
	HardMaxAnalysisOverlapBasisPoints = 2000
	// HardMaxAnalysisOverlapBytes bounds the absolute overlap size in bytes,
	// regardless of the configured basis-point fraction.
	HardMaxAnalysisOverlapBytes = 4096
	// HardMaxAnalysisLeaves bounds the number of leaf segments in one
	// analysis manifest.
	HardMaxAnalysisLeaves = 512
	// HardMaxAnalysisReductionFanIn bounds the number of children one
	// reduction step may combine.
	HardMaxAnalysisReductionFanIn = 16
	// HardMaxAnalysisReductionDepth bounds the height of the reduction tree
	// above the leaf level.
	HardMaxAnalysisReductionDepth = 4
	// HardMaxAnalysisConcurrentLeaves bounds the per-analysis concurrent leaf
	// call limit, independent of the process-wide model-call limiter.
	HardMaxAnalysisConcurrentLeaves = 8
	// HardMaxAnalysisAttemptsPerStep bounds retry attempts for one step.
	HardMaxAnalysisAttemptsPerStep = 5
	// HardMaxAnalysisCallTimeoutSeconds bounds one model call's timeout.
	HardMaxAnalysisCallTimeoutSeconds = 600
	// HardMaxAnalysisWallTimeSeconds bounds the total wall-clock budget for
	// one analysis.
	HardMaxAnalysisWallTimeSeconds = 3600
	// HardMaxAnalysisObjectiveRunes bounds the normalized objective text.
	HardMaxAnalysisObjectiveRunes = 2000
	// HardMaxAnalysisEvidenceExcerptBytes bounds one host-resolved evidence
	// excerpt.
	HardMaxAnalysisEvidenceExcerptBytes = 2048
	// HardMaxAnalysisEvidencePerLeaf bounds the evidence selectors one leaf
	// may declare.
	HardMaxAnalysisEvidencePerLeaf = 16
	// HardMaxAnalysisEvidencePerPacket bounds the evidence references one
	// terminal packet may cite.
	HardMaxAnalysisEvidencePerPacket = 64
	// HardMaxAnalysisBundleBytes bounds one downstream bundle.
	HardMaxAnalysisBundleBytes = 65536
	// HardMaxAnalysisStatementRunes bounds one typed statement (finding,
	// constraint, contradiction, or unresolved question).
	HardMaxAnalysisStatementRunes = 512
	// HardMaxAnalysisFindings bounds the findings list of one terminal
	// packet.
	HardMaxAnalysisFindings = 64
	// HardMaxAnalysisConstraints bounds the constraints list of one terminal
	// packet.
	HardMaxAnalysisConstraints = 32
	// HardMaxAnalysisContradictions bounds the contradictions list of one
	// terminal packet.
	HardMaxAnalysisContradictions = 16
	// HardMaxAnalysisUnresolvedQuestions bounds the unresolved-questions list
	// of one terminal packet.
	HardMaxAnalysisUnresolvedQuestions = 16
	// hardMaxAnalysisBoundedIDRunes bounds opaque step, evidence, and bundle
	// identifiers that are not themselves 64-hex opaque analysis IDs.
	hardMaxAnalysisBoundedIDRunes = 128
)

// ErrAnalysisValidation classifies input or structured-output rejection: the
// entry or the model's structured output does not satisfy the analysis
// contract.
var ErrAnalysisValidation = errors.New("result analysis validation failed")

// ErrAnalysisUnavailable classifies a source or store that cannot be read.
var ErrAnalysisUnavailable = errors.New("result analysis source is unavailable")

// ErrAnalysisCASConflict classifies a lost step generation or lease: a
// caller presented a claim token that no longer matches the current owner.
var ErrAnalysisCASConflict = errors.New("result analysis step CAS conflict")

// AnalysisObjectiveClass is the closed set of supported objective classes.
// Version 1 supports exactly one class; a second class can be added later
// without changing the identity scheme, the queue, or the tables, because
// the class is persisted on every analysis row and every step payload.
type AnalysisObjectiveClass string

// AnalysisObjectiveBoundedQuestionV1 is the only objective class version 1
// supports: one bounded natural-language question over one verified source.
const AnalysisObjectiveBoundedQuestionV1 AnalysisObjectiveClass = "bounded_question_v1"

// ValidAnalysisObjectiveClass reports whether value is a supported objective
// class. Version 1 rejects any other value at the validation boundary.
func ValidAnalysisObjectiveClass(value string) bool {
	return AnalysisObjectiveClass(value) == AnalysisObjectiveBoundedQuestionV1
}

// AnalysisLimits carries the effective, immutable bounds for one analysis.
// The limits snapshot is part of the analysis identity: once an analysis row
// is created, its limits never change, so a later configuration change never
// mutates a running or completed analysis.
type AnalysisLimits struct {
	MaxSegmentBytes             int64
	OverlapBasisPoints          int
	OverlapMaxBytes             int64
	MaxLeaves                   int
	MaxReductionFanIn           int
	MaxReductionDepth           int
	MaxConcurrentLeaves         int
	MaxAttemptsPerStep          int
	CallTimeoutSeconds          int
	WallTimeSeconds             int
	EvidenceExcerptBytes        int
	EvidenceSelectorsPerLeaf    int
	EvidenceReferencesPerPacket int
	BundleBytes                 int
}

// Validate enforces that every field is positive and within its hard
// maximum, plus one satisfiability rule: MaxReductionFanIn raised to
// MaxReductionDepth must be at least MaxLeaves, so a reduction tree of the
// configured shape can always be built over the configured leaf count. A
// combination that fails this rule would strand a running analysis with
// leaves it can never fully reduce.
//
// The TRD also describes "MaxLeaves * MaxSegmentBytes debe poder cubrir el
// tamaño máximo de fuente declarado". Checkpoint 1 defines no separate
// declared-maximum-source-size field: MaxLeaves * MaxSegmentBytes IS the
// declared maximum source size (see the Segmentation section: "A source
// larger than max_leaves * max_segment_bytes fails typed before any model
// contact"). That equation is therefore tautologically satisfied by
// construction and is not a second independent check here.
func (l AnalysisLimits) Validate() error {
	rangeChecks := []struct {
		name  string
		value int64
		max   int64
	}{
		{"max segment bytes", l.MaxSegmentBytes, HardMaxAnalysisSegmentBytes},
		{"overlap max bytes", l.OverlapMaxBytes, HardMaxAnalysisOverlapBytes},
	}
	for _, check := range rangeChecks {
		if check.value < 1 || check.value > check.max {
			return fmt.Errorf("%w: analysis limit %s must be between 1 and %d", ErrAnalysisValidation, check.name, check.max)
		}
	}
	intChecks := []struct {
		name  string
		value int
		max   int
	}{
		{"overlap basis points", l.OverlapBasisPoints, HardMaxAnalysisOverlapBasisPoints},
		{"max leaves", l.MaxLeaves, HardMaxAnalysisLeaves},
		{"max reduction fan-in", l.MaxReductionFanIn, HardMaxAnalysisReductionFanIn},
		{"max reduction depth", l.MaxReductionDepth, HardMaxAnalysisReductionDepth},
		{"max concurrent leaves", l.MaxConcurrentLeaves, HardMaxAnalysisConcurrentLeaves},
		{"max attempts per step", l.MaxAttemptsPerStep, HardMaxAnalysisAttemptsPerStep},
		{"call timeout seconds", l.CallTimeoutSeconds, HardMaxAnalysisCallTimeoutSeconds},
		{"wall time seconds", l.WallTimeSeconds, HardMaxAnalysisWallTimeSeconds},
		{"evidence excerpt bytes", l.EvidenceExcerptBytes, HardMaxAnalysisEvidenceExcerptBytes},
		{"evidence selectors per leaf", l.EvidenceSelectorsPerLeaf, HardMaxAnalysisEvidencePerLeaf},
		{"evidence references per packet", l.EvidenceReferencesPerPacket, HardMaxAnalysisEvidencePerPacket},
		{"bundle bytes", l.BundleBytes, HardMaxAnalysisBundleBytes},
	}
	for _, check := range intChecks {
		if check.value < 1 || check.value > check.max {
			return fmt.Errorf("%w: analysis limit %s must be between 1 and %d", ErrAnalysisValidation, check.name, check.max)
		}
	}
	if !AnalysisReductionTreeSatisfiable(l.MaxReductionFanIn, l.MaxReductionDepth, l.MaxLeaves) {
		return fmt.Errorf("%w: analysis reduction fan-in %d raised to depth %d cannot cover %d leaves", ErrAnalysisValidation, l.MaxReductionFanIn, l.MaxReductionDepth, l.MaxLeaves)
	}
	return nil
}

// AnalysisReductionTreeSatisfiable reports whether fanIn^depth >= leaves,
// computed without floating point so no configured hard-bounded combination
// can overflow int64. It is exported so a caller that must reject an
// unsatisfiable reduction tree before AnalysisLimits exists as a value (for
// example internal/config's static config validation, which must fail at
// startup rather than defer the failure to when a domain.AnalysisLimits is
// finally constructed at analysis-creation time) can call the exact same
// rule instead of reimplementing this arithmetic a second time.
// AnalysisLimits.Validate itself also calls this function, so the rule has
// exactly one definition.
func AnalysisReductionTreeSatisfiable(fanIn, depth, leaves int) bool {
	capacity := int64(1)
	for range depth {
		capacity *= int64(fanIn)
		if capacity >= int64(leaves) {
			return true
		}
	}
	return capacity >= int64(leaves)
}

// Digest returns the deterministic SHA-256 hex digest of the limits, stable
// across process runs and independent of construction order because every
// field is written in a fixed declared order.
func (l AnalysisLimits) Digest() string {
	h := sha256.New()
	writeAnalysisInt64(h, l.MaxSegmentBytes)
	writeAnalysisInt64(h, int64(l.OverlapBasisPoints))
	writeAnalysisInt64(h, l.OverlapMaxBytes)
	writeAnalysisInt64(h, int64(l.MaxLeaves))
	writeAnalysisInt64(h, int64(l.MaxReductionFanIn))
	writeAnalysisInt64(h, int64(l.MaxReductionDepth))
	writeAnalysisInt64(h, int64(l.MaxConcurrentLeaves))
	writeAnalysisInt64(h, int64(l.MaxAttemptsPerStep))
	writeAnalysisInt64(h, int64(l.CallTimeoutSeconds))
	writeAnalysisInt64(h, int64(l.WallTimeSeconds))
	writeAnalysisInt64(h, int64(l.EvidenceExcerptBytes))
	writeAnalysisInt64(h, int64(l.EvidenceSelectorsPerLeaf))
	writeAnalysisInt64(h, int64(l.EvidenceReferencesPerPacket))
	writeAnalysisInt64(h, int64(l.BundleBytes))
	return fmt.Sprintf("%x", h.Sum(nil))
}

func writeAnalysisInt64(h interface{ Write([]byte) (int, error) }, v int64) {
	_, _ = h.Write([]byte(strconv.FormatInt(v, 10)))
	_, _ = h.Write([]byte{0})
}

// AnalysisIdentity carries the semantic components that define one analysis.
// Changing any component creates a new analysis identity; a generic handoff
// may seed context but can never satisfy a required analysis. The analysis
// identifier itself is not derived from this identity: it is an opaque
// random value assigned at creation time (checkpoint 3), following the TRD
// 02 rule that a content digest is evidence of equality, never an
// authorization selector.
type AnalysisIdentity struct {
	SourceResultID      string
	SourceSHA256        string
	ObjectiveClass      AnalysisObjectiveClass
	ObjectiveDigest     string
	SegmentationVersion string
	PromptVersion       string
	ModelFingerprint    string
	LimitsDigest        string
}

// Validate enforces that every semantic component is present and well
// formed.
func (a AnalysisIdentity) Validate() error {
	if !validResultID(a.SourceResultID) {
		return fmt.Errorf("%w: analysis identity requires a valid source result id", ErrAnalysisValidation)
	}
	if !validSHA256Hex(a.SourceSHA256) {
		return fmt.Errorf("%w: analysis identity requires a valid source digest", ErrAnalysisValidation)
	}
	if !ValidAnalysisObjectiveClass(string(a.ObjectiveClass)) {
		return fmt.Errorf("%w: analysis identity has unknown objective class %q", ErrAnalysisValidation, a.ObjectiveClass)
	}
	if !validSHA256Hex(a.ObjectiveDigest) {
		return fmt.Errorf("%w: analysis identity requires a valid objective digest", ErrAnalysisValidation)
	}
	if !validAnalysisBoundedID(a.SegmentationVersion) {
		return fmt.Errorf("%w: analysis identity requires a bounded segmentation version", ErrAnalysisValidation)
	}
	if !validAnalysisBoundedID(a.PromptVersion) {
		return fmt.Errorf("%w: analysis identity requires a bounded prompt version", ErrAnalysisValidation)
	}
	if !validAnalysisBoundedID(a.ModelFingerprint) {
		return fmt.Errorf("%w: analysis identity requires a bounded model fingerprint", ErrAnalysisValidation)
	}
	if !validSHA256Hex(a.LimitsDigest) {
		return fmt.Errorf("%w: analysis identity requires a valid limits digest", ErrAnalysisValidation)
	}
	return nil
}

// SemanticDigest returns the deterministic SHA-256 hex digest over the eight
// semantic components, in fixed order, netstring-encoded (each component
// written as its decimal byte length followed by ':' and its raw bytes) so
// no in-band separator choice can ever create a collision between two
// different component sequences.
func (a AnalysisIdentity) SemanticDigest() string {
	h := sha256.New()
	writeAnalysisNetstring(h, a.SourceResultID)
	writeAnalysisNetstring(h, a.SourceSHA256)
	writeAnalysisNetstring(h, string(a.ObjectiveClass))
	writeAnalysisNetstring(h, a.ObjectiveDigest)
	writeAnalysisNetstring(h, a.SegmentationVersion)
	writeAnalysisNetstring(h, a.PromptVersion)
	writeAnalysisNetstring(h, a.ModelFingerprint)
	writeAnalysisNetstring(h, a.LimitsDigest)
	return fmt.Sprintf("%x", h.Sum(nil))
}

func writeAnalysisNetstring(h interface{ Write([]byte) (int, error) }, value string) {
	_, _ = h.Write([]byte(strconv.Itoa(len(value))))
	_, _ = h.Write([]byte{':'})
	_, _ = h.Write([]byte(value))
}

// ValidAnalysisID reports whether value is a well-formed opaque analysis
// identifier: 64 lowercase hex characters, the same shape as a TRD 02
// result_id. Generation is a checkpoint 3 concern; this is a form check
// only.
func ValidAnalysisID(value string) bool {
	return validResultID(value)
}

// validAnalysisBoundedID reports whether value is a non-empty, bounded,
// valid UTF-8 identifier with no control characters. It is used for opaque
// identifiers that are not the fixed 64-hex analysis ID shape: segmentation
// version, prompt version, model fingerprint, step IDs, evidence IDs, and
// bundle IDs.
func validAnalysisBoundedID(value string) bool {
	if strings.TrimSpace(value) == "" || !utf8.ValidString(value) {
		return false
	}
	if utf8.RuneCountInString(value) > hardMaxAnalysisBoundedIDRunes {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// NormalizeAnalysisObjective produces the canonical objective text used for
// digesting and for the model prompt. It rejects invalid UTF-8, discards NUL
// and C0 control characters other than '\n', '\r', and '\t', canonicalizes
// whitespace, and rejects empty or over-long text.
//
// This is a new normalizer, not a reuse of either existing sanitizer.
// domain.SanitizeResultText applies the same control-character rule this
// function needs, but it also escapes '<' to "&lt;" (external_agent_job.go),
// which is correct for result bytes that are later delivered as Markdown and
// wrong for an objective: a legitimate objective such as "which configs use
// value < 10?" would be altered, and its digest would stop matching the text
// the human actually wrote. domain.SanitizeConversationSummary
// (compaction.go) is far stricter than an objective warrants: it rejects any
// '<' or '>', rejects words such as "policy", "approved", or "secret", and
// requires attributed declarative sentences. Those are rules for a summary
// that returns to the model as reference prose, not for a bounded question.
// This function therefore copies only the control-character rule from
// SanitizeResultText, without the '<' escape.
//
// Credential redaction does not happen here: internal/domain is stdlib-only
// and cannot import internal/secure. The caller (a usecase or adapter
// boundary) must redact before calling this function.
func NormalizeAnalysisObjective(text string) (string, error) {
	if !utf8.ValidString(text) {
		return "", fmt.Errorf("%w: analysis objective is not valid UTF-8", ErrAnalysisValidation)
	}
	var builder strings.Builder
	for _, r := range text {
		if r == '\x00' || (r < ' ' && r != '\n' && r != '\r' && r != '\t') {
			continue
		}
		builder.WriteRune(r)
	}
	normalized := canonicalizeAnalysisWhitespace(builder.String())
	if normalized == "" {
		return "", fmt.Errorf("%w: analysis objective must not be empty", ErrAnalysisValidation)
	}
	if utf8.RuneCountInString(normalized) > HardMaxAnalysisObjectiveRunes {
		return "", fmt.Errorf("%w: analysis objective exceeds %d code points", ErrAnalysisValidation, HardMaxAnalysisObjectiveRunes)
	}
	return normalized, nil
}

// canonicalizeAnalysisWhitespace collapses every run of Unicode whitespace
// to one ASCII space and trims the result, without altering any non-
// whitespace rune (including '<' and '>').
func canonicalizeAnalysisWhitespace(text string) string {
	fields := strings.FieldsFunc(text, unicode.IsSpace)
	return strings.Join(fields, " ")
}

// AnalysisObjectiveDigest returns the deterministic SHA-256 hex digest of
// one objective class plus its normalized text, netstring-encoded so the
// class and text can never collide across a boundary shift.
func AnalysisObjectiveDigest(class AnalysisObjectiveClass, normalizedText string) string {
	h := sha256.New()
	writeAnalysisNetstring(h, string(class))
	writeAnalysisNetstring(h, normalizedText)
	return fmt.Sprintf("%x", h.Sum(nil))
}

// AnalysisByteRange is one half-open byte range [OffsetBytes,
// OffsetBytes+LengthBytes) over a source or a segment.
type AnalysisByteRange struct {
	OffsetBytes int64
	LengthBytes int64
}

// Validate enforces a non-negative offset and a strictly positive length.
func (r AnalysisByteRange) Validate() error {
	if r.OffsetBytes < 0 || r.LengthBytes <= 0 {
		return fmt.Errorf("%w: byte range must have a non-negative offset and a positive length", ErrAnalysisValidation)
	}
	return nil
}

// AnalysisSegment is one manifest record: its ordinal position, its byte
// range in the source, its content digest, the segmenter version that
// produced it, and its overlap with the previous segment.
type AnalysisSegment struct {
	Ordinal          int
	OffsetBytes      int64
	LengthBytes      int64
	SHA256           string
	SegmenterVersion string
	OverlapPrevBytes int64
}

// Validate enforces the structural shape of one segment record in
// isolation: non-negative ordinal, a valid byte range, a valid digest, a
// bounded segmenter version, and a non-negative overlap.
func (s AnalysisSegment) Validate() error {
	if s.Ordinal < 0 {
		return fmt.Errorf("%w: segment ordinal must not be negative", ErrAnalysisValidation)
	}
	if err := (AnalysisByteRange{OffsetBytes: s.OffsetBytes, LengthBytes: s.LengthBytes}).Validate(); err != nil {
		return err
	}
	if !validSHA256Hex(s.SHA256) {
		return fmt.Errorf("%w: segment requires a valid digest", ErrAnalysisValidation)
	}
	if !validAnalysisBoundedID(s.SegmenterVersion) {
		return fmt.Errorf("%w: segment requires a bounded segmenter version", ErrAnalysisValidation)
	}
	if s.OverlapPrevBytes < 0 || s.OverlapPrevBytes > HardMaxAnalysisOverlapBytes {
		return fmt.Errorf("%w: segment overlap must be between 0 and %d", ErrAnalysisValidation, HardMaxAnalysisOverlapBytes)
	}
	return nil
}

// AnalysisSegmentManifest is the complete, ordered segmentation of one
// verified source.
type AnalysisSegmentManifest struct {
	SourceSHA256 string
	SourceBytes  int64
	Segments     []AnalysisSegment
}

// Validate enforces: a valid source digest and a positive source size;
// consecutive ordinals starting at 0; strictly increasing offsets; no
// segment exceeding the declared source size; and, for every segment after
// the first, an overlap that is coherent with the previous segment's byte
// range (the segment's offset must equal the previous segment's end minus
// its declared overlap) and within the hard overlap bound. The first
// segment must declare zero overlap.
func (m AnalysisSegmentManifest) Validate() error {
	if !validSHA256Hex(m.SourceSHA256) {
		return fmt.Errorf("%w: segment manifest requires a valid source digest", ErrAnalysisValidation)
	}
	if m.SourceBytes <= 0 {
		return fmt.Errorf("%w: segment manifest requires a positive source size", ErrAnalysisValidation)
	}
	if len(m.Segments) == 0 {
		return fmt.Errorf("%w: segment manifest must not be empty", ErrAnalysisValidation)
	}
	var previous *AnalysisSegment
	for i, segment := range m.Segments {
		if err := segment.Validate(); err != nil {
			return err
		}
		if segment.Ordinal != i {
			return fmt.Errorf("%w: segment ordinals must be consecutive from 0, got %d at index %d", ErrAnalysisValidation, segment.Ordinal, i)
		}
		if segment.OffsetBytes+segment.LengthBytes > m.SourceBytes {
			return fmt.Errorf("%w: segment %d exceeds the declared source size", ErrAnalysisValidation, segment.Ordinal)
		}
		if previous == nil {
			if segment.OverlapPrevBytes != 0 {
				return fmt.Errorf("%w: the first segment must declare zero overlap", ErrAnalysisValidation)
			}
		} else {
			if segment.OffsetBytes <= previous.OffsetBytes {
				return fmt.Errorf("%w: segment offsets must strictly increase", ErrAnalysisValidation)
			}
			expectedOffset := previous.OffsetBytes + previous.LengthBytes - segment.OverlapPrevBytes
			if segment.OffsetBytes != expectedOffset {
				return fmt.Errorf("%w: segment %d overlap is not coherent with the previous segment", ErrAnalysisValidation, segment.Ordinal)
			}
			if segment.OverlapPrevBytes > previous.LengthBytes {
				return fmt.Errorf("%w: segment %d overlap exceeds the previous segment length", ErrAnalysisValidation, segment.Ordinal)
			}
		}
		copySegment := segment
		previous = &copySegment
	}
	return nil
}

// AnalysisCoverage is the overlap-aware union of the byte ranges verified by
// a segment manifest or claimed complete by a terminal packet.
type AnalysisCoverage struct {
	CoveredBytes int64
	Complete     bool
	Gaps         []AnalysisByteRange
}

// Coverage computes the union of the manifest's segment ranges, merging
// overlapping or adjacent ranges so overlap bytes are never counted twice,
// and reporting every uncovered gap up to SourceBytes. Complete is true only
// when there are no gaps and CoveredBytes equals SourceBytes exactly.
func (m AnalysisSegmentManifest) Coverage() AnalysisCoverage {
	var gaps []AnalysisByteRange
	var covered int64
	var cursor int64
	for _, segment := range m.Segments {
		start := segment.OffsetBytes
		end := segment.OffsetBytes + segment.LengthBytes
		if start > cursor {
			gaps = append(gaps, AnalysisByteRange{OffsetBytes: cursor, LengthBytes: start - cursor})
			cursor = start
		}
		if end > cursor {
			covered += end - cursor
			cursor = end
		}
	}
	if cursor < m.SourceBytes {
		gaps = append(gaps, AnalysisByteRange{OffsetBytes: cursor, LengthBytes: m.SourceBytes - cursor})
	}
	return AnalysisCoverage{
		CoveredBytes: covered,
		Complete:     len(gaps) == 0 && covered == m.SourceBytes,
		Gaps:         gaps,
	}
}

// StatusText renders the one user-facing sentence that reports this
// coverage. It always names verified byte ranges and never claims review,
// comprehension, or approval: coverage proves which source bytes were read
// and digest-verified, never that their content was judged, understood, or
// approved. Every caller that surfaces analysis coverage to a human (tool
// output, status text, dependent-dispatch messaging) must render coverage
// through this one function, so the distinction cannot drift between call
// sites.
func (c AnalysisCoverage) StatusText() string {
	if c.Complete {
		return fmt.Sprintf("Source coverage: %d verified bytes, complete. This reports which byte ranges were read and digest-verified, not that the content was reviewed, understood, or approved.", c.CoveredBytes)
	}
	return fmt.Sprintf("Source coverage: %d verified bytes, incomplete (%d gap range(s) remain). This reports which byte ranges were read and digest-verified, not that the content was reviewed, understood, or approved.", c.CoveredBytes, len(c.Gaps))
}

// AnalysisStatement is one bounded typed statement: a finding, a
// constraint, or an unresolved question.
type AnalysisStatement struct {
	Text string
}

// Validate enforces non-empty, valid UTF-8, bounded text.
func (s AnalysisStatement) Validate() error {
	if !utf8.ValidString(s.Text) {
		return fmt.Errorf("%w: statement is not valid UTF-8", ErrAnalysisValidation)
	}
	if strings.TrimSpace(s.Text) == "" {
		return fmt.Errorf("%w: statement must not be empty", ErrAnalysisValidation)
	}
	if utf8.RuneCountInString(s.Text) > HardMaxAnalysisStatementRunes {
		return fmt.Errorf("%w: statement exceeds %d code points", ErrAnalysisValidation, HardMaxAnalysisStatementRunes)
	}
	return nil
}

// AnalysisContradiction is one bounded typed statement carrying the
// identities of the conflicting leaf steps it was found across.
type AnalysisContradiction struct {
	Text      string
	LeafSteps []string
}

// Validate enforces a bounded statement and a bounded set of well-formed
// leaf step identifiers.
func (c AnalysisContradiction) Validate() error {
	if err := (AnalysisStatement{Text: c.Text}).Validate(); err != nil {
		return err
	}
	if len(c.LeafSteps) > HardMaxAnalysisEvidencePerLeaf {
		return fmt.Errorf("%w: contradiction cites more than %d leaf steps", ErrAnalysisValidation, HardMaxAnalysisEvidencePerLeaf)
	}
	for _, id := range c.LeafSteps {
		if !validAnalysisBoundedID(id) {
			return fmt.Errorf("%w: contradiction cites an invalid leaf step identity", ErrAnalysisValidation)
		}
	}
	return nil
}

// AnalysisLeaf is analysis_leaf_v1: the bounded structured output of one
// leaf model call over one segment.
type AnalysisLeaf struct {
	ObjectiveClass      AnalysisObjectiveClass
	ObjectiveDigest     string
	SegmentOrdinal      int
	Findings            []AnalysisStatement
	Constraints         []AnalysisStatement
	Contradictions      []AnalysisContradiction
	UnresolvedQuestions []AnalysisStatement
	EvidenceSelectors   []AnalysisByteRange
}

// Validate enforces the closed field vocabulary fail-closed: unknown
// objective class, an invalid objective digest, a negative segment ordinal,
// any list past its hard maximum, any malformed element, and no surviving
// finding or unresolved question are all ErrAnalysisValidation. An output
// with neither a finding nor an unresolved question is not a valid leaf: it
// carries no objective-relevant content for the reduction stage.
func (l AnalysisLeaf) Validate() error {
	if !ValidAnalysisObjectiveClass(string(l.ObjectiveClass)) {
		return fmt.Errorf("%w: leaf has unknown objective class %q", ErrAnalysisValidation, l.ObjectiveClass)
	}
	if !validSHA256Hex(l.ObjectiveDigest) {
		return fmt.Errorf("%w: leaf requires a valid objective digest", ErrAnalysisValidation)
	}
	if l.SegmentOrdinal < 0 {
		return fmt.Errorf("%w: leaf segment ordinal must not be negative", ErrAnalysisValidation)
	}
	if err := validateAnalysisStatementList(l.Findings, HardMaxAnalysisFindings, "leaf findings"); err != nil {
		return err
	}
	if err := validateAnalysisStatementList(l.Constraints, HardMaxAnalysisConstraints, "leaf constraints"); err != nil {
		return err
	}
	if len(l.Contradictions) > HardMaxAnalysisContradictions {
		return fmt.Errorf("%w: leaf contradictions exceed %d", ErrAnalysisValidation, HardMaxAnalysisContradictions)
	}
	for _, contradiction := range l.Contradictions {
		if err := contradiction.Validate(); err != nil {
			return err
		}
	}
	if err := validateAnalysisStatementList(l.UnresolvedQuestions, HardMaxAnalysisUnresolvedQuestions, "leaf unresolved questions"); err != nil {
		return err
	}
	if len(l.EvidenceSelectors) > HardMaxAnalysisEvidencePerLeaf {
		return fmt.Errorf("%w: leaf evidence selectors exceed %d", ErrAnalysisValidation, HardMaxAnalysisEvidencePerLeaf)
	}
	for _, selector := range l.EvidenceSelectors {
		if err := selector.Validate(); err != nil {
			return err
		}
	}
	if len(l.Findings) == 0 && len(l.UnresolvedQuestions) == 0 {
		return fmt.Errorf("%w: leaf has no surviving finding and no surviving unresolved question", ErrAnalysisValidation)
	}
	return nil
}

func validateAnalysisStatementList(statements []AnalysisStatement, max int, label string) error {
	if len(statements) > max {
		return fmt.Errorf("%w: %s exceed %d", ErrAnalysisValidation, label, max)
	}
	for _, statement := range statements {
		if err := statement.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// AnalysisPacket is analysis_packet_v1: the terminal decision packet, also
// used as the output shape of every reduction node above the leaf level.
type AnalysisPacket struct {
	ObjectiveClass      AnalysisObjectiveClass
	ObjectiveDigest     string
	Findings            []AnalysisStatement
	Constraints         []AnalysisStatement
	Contradictions      []AnalysisContradiction
	UnresolvedQuestions []AnalysisStatement
	EvidenceRefs        []string
	SourceSHA256        string
	Coverage            AnalysisCoverage
	Lineage             []string
}

// Validate enforces the closed field vocabulary fail-closed, exactly like
// AnalysisLeaf, plus the packet-only fields: a bounded, well-formed set of
// evidence reference identities, a valid source digest, and a bounded,
// well-formed, ordered lineage of child step identities. An empty output
// (no finding and no unresolved question) is never a valid packet.
func (p AnalysisPacket) Validate() error {
	if !ValidAnalysisObjectiveClass(string(p.ObjectiveClass)) {
		return fmt.Errorf("%w: packet has unknown objective class %q", ErrAnalysisValidation, p.ObjectiveClass)
	}
	if !validSHA256Hex(p.ObjectiveDigest) {
		return fmt.Errorf("%w: packet requires a valid objective digest", ErrAnalysisValidation)
	}
	if err := validateAnalysisStatementList(p.Findings, HardMaxAnalysisFindings, "packet findings"); err != nil {
		return err
	}
	if err := validateAnalysisStatementList(p.Constraints, HardMaxAnalysisConstraints, "packet constraints"); err != nil {
		return err
	}
	if len(p.Contradictions) > HardMaxAnalysisContradictions {
		return fmt.Errorf("%w: packet contradictions exceed %d", ErrAnalysisValidation, HardMaxAnalysisContradictions)
	}
	for _, contradiction := range p.Contradictions {
		if err := contradiction.Validate(); err != nil {
			return err
		}
	}
	if err := validateAnalysisStatementList(p.UnresolvedQuestions, HardMaxAnalysisUnresolvedQuestions, "packet unresolved questions"); err != nil {
		return err
	}
	if len(p.EvidenceRefs) > HardMaxAnalysisEvidencePerPacket {
		return fmt.Errorf("%w: packet evidence references exceed %d", ErrAnalysisValidation, HardMaxAnalysisEvidencePerPacket)
	}
	for _, ref := range p.EvidenceRefs {
		if !validAnalysisBoundedID(ref) {
			return fmt.Errorf("%w: packet evidence reference is not a bounded identity", ErrAnalysisValidation)
		}
	}
	if !validSHA256Hex(p.SourceSHA256) {
		return fmt.Errorf("%w: packet requires a valid source digest", ErrAnalysisValidation)
	}
	if len(p.Lineage) > HardMaxAnalysisReductionFanIn {
		return fmt.Errorf("%w: packet lineage exceeds %d", ErrAnalysisValidation, HardMaxAnalysisReductionFanIn)
	}
	for _, id := range p.Lineage {
		if !validAnalysisBoundedID(id) {
			return fmt.Errorf("%w: packet lineage cites an invalid step identity", ErrAnalysisValidation)
		}
	}
	if len(p.Findings) == 0 && len(p.UnresolvedQuestions) == 0 {
		return fmt.Errorf("%w: packet has no surviving finding and no surviving unresolved question", ErrAnalysisValidation)
	}
	return nil
}

// AnalysisStepKind is the closed set of durable analysis step kinds.
type AnalysisStepKind string

const (
	AnalysisStepLeaf      AnalysisStepKind = "leaf"
	AnalysisStepReduction AnalysisStepKind = "reduction"
)

// ValidAnalysisStepKind reports whether kind is a known step kind.
func ValidAnalysisStepKind(kind AnalysisStepKind) bool {
	switch kind {
	case AnalysisStepLeaf, AnalysisStepReduction:
		return true
	default:
		return false
	}
}

// AnalysisStepState is the closed lifecycle of one durable analysis step.
type AnalysisStepState string

const (
	AnalysisStepPrepared  AnalysisStepState = "prepared"
	AnalysisStepClaimed   AnalysisStepState = "claimed"
	AnalysisStepCompleted AnalysisStepState = "completed"
	AnalysisStepFailed    AnalysisStepState = "failed"
)

// ValidAnalysisStepState reports whether state is a known step state.
func ValidAnalysisStepState(state AnalysisStepState) bool {
	switch state {
	case AnalysisStepPrepared, AnalysisStepClaimed, AnalysisStepCompleted, AnalysisStepFailed:
		return true
	default:
		return false
	}
}

// AnalysisState is the closed lifecycle of one durable analysis.
type AnalysisState string

const (
	AnalysisPreparing AnalysisState = "preparing"
	AnalysisRunning   AnalysisState = "running"
	AnalysisCompleted AnalysisState = "completed"
	AnalysisFailed    AnalysisState = "failed"
)

// ValidAnalysisState reports whether state is a known analysis state.
func ValidAnalysisState(state AnalysisState) bool {
	switch state {
	case AnalysisPreparing, AnalysisRunning, AnalysisCompleted, AnalysisFailed:
		return true
	default:
		return false
	}
}

// AnalysisFailureCode is the closed set of typed analysis failure codes.
// Every failure that can end an analysis or a step is one of these codes;
// an unmapped internal error is never surfaced as analysis truth.
type AnalysisFailureCode string

const (
	// AnalysisFailureSourceTooLarge applies when the verified source is
	// larger than max_leaves * max_segment_bytes: no manifest can be built.
	AnalysisFailureSourceTooLarge AnalysisFailureCode = "analysis_source_too_large"
	// AnalysisFailureSegmentInvalid applies when the segment manifest or one
	// of its byte ranges is invalid, discovered before any model contact.
	AnalysisFailureSegmentInvalid AnalysisFailureCode = "analysis_segment_invalid"
	// AnalysisFailureLeafSchemaInvalid applies when a leaf's structured
	// output still fails validation after its configured retry budget.
	AnalysisFailureLeafSchemaInvalid AnalysisFailureCode = "analysis_leaf_schema_invalid"
	// AnalysisFailureEvidenceSelectorInvalid applies when a model-selected
	// evidence range falls outside its own segment, crosses a non-rune
	// boundary, or fails host digest verification.
	AnalysisFailureEvidenceSelectorInvalid AnalysisFailureCode = "analysis_evidence_selector_invalid"
	// AnalysisFailureReductionIrreducible applies when findings cannot fit a
	// bounded reduction without dropping objective-required evidence.
	AnalysisFailureReductionIrreducible AnalysisFailureCode = "analysis_reduction_irreducible"
	// AnalysisFailureCoverageIncomplete applies when the manifest or the
	// terminal packet leaves an uncovered byte gap.
	AnalysisFailureCoverageIncomplete AnalysisFailureCode = "analysis_coverage_incomplete"
	// AnalysisFailureBundleTooLarge applies when a requested downstream
	// bundle exceeds its configured or hard byte bound.
	AnalysisFailureBundleTooLarge AnalysisFailureCode = "analysis_bundle_too_large"
	// AnalysisFailureWallTimeExceeded applies when the configured wall-clock
	// budget for one analysis elapses before completion.
	AnalysisFailureWallTimeExceeded AnalysisFailureCode = "analysis_wall_time_exceeded"
	// AnalysisFailureAttemptsExhausted applies when a step's retry budget is
	// exhausted without a durable result.
	AnalysisFailureAttemptsExhausted AnalysisFailureCode = "analysis_attempts_exhausted"
	// AnalysisFailureIdentityStale applies when the source result identity
	// or the workstream binding no longer matches the analysis row.
	AnalysisFailureIdentityStale AnalysisFailureCode = "analysis_identity_stale"
)

// ValidAnalysisFailureCode reports whether code is a known, closed failure
// code.
func ValidAnalysisFailureCode(code string) bool {
	switch AnalysisFailureCode(code) {
	case AnalysisFailureSourceTooLarge, AnalysisFailureSegmentInvalid, AnalysisFailureLeafSchemaInvalid,
		AnalysisFailureEvidenceSelectorInvalid, AnalysisFailureReductionIrreducible, AnalysisFailureCoverageIncomplete,
		AnalysisFailureBundleTooLarge, AnalysisFailureWallTimeExceeded, AnalysisFailureAttemptsExhausted,
		AnalysisFailureIdentityStale:
		return true
	default:
		return false
	}
}

// AnalysisStepClaim is the structural claim token returned by a step claim.
// Terminal and retry CAS operations must present this exact token, so a
// worker whose lease expired and was reclaimed can never mutate the new
// owner's claim. This is a structural copy of domain.KnowledgeQueueClaim
// (TRD 06): that shape already proved this property, and no second claim
// design is introduced here.
type AnalysisStepClaim struct {
	AnalysisID string
	StepID     string
	Generation int64
	LeaseUntil time.Time
}

// Validate enforces a well-formed analysis identity, a bounded step
// identity, and a non-negative generation.
func (c AnalysisStepClaim) Validate() error {
	if !ValidAnalysisID(c.AnalysisID) {
		return fmt.Errorf("%w: step claim requires a valid analysis id", ErrAnalysisValidation)
	}
	if !validAnalysisBoundedID(c.StepID) {
		return fmt.Errorf("%w: step claim requires a bounded step id", ErrAnalysisValidation)
	}
	if c.Generation < 0 {
		return fmt.Errorf("%w: step claim generation must not be negative", ErrAnalysisValidation)
	}
	return nil
}

// AnalysisModelFingerprint returns the frozen SHA-256 fingerprint over the
// NUL-joined sequence `provider_id NUL model NUL analysis-v1`. It follows
// the same shape as ModelFingerprint (TRD 06's embedding fingerprint) but
// carries its own version tag, so an analysis fingerprint and an embedding
// fingerprint can never collide even over the same provider and model
// string. The empty result_analysis.model block falls back to the main
// model profile; the caller passes that profile's own provider id and
// model name here so the fallback is still explicit in the fingerprint,
// per TRD 07's Concurrency and Model Profile section.
func AnalysisModelFingerprint(providerID, model string) string {
	sum := sha256.Sum256([]byte(providerID + "\x00" + model + "\x00" + "analysis-v1"))
	return hex.EncodeToString(sum[:])
}

// ResultAnalysisHealth is the offline, content-free doctor view of v40
// result analysis state (checkpoint 6): bounded counts and categories only.
// It never carries an analysis id, an objective, a finding, an evidence
// excerpt, or a digest, matching the same discipline
// KnowledgeRetrievalHealth already established for the v39 retrieval state.
type ResultAnalysisHealth struct {
	// SchemaVersion is the database's PRAGMA user_version. A doctor caller
	// compares it against the binary's own current schema version.
	SchemaVersion int
	// StepQueueDepth counts analysis_steps rows in 'prepared' or 'claimed'
	// state: pending or in-flight work, excluding terminal rows.
	StepQueueDepth int
	// ExpiredLeases counts 'claimed' steps whose lease has already elapsed:
	// work a restarted or recovered worker will reclaim on its next tick.
	ExpiredLeases int
	// NonTerminalAnalyses counts result_analyses rows in 'preparing' or
	// 'running' state.
	NonTerminalAnalyses int
	// IncompleteCoverageAnalyses counts 'running' analyses whose root
	// reduction step has not yet completed, so their terminal packet's
	// coverage cannot yet be proven complete.
	IncompleteCoverageAnalyses int
}
