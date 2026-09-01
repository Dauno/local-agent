package rollout

import (
	"errors"
	"strings"
	"testing"
	"testing/quick"
	"time"
)

const testSHA256 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

var testVerifiedAt = time.Date(2026, 8, 21, 14, 30, 0, 0, time.UTC)

func withBaselineCutoff(state RolloutState, jobs, activations int, cutoff int64) RolloutState {
	state.BaselinePresent = true
	state.BaselineValid = true
	state.Baseline = IdentityBaseline{JobsCompletedWithoutResultIdentity: jobs, ActivationsWithoutContent: activations}
	state.CutoffPresent = true
	state.CutoffValid = true
	state.CutoffUnixNanos = cutoff
	return state
}

func withBackupIdentity(state RolloutState, sourceVersion int) RolloutState {
	state.BackupPathPresent = true
	state.BackupPathValid = true
	state.BackupPath = "/tmp/local-agent.pre-v41.v33.backup.db"
	state.BackupBytesPresent = true
	state.BackupBytesValid = true
	state.BackupBytes = 4096
	state.BackupSHA256Present = true
	state.BackupSHA256Valid = true
	state.BackupSHA256 = testSHA256
	state.BackupSourceVersionPresent = true
	state.BackupSourceVersionValid = true
	state.BackupSourceVersion = sourceVersion
	state.BackupVerifiedAtPresent = true
	state.BackupVerifiedAtValid = true
	state.BackupVerifiedAt = testVerifiedAt
	return state
}

func withNotRequired(state RolloutState) RolloutState {
	state.BackupNotRequiredAtPresent = true
	state.BackupNotRequiredAtValid = true
	state.BackupNotRequiredAt = testVerifiedAt
	return state
}

func withPostflight(state RolloutState, status PostflightStatus) RolloutState {
	state.PostflightPresent = true
	state.PostflightValid = true
	state.PostflightStatus = status
	state.PostflightDetailPresent = true
	state.PostflightDetail = "postflight detail"
	return state
}

func TestUpgradeKindForRowCollapsesOnlyRowsOneAndTwo(t *testing.T) {
	if UpgradeKindForRow(RolloutRowFreshCapture) != UpgradeFreshUpgrade ||
		UpgradeKindForRow(RolloutRowFreshResume) != UpgradeFreshUpgrade {
		t.Fatal("rows 1 and 2 must share the FreshUpgrade label")
	}
	if RolloutRowFreshCapture == RolloutRowFreshResume {
		t.Fatal("row 1 and row 2 are distinct RolloutRow values")
	}
	pairs := []struct {
		row  RolloutRow
		kind UpgradeKind
	}{
		{RolloutRowAdoption, UpgradeAdoption},
		{RolloutRowResumeNeeded, UpgradeResumeNeeded},
		{RolloutRowAlreadyComplete, UpgradeAlreadyComplete},
	}
	for _, pair := range pairs {
		if got := UpgradeKindForRow(pair.row); got != pair.kind {
			t.Fatalf("UpgradeKindForRow(%d) = %d, want %d", pair.row, got, pair.kind)
		}
	}
}

func TestClassifyBackupIdentityThreeShapes(t *testing.T) {
	shape, err := ClassifyBackupIdentity(RolloutState{})
	if err != nil || shape != BackupIdentityAbsent {
		t.Fatalf("empty state shape=%d err=%v, want Absent", shape, err)
	}
	shape, err = ClassifyBackupIdentity(withBackupIdentity(RolloutState{}, 33))
	if err != nil || shape != BackupIdentityPresent {
		t.Fatalf("five-key state shape=%d err=%v, want Present", shape, err)
	}
	shape, err = ClassifyBackupIdentity(withNotRequired(RolloutState{}))
	if err != nil || shape != BackupIdentityNotRequired {
		t.Fatalf("marker state shape=%d err=%v, want NotRequired", shape, err)
	}
}

