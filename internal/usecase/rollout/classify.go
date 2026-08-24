package rollout

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var baselinePattern = regexp.MustCompile(`^jobs=(\d+);activations=(\d+)$`)

// FormatBaseline renders the durable baseline value format.
func FormatBaseline(baseline IdentityBaseline) string {
	return fmt.Sprintf("jobs=%d;activations=%d", baseline.JobsCompletedWithoutResultIdentity, baseline.ActivationsWithoutContent)
}

// ParseBaseline reads the durable baseline value. Both counters must be
// non-negative decimal integers.
func ParseBaseline(raw string) (IdentityBaseline, bool) {
	match := baselinePattern.FindStringSubmatch(raw)
	if match == nil {
		return IdentityBaseline{}, false
	}
	jobs, jobsErr := strconv.ParseUint(match[1], 10, 64)
	activations, actErr := strconv.ParseUint(match[2], 10, 64)
	if jobsErr != nil || actErr != nil {
		return IdentityBaseline{}, false
	}
	return IdentityBaseline{JobsCompletedWithoutResultIdentity: int(jobs), ActivationsWithoutContent: int(activations)}, true
}

// ParseNonNegativeDecimal accepts only plain non-negative decimal integers.
func ParseNonNegativeDecimal(raw string) (int64, bool) {
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 0 {
		return 0, false
	}
	return value, true
}

// ParseSHA256Hex requires exactly 64 lowercase hexadecimal characters.
func ParseSHA256Hex(raw string) (string, bool) {
	if len(raw) != 64 {
		return "", false
	}
	for _, r := range raw {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return "", false
		}
	}
	return raw, true
}

// ParseBackupSourceVersion requires a schema version this binary can read.
func ParseBackupSourceVersion(raw string) (int, bool) {
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 || value > MaxSourceVersion {
		return 0, false
	}
	return value, true
}

// ParseRFC3339 accepts exactly RFC 3339 timestamps.
func ParseRFC3339(raw string) (time.Time, bool) {
	value, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, false
	}
	return value, true
}

func corruptState(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrRolloutStateCorrupt, fmt.Sprintf(format, args...))
}

// ClassifyBackupIdentity reads the six backup-related keys and returns the
// one shape they form: all five identity keys valid together, the
// not-required marker alone, or nothing at all. Every other reading is a
// Corrupt error naming the observed keys.
func ClassifyBackupIdentity(state RolloutState) (BackupIdentityShape, error) {
	five := []struct {
		present bool
		valid   bool
		name    string
	}{
		{state.BackupPathPresent, state.BackupPathValid, KeyBackupPath},
		{state.BackupBytesPresent, state.BackupBytesValid, KeyBackupBytes},
		{state.BackupSHA256Present, state.BackupSHA256Valid, KeyBackupSHA256},
		{state.BackupSourceVersionPresent, state.BackupSourceVersionValid, KeyBackupSourceVersion},
		{state.BackupVerifiedAtPresent, state.BackupVerifiedAtValid, KeyBackupVerifiedAt},
	}
	presentCount := 0
	var presentNames, missingNames []string
	for _, key := range five {
		switch {
		case key.present && !key.valid:
			return 0, corruptState("%s is present but fails its format check", key.name)
		case key.present:
			presentCount++
			presentNames = append(presentNames, key.name)
		default:
			missingNames = append(missingNames, key.name)
		}
	}
	notRequired := state.BackupNotRequiredAtPresent
	if notRequired && !state.BackupNotRequiredAtValid {
		return 0, corruptState("%s is present but is not a valid RFC 3339 timestamp", KeyBackupNotRequiredAt)
	}
	switch {
	case presentCount == 0 && !notRequired:
		return BackupIdentityAbsent, nil
	case presentCount == len(five) && !notRequired:
		return BackupIdentityPresent, nil
	case presentCount == 0 && notRequired:
		return BackupIdentityNotRequired, nil
	case notRequired:
		return 0, corruptState("%s is present together with backup identity keys %s; the two shapes are mutually exclusive", KeyBackupNotRequiredAt, strings.Join(presentNames, ", "))
	default:
		return 0, corruptState("backup identity keys are partially present: have [%s], missing [%s]; the writer sets all five together", strings.Join(presentNames, ", "), strings.Join(missingNames, ", "))
	}
}