func TestClassifyBackupIdentityRejectsEveryNonShapeReading(t *testing.T) {
	cases := []struct {
		name  string
		state RolloutState
		want  string
	}{
		{
			name: "two of five backup keys",
			state: func() RolloutState {
				s := withBackupIdentity(RolloutState{}, 33)
				s.BackupBytesPresent = false
				s.BackupBytesValid = false
				return s
			}(),
			want: KeyBackupBytes,
		},
		{
			name: "four of five backup keys",
			state: func() RolloutState {
				s := withBackupIdentity(RolloutState{}, 33)
				s.BackupVerifiedAtPresent = false
				s.BackupVerifiedAtValid = false
				return s
			}(),
			want: KeyBackupVerifiedAt,
		},
		{
			name:  "marker beside full identity",
			state: withNotRequired(withBackupIdentity(RolloutState{}, 33)),
			want:  KeyBackupNotRequiredAt,
		},
		{
			name: "relative backup path",
			state: func() RolloutState {
				s := withBackupIdentity(RolloutState{}, 33)
				s.BackupPathValid = false
				s.BackupPath = ""
				return s
			}(),
			want: KeyBackupPath,
		},
		{
			name: "uppercase sha256",
			state: func() RolloutState {
				s := withBackupIdentity(RolloutState{}, 33)
				s.BackupSHA256Valid = false
				s.BackupSHA256 = strings.ToUpper(testSHA256)
				return s
			}(),
			want: KeyBackupSHA256,
		},
		{
			name: "unparseable verified_at",
			state: func() RolloutState {
				s := withBackupIdentity(RolloutState{}, 33)
				s.BackupVerifiedAtValid = false
				s.BackupVerifiedAt = time.Time{}
				return s
			}(),
			want: KeyBackupVerifiedAt,
		},
		{
			name: "unparseable marker timestamp",
			state: func() RolloutState {
				s := withNotRequired(RolloutState{})
				s.BackupNotRequiredAtValid = false
				s.BackupNotRequiredAt = time.Time{}
				return s
			}(),
			want: KeyBackupNotRequiredAt,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			shape, err := ClassifyBackupIdentity(testCase.state)
			if err == nil {
				t.Fatalf("shape=%d, want Corrupt", shape)
			}
			if !errors.Is(err, ErrRolloutStateCorrupt) {
				t.Fatalf("err = %v, want ErrRolloutStateCorrupt", err)
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("err = %v, want it to name %s", err, testCase.want)
			}
		})
	}
}

func TestClassifyRolloutMatchesRecoveryRows(t *testing.T) {
	cases := []struct {
		name   string
		schema int
		state  RolloutState
		want   RolloutRow
	}{
		{"row 1 floor", 33, RolloutState{}, RolloutRowFreshCapture},
		{"row 1 ceiling", 40, RolloutState{}, RolloutRowFreshCapture},
		{"row 2 intact", 33, withBackupIdentity(withBaselineCutoff(RolloutState{}, 21, 12, 100), 33), RolloutRowFreshResume},
		{"row 2 ceiling", 40, withBackupIdentity(withBaselineCutoff(RolloutState{}, 0, 0, 100), 40), RolloutRowFreshResume},
		{"row 3 adoption", TargetVersion, RolloutState{}, RolloutRowAdoption},
		{"row 4 failed postflight", TargetVersion, withPostflight(withBackupIdentity(withBaselineCutoff(RolloutState{}, 0, 0, 5), 41), PostflightFailed), RolloutRowResumeNeeded},
		{"row 4 missing postflight with marker", TargetVersion, withNotRequired(withBaselineCutoff(RolloutState{}, 0, 0, 5)), RolloutRowResumeNeeded},
		{"row 5 complete", TargetVersion, withPostflight(withBackupIdentity(withBaselineCutoff(RolloutState{}, 0, 0, 5), 41), PostflightPassed), RolloutRowAlreadyComplete},
		{"row 5 complete with marker", TargetVersion, withPostflight(withNotRequired(withBaselineCutoff(RolloutState{}, 0, 0, 5)), PostflightPassed), RolloutRowAlreadyComplete},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			row, err := ClassifyRollout(testCase.schema, testCase.state)
			if err != nil {
				t.Fatalf("err = %v, want %d", err, testCase.want)
			}
			if row != testCase.want {
				t.Fatalf("row = %d, want %d", row, testCase.want)
			}
		})
	}
}

func TestClassifyRolloutRefusesOutOfRangeSchemas(t *testing.T) {
	for _, schema := range []int{TargetVersion + 1, 41 + 7, 32, 20, 14, 0} {
		row, err := ClassifyRollout(schema, RolloutState{})
		if schema > TargetVersion {
			if !errors.Is(err, ErrFutureSchema) {
				t.Fatalf("schema %d err = %v, want ErrFutureSchema", schema, err)
			}
			continue
		}
		var typed UnsupportedSourceSchemaError
		if !errors.As(err, &typed) {
			t.Fatalf("schema %d err = %v, want UnsupportedSourceSchemaError", schema, err)
		}
		if typed.Found != schema || typed.MinSupported != MinSourceVersion || typed.MaxSupported != MaxSourceVersion {
			t.Fatalf("schema %d typed = %+v", schema, typed)
		}
		if !strings.Contains(err.Error(), "[33, 46]") {
			t.Fatalf("schema %d err = %v, want it to name the supported range", schema, err)
		}
		_ = row
	}
}

func TestClassifyRolloutBindsRowTwoToLiveSchema(t *testing.T) {
	_, err := ClassifyRollout(34, withBackupIdentity(withBaselineCutoff(RolloutState{}, 1, 2, 3), 33))
	if !errors.Is(err, ErrRolloutStateCorrupt) {
		t.Fatalf("err = %v, want Corrupt for mismatched binding", err)
	}
	for _, needle := range []string{"33", "34", KeyBackupSourceVersion} {
		if !strings.Contains(err.Error(), needle) {
			t.Fatalf("err = %v, want it to name %s", err, needle)
		}
	}
}

func TestClassifyRolloutFailsClosedOnPartialStates(t *testing.T) {
	baselineOnly := RolloutState{
		BaselinePresent: true,
		BaselineValid:   true,
	}
	cutoffOnly := RolloutState{
		CutoffPresent: true,
		CutoffValid:   true,
	}
	malformedBaseline := RolloutState{
		BaselinePresent: true,
		BaselineValid:   false,
	}
	malformedCutoff := RolloutState{
		CutoffPresent: true,
		CutoffValid:   false,
	}

	statusWithoutDetail := RolloutState{
		PostflightPresent: true,
		PostflightValid:   true,
		PostflightStatus:  PostflightPassed,
	}
	detailWithoutStatus := RolloutState{
		PostflightDetailPresent: true,
		PostflightDetail:        "orphan",
	}
	unknownStatus := RolloutState{
		PostflightPresent: true,
		PostflightValid:   false,
	}

	cases := []struct {
		name     string
		schema   int
		state    RolloutState
		contains []string
	}{
		{"baseline without cutoff", 41, baselineOnly, []string{KeyBaseline, KeyCutoff}},
		{"cutoff without baseline", 41, cutoffOnly, []string{KeyCutoff, KeyBaseline}},
		{"malformed baseline", 41, malformedBaseline, []string{KeyBaseline}},
		{"malformed cutoff", 41, malformedCutoff, []string{KeyCutoff}},
		{"baseline+cutoff without identity at target", TargetVersion, withBaselineCutoff(RolloutState{}, 0, 0, 1), []string{KeyBackupPath, KeyBackupVerifiedAt}},
		{"baseline+cutoff without identity below target", 36, withBaselineCutoff(RolloutState{}, 0, 0, 1), nil},
		{"identity without baseline+cutoff", TargetVersion, withBackupIdentity(RolloutState{}, 41), []string{KeyBackupSHA256}},
		{"identity without baseline+cutoff below target", 38, withBackupIdentity(RolloutState{}, 38), nil},
		{"not_required below target", 39, withNotRequired(RolloutState{}), []string{KeyBackupNotRequiredAt}},
		{"unknown postflight status", TargetVersion, unknownStatus, []string{KeyPostflightStatus}},
		{"status without detail", TargetVersion, statusWithoutDetail, []string{KeyPostflightDetail}},
		{"detail without status", TargetVersion, detailWithoutStatus, []string{KeyPostflightStatus}},
		{"valid status on row-2 shape", 35, withPostflight(withBackupIdentity(withBaselineCutoff(RolloutState{}, 0, 0, 1), 35), PostflightPassed), []string{KeyPostflightStatus}},
		{"valid status on row-1 shape", 36, withPostflight(RolloutState{}, PostflightPassed), []string{KeyPostflightStatus}},
		{"valid status on row-3 shape", TargetVersion, withPostflight(RolloutState{}, PostflightPassed), []string{KeyPostflightStatus}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := ClassifyRollout(testCase.schema, testCase.state)
			if err == nil {
				t.Fatalf("want Corrupt")
			}
			if !errors.Is(err, ErrRolloutStateCorrupt) {
				t.Fatalf("err = %v, want ErrRolloutStateCorrupt", err)
			}
			for _, needle := range testCase.contains {
				if !strings.Contains(err.Error(), needle) {
					t.Fatalf("err = %v, want it to name %s", err, needle)
				}
			}
		})
	}
}

func TestIsRolloutCompletePropagatesErrorsUnchanged(t *testing.T) {
	complete, err := IsRolloutComplete(TargetVersion, withPostflight(withNotRequired(withBaselineCutoff(RolloutState{}, 0, 0, 1)), PostflightPassed))
	if err != nil || !complete {
		t.Fatalf("complete=%v err=%v, want true,nil", complete, err)
	}
	complete, err = IsRolloutComplete(33, RolloutState{})
	if err != nil || complete {
		t.Fatalf("complete=%v err=%v, want false,nil on row 1", complete, err)
	}
	if _, err := IsRolloutComplete(TargetVersion+1, RolloutState{}); !errors.Is(err, ErrFutureSchema) {
		t.Fatalf("err = %v, want ErrFutureSchema propagated", err)
	}
	if _, err := IsRolloutComplete(10, RolloutState{}); !errors.Is(err, ErrUnsupportedSourceSchema) {
		t.Fatalf("err = %v, want ErrUnsupportedSourceSchema propagated", err)
	}
	corruptState := RolloutState{
		BaselinePresent: true,
		BaselineValid:   true,
	}
	if _, err := IsRolloutComplete(41, corruptState); !errors.Is(err, ErrRolloutStateCorrupt) {
		t.Fatalf("err = %v, want Corrupt propagated", err)
	}
}