// ClassifyRollout maps one live schema read plus one durable rollout-state
// reading onto exactly one Recovery Table row. schema must be each caller's
// own live read in the same pass as state; for row 2 it additionally binds
// the recorded backup source version to that live schema. Unmatched readings
// fail closed with Corrupt instead of guessing.
func ClassifyRollout(schema int, state RolloutState) (RolloutRow, error) {
	if schema > TargetVersion {
		return RolloutRowFreshCapture, fmt.Errorf("%w: found v%d, target is v%d", ErrFutureSchema, schema, TargetVersion)
	}
	if schema < MinSourceVersion {
		return RolloutRowFreshCapture, UnsupportedSourceSchemaError{Found: schema, MinSupported: MinSourceVersion, MaxSupported: MaxSourceVersion}
	}
	if err := classifyFormatFailures(state); err != nil {
		return RolloutRowFreshCapture, err
	}
	shape, err := ClassifyBackupIdentity(state)
	if err != nil {
		return RolloutRowFreshCapture, err
	}
	if state.PostflightPresent != state.PostflightDetailPresent {
		if state.PostflightPresent {
			return RolloutRowFreshCapture, corruptState("%s is present without %s; both postflight keys commit together or neither exists", KeyPostflightStatus, KeyPostflightDetail)
		}
		return RolloutRowFreshCapture, corruptState("%s is present without %s; both postflight keys commit together or neither exists", KeyPostflightDetail, KeyPostflightStatus)
	}
	if state.BaselinePresent != state.CutoffPresent {
		if state.BaselinePresent {
			return RolloutRowFreshCapture, corruptState("%s is present without %s; the writer commits both keys in one transaction", KeyBaseline, KeyCutoff)
		}
		return RolloutRowFreshCapture, corruptState("%s is present without %s; the writer commits both keys in one transaction", KeyCutoff, KeyBaseline)
	}
	baselineCutoff := state.BaselinePresent && state.CutoffPresent
	atTarget := schema == TargetVersion
	misplacedPostflight := func(shapeName string) error {
		return corruptState("%s=%q is present on the %s shape; a valid status can exist only on row-4/row-5 shape (schema 41 with baseline+cutoff and a backup-identity shape)", KeyPostflightStatus, state.PostflightStatus, shapeName)
	}

	switch {
	case !atTarget && shape == BackupIdentityNotRequired:
		return RolloutRowFreshCapture, corruptState("%s is present but the marker requires schema %d, found v%d", KeyBackupNotRequiredAt, TargetVersion, schema)

	case !atTarget && baselineCutoff:
		if shape != BackupIdentityPresent {
			return RolloutRowFreshCapture, corruptState("baseline+cutoff are present at schema v%d but backup identity keys [%s] are absent; rows 1 and 2 never leave this shape", schema, strings.Join(fiveKeyNames(), ", "))
		}
		if state.BackupSourceVersion != schema {
			return RolloutRowFreshCapture, corruptState("recorded %s=%d does not equal the live source schema v%d; equality with this invocation's own read is required", KeyBackupSourceVersion, state.BackupSourceVersion, schema)
		}
		if state.PostflightPresent {
			return RolloutRowFreshCapture, misplacedPostflight("row-2 resume (schema " + strconv.Itoa(schema) + " with baseline+cutoff+backup identity)")
		}
		return RolloutRowFreshResume, nil

	case !atTarget:
		if shape != BackupIdentityAbsent {
			return RolloutRowFreshCapture, corruptState("backup identity keys [%s] are present without %s/%s at schema v%d; the two groups are written together", strings.Join(presentIdentityKeys(state), ", "), KeyBaseline, KeyCutoff, schema)
		}
		if state.PostflightPresent {
			return RolloutRowFreshCapture, misplacedPostflight("row-1 fresh capture (baseline+cutoff absent)")
		}
		return RolloutRowFreshCapture, nil

	case !baselineCutoff:
		if shape != BackupIdentityAbsent {
			return RolloutRowFreshCapture, corruptState("backup identity keys [%s] are present without %s/%s at schema 41; the two groups are written together", strings.Join(presentIdentityKeys(state), ", "), KeyBaseline, KeyCutoff)
		}
		if state.PostflightPresent {
			return RolloutRowFreshCapture, misplacedPostflight("row-3 adoption (no rollout keys)")
		}
		return RolloutRowAdoption, nil

	case shape == BackupIdentityAbsent:
		return RolloutRowResumeNeeded, corruptState("baseline+cutoff are present at schema 41 but all five backup identity keys [%s] are absent; no documented crash window leaves this shape", strings.Join(fiveKeyNames(), ", "))

	case !state.PostflightPresent || state.PostflightStatus == PostflightFailed:
		return RolloutRowResumeNeeded, nil

	default:
		return RolloutRowAlreadyComplete, nil
	}
}