func TestComparePostflightExactContract(t *testing.T) {
	ok, field, delta := ComparePostflight(
		IdentityBaseline{JobsCompletedWithoutResultIdentity: 21, ActivationsWithoutContent: 12},
		IdentityBaseline{JobsCompletedWithoutResultIdentity: 21, ActivationsWithoutContent: 12},
	)
	if !ok || field != "" || delta != 0 {
		t.Fatalf("(ok=%v field=%q delta=%d), want (true \"\" 0)", ok, field, delta)
	}
	ok, field, delta = ComparePostflight(
		IdentityBaseline{JobsCompletedWithoutResultIdentity: 21, ActivationsWithoutContent: 12},
		IdentityBaseline{JobsCompletedWithoutResultIdentity: 24, ActivationsWithoutContent: 12},
	)
	if ok || field != "JobsCompletedWithoutResultIdentity" || delta != 3 {
		t.Fatalf("(ok=%v field=%q delta=%d), want regression with exact delta 3", ok, field, delta)
	}
	ok, field, delta = ComparePostflight(
		IdentityBaseline{ActivationsWithoutContent: 12},
		IdentityBaseline{ActivationsWithoutContent: 15},
	)
	if ok || field != "ActivationsWithoutContent" || delta != 3 {
		t.Fatalf("(ok=%v field=%q delta=%d), want activation regression", ok, field, delta)
	}
}

func TestParseHelpersPinDurableFormats(t *testing.T) {
	if _, ok := ParseBaseline("jobs=3;activations=7"); !ok {
		t.Fatal("canonical baseline must parse")
	}
	if _, ok := ParseBaseline("jobs=3"); ok {
		t.Fatal("partial baseline must not parse")
	}
	if _, ok := ParseBaseline("jobs=-1;activations=0"); ok {
		t.Fatal("negative counters must not parse")
	}
	if _, ok := ParseBaseline("jobs=18446744073709551615;activations=0"); ok {
		t.Fatal("counters larger than int must not parse")
	}
	if _, ok := ParseNonNegativeDecimal("12"); !ok {
		t.Fatal("decimal must parse")
	}
	for _, raw := range []string{"", "-1", "1e3", "abc"} {
		if _, ok := ParseNonNegativeDecimal(raw); ok {
			t.Fatalf("%q must not parse as non-negative decimal", raw)
		}
	}
	if _, ok := ParseSHA256Hex(strings.Repeat("AB", 32)); ok {
		t.Fatal("uppercase digest must not parse")
	}
	if _, ok := ParseSHA256Hex(testSHA256[:63]); ok {
		t.Fatal("short digest must not parse")
	}
	if value, ok := ParseBackupSourceVersion("41"); !ok || value != 41 {
		t.Fatalf("source_version bound check failed: %d %v", value, ok)
	}
	if value, ok := ParseBackupSourceVersion("46"); !ok || value != 46 {
		t.Fatalf("source_version at MaxSourceVersion must parse: %d %v", value, ok)
	}
	if value, ok := ParseBackupSourceVersion("47"); !ok || value != 47 {
		t.Fatalf("source_version at TargetVersion must parse: %d %v", value, ok)
	}
	for _, raw := range []string{"0", "48", "-3", "x"} {
		if _, ok := ParseBackupSourceVersion(raw); ok {
			t.Fatalf("%q must not parse as source version", raw)
		}
	}
	if _, ok := ParseRFC3339(testVerifiedAt.Format(time.RFC3339)); !ok {
		t.Fatal("RFC 3339 must parse")
	}
	if _, ok := ParseRFC3339("20260821T143000Z"); ok {
		t.Fatal("basic-format timestamp must not parse")
	}
	if err := quick.Check(func(jobs uint8, acts uint8) bool {
		parsed, ok := ParseBaseline(FormatBaseline(IdentityBaseline{int(jobs), int(acts)}))
		return ok && parsed.JobsCompletedWithoutResultIdentity == int(jobs) && parsed.ActivationsWithoutContent == int(acts)
	}, nil); err != nil {
		t.Fatalf("format/parse round trip: %v", err)
	}
}