func fiveKeyNames() []string {
	return []string{KeyBackupPath, KeyBackupBytes, KeyBackupSHA256, KeyBackupSourceVersion, KeyBackupVerifiedAt}
}

func presentIdentityKeys(state RolloutState) []string {
	var names []string
	for _, key := range []struct {
		present bool
		name    string
	}{
		{state.BackupPathPresent, KeyBackupPath},
		{state.BackupBytesPresent, KeyBackupBytes},
		{state.BackupSHA256Present, KeyBackupSHA256},
		{state.BackupSourceVersionPresent, KeyBackupSourceVersion},
		{state.BackupVerifiedAtPresent, KeyBackupVerifiedAt},
		{state.BackupNotRequiredAtPresent, KeyBackupNotRequiredAt},
	} {
		if key.present {
			names = append(names, key.name)
		}
	}
	return names
}

func classifyFormatFailures(state RolloutState) error {
	checks := []struct {
		present bool
		valid   bool
		reason  string
	}{
		{state.BaselinePresent, state.BaselineValid, fmt.Sprintf("%s is present but does not parse as jobs={n};activations={m} with non-negative integers", KeyBaseline)},
		{state.CutoffPresent, state.CutoffValid, fmt.Sprintf("%s is present but is not a non-negative decimal integer", KeyCutoff)},
		{state.BackupPathPresent, state.BackupPathValid, fmt.Sprintf("%s is present but is not an absolute path", KeyBackupPath)},
		{state.BackupBytesPresent, state.BackupBytesValid, fmt.Sprintf("%s is present but is not a non-negative decimal integer", KeyBackupBytes)},
		{state.BackupSHA256Present, state.BackupSHA256Valid, fmt.Sprintf("%s is present but is not 64 lowercase hex characters", KeyBackupSHA256)},
		{state.BackupSourceVersionPresent, state.BackupSourceVersionValid, fmt.Sprintf("%s is present but is not an integer in [%d, %d]", KeyBackupSourceVersion, MinSourceVersion, MaxSourceVersion)},
		{state.BackupVerifiedAtPresent, state.BackupVerifiedAtValid, fmt.Sprintf("%s is present but is not a valid RFC 3339 timestamp", KeyBackupVerifiedAt)},
		{state.BackupNotRequiredAtPresent, state.BackupNotRequiredAtValid, fmt.Sprintf("%s is present but is not a valid RFC 3339 timestamp", KeyBackupNotRequiredAt)},
		{state.PostflightPresent, state.PostflightValid, fmt.Sprintf("%s is present but its value is neither %q nor %q", KeyPostflightStatus, PostflightPassed, PostflightFailed)},
	}
	for _, check := range checks {
		if check.present && !check.valid {
			return corruptState("%s", check.reason)
		}
	}
	return nil
}

// IsRolloutComplete reports row 5 only. It propagates a Corrupt,
// ErrFutureSchema, or ErrUnsupportedSourceSchema error unchanged rather than
// folding it into false.
func IsRolloutComplete(schema int, state RolloutState) (bool, error) {
	row, err := ClassifyRollout(schema, state)
	if err != nil {
		return false, err
	}
	return row == RolloutRowAlreadyComplete, nil
}

// ComparePostflight applies the carve-out rule: fields without a legacy
// carve-out never appear here because their fatal check is nonzero-only;
// the two carve-out fields fail when they exceed the durable baseline.
func ComparePostflight(baseline IdentityBaseline, post IdentityBaseline) (ok bool, field string, delta int) {
	if post.JobsCompletedWithoutResultIdentity > baseline.JobsCompletedWithoutResultIdentity {
		delta := post.JobsCompletedWithoutResultIdentity - baseline.JobsCompletedWithoutResultIdentity
		return false, "JobsCompletedWithoutResultIdentity", delta
	}
	if post.ActivationsWithoutContent > baseline.ActivationsWithoutContent {
		delta := post.ActivationsWithoutContent - baseline.ActivationsWithoutContent
		return false, "ActivationsWithoutContent", delta
	}
	return true, "", 0
}
