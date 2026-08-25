//go:build unix

package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"testing"

	"github.com/Dauno/slack-local-agent/internal/cli"
	"github.com/Dauno/slack-local-agent/internal/config"
	"github.com/Dauno/slack-local-agent/internal/domain"
)

func executeUpgradeCLI(t *testing.T, h *upgradeHarness, args []string, stdin string) (int, string, string) {
	t.Helper()
	var output, stderr bytes.Buffer
	root, err := cli.NewRoot(h.application, cli.Streams{In: strings.NewReader(stdin), Out: &output, Err: &stderr})
	if err != nil {
		t.Fatal(err)
	}
	code := cli.Execute(context.Background(), root, args, &stderr)
	return code, output.String(), stderr.String()
}

const (
	freshSummaryPrefix      = "will migrate v33 to v43; backup will be written under "
	databaseAlreadyComplete = "database already at schema v43; nothing to do"
	upgradeCancelled        = "Actualizacion cancelada."
	resumeNeededSummary     = "database is at v43 with an incomplete rollout (postflight not yet passed); will re-run postflight"
)
const promptMigrate = "Aplicar la migracion de schema v33 a v43 sobre "
const promptV33Stopped = "El proceso v33 desplegado no participa en el protocolo de bloqueo de este comando. Confirme que ese proceso esta detenido antes de continuar."
const terminalRangeText = "is outside the range local-agent db upgrade accepts ([33, 43]); this file cannot be upgraded or opened by this binary"

func TestDBUpgradeFreshRunPromptsTwiceThenCompletes(t *testing.T) {
	h := newUpgradeHarness(t)
	rowOneFixture(t, h)

	code, out, stderr := executeUpgradeCLI(t, h, []string{"db", "upgrade"}, "y\ny\n")
	if code != 0 {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, out, stderr)
	}
	for _, want := range []string{freshSummaryPrefix, promptMigrate, promptV33Stopped, "backup verified: ", "jobs completed without result identity: baseline 0, post 0", "run local-agent jobs quarantine-legacy-identity"} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout = %q, want it to contain %q", out, want)
		}
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want empty", stderr)
	}
	secondCode, secondOut, _ := executeUpgradeCLI(t, h, []string{"db", "upgrade"}, "")
	if secondCode != 0 || !strings.Contains(secondOut, databaseAlreadyComplete) {
		t.Fatalf("second run exit=%d out=%q", secondCode, secondOut)
	}
}

func TestDBUpgradeDeclineCancelsWithoutLockOrMutation(t *testing.T) {
	h := newUpgradeHarness(t)
	rowOneFixture(t, h)

	code, out, _ := executeUpgradeCLI(t, h, []string{"db", "upgrade"}, "n\n")
	if code != 0 || !strings.Contains(out, upgradeCancelled) {
		t.Fatalf("exit=%d out=%q, want cancellation with exit 0", code, out)
	}
	if h.lockerLog.count("lock:") != 0 {
		t.Fatalf("declined run took the lock: %q", h.lockerLog.joined())
	}
	if got := queryUserVersion(t, h.paths.DatabaseFile); got != 33 {
		t.Fatalf("user_version = %d after decline", got)
	}
}

func TestDBUpgradeYesSkipsEveryPrompt(t *testing.T) {
	h := newUpgradeHarness(t)
	rowOneFixture(t, h)

	code, out, stderr := executeUpgradeCLI(t, h, []string{"db", "upgrade", "--yes"}, "")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr)
	}
	if strings.Contains(out, "[y/N]") {
		t.Fatalf("--yes still prompted: %q", out)
	}
}

func TestDBUpgradeAlreadyCompleteTakesNoLockAndShowsNoPrompt(t *testing.T) {
	h := newUpgradeHarness(t)
	code, out, _ := executeUpgradeCLI(t, h, []string{"db", "upgrade"}, "")
	if code != 0 || out != databaseAlreadyComplete+"\n" {
		t.Fatalf("exit=%d out=%q, want exactly %q", code, out, databaseAlreadyComplete)
	}
	if h.lockerLog.count("lock:") != 0 {
		t.Fatalf("no-op run took the lock: %q", h.lockerLog.joined())
	}
}

func TestDBUpgradeResumeNeededAsksOnce(t *testing.T) {
	h := newUpgradeHarness(t)
	rowFourFixture(t, h, nil)

	code, out, _ := executeUpgradeCLI(t, h, []string{"db", "upgrade"}, "n\n")
	if code != 0 || !strings.Contains(out, resumeNeededSummary) || !strings.Contains(out, upgradeCancelled) {
		t.Fatalf("declined resume exit=%d out=%q", code, out)
	}
	h.resetLogs()
	code, out, _ = executeUpgradeCLI(t, h, []string{"db", "upgrade"}, "y\n")
	if code != 0 || !strings.Contains(out, "activations without content: baseline 0, post 0") {
		t.Fatalf("accepted resume exit=%d out=%q", code, out)
	}
	if strings.Count(out, "[y/N]") != 1 {
		t.Fatalf("resume prompts = %d in %q, want one", strings.Count(out, "[y/N]"), out)
	}
}

func TestDBUpgradeOutOfRangeExitsTwoWithoutPrompts(t *testing.T) {
	cases := []struct {
		version int
		wantErr string
	}{
		{32, "database schema v32 " + terminalRangeText},
		{20, "database schema v20 " + terminalRangeText},
		{14, "database schema v14 " + terminalRangeText},
		{0, "database schema v0 " + terminalRangeText},
		{44, "found v44"},
	}
	for _, testCase := range cases {
		t.Run("v"+string(rune('0'+testCase.version)), func(t *testing.T) {
			h := newUpgradeHarness(t)
			replaceFixture(t, h.paths.DatabaseFile, testCase.version, nil)

			code, out, stderr := executeUpgradeCLI(t, h, []string{"db", "upgrade"}, "y\ny\n")
			if code != 2 {
				t.Fatalf("exit=%d stderr=%q, want 2", code, stderr)
			}
			if !strings.Contains(stderr, testCase.wantErr) {
				t.Fatalf("stderr = %q, want %q", stderr, testCase.wantErr)
			}
			if strings.Contains(out, "[y/N]") || strings.Contains(stderr, "run local-agent db upgrade") {
				t.Fatalf("out-of-range run prompted or advised: out=%q stderr=%q", out, stderr)
			}
			if h.lockerLog.count("lock:") != 0 {
				t.Fatalf("refusal took the lock: %q", h.lockerLog.joined())
			}
		})
	}
}

func TestRollbackCheckClearAndBlocked(t *testing.T) {
	t.Run("clear", func(t *testing.T) {
		h := newUpgradeHarness(t)
		code, out, stderr := executeUpgradeCLI(t, h, []string{"db", "rollback-check"}, "")
		const clearText = "rollback drain clear: 0 sessions have a pending discovery marker; safe to run a schema-v41-compatible binary at or before 3cfe091"
		if code != 0 || out != clearText+"\n" || stderr != "" {
			t.Fatalf("exit=%d out=%q stderr=%q, want the exact clear text with exit 0", code, out, stderr)
		}
	})
	t.Run("blocked", func(t *testing.T) {
		h := newUpgradeHarness(t)
		plain, err := sqlOpenPlain(h.paths.DatabaseFile)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = plain.Close() }()
		for _, session := range []string{"sess-a", "sess-b"} {
			if _, err := plain.Exec(`INSERT INTO adk_context_summary_jobs
				(session_identity, target_ordinal, status, next_attempt, created_at, updated_at)
				VALUES (?, ?, 'pending', 1, 1, 1)`,
				session, domain.SummaryDiscoveryTargetFloor+1); err != nil {
				t.Fatal(err)
			}
		}
		code, out, _ := executeUpgradeCLI(t, h, []string{"db", "rollback-check"}, "")
		const blockedPrefix = "rollback blocked: 2 sessions have a pending discovery marker; let the current binary drain them or cancel them explicitly before rolling back to a binary at or before 3cfe091"
		if code != 1 {
			t.Fatalf("exit=%d out=%q, want 1", code, out)
		}
		if !strings.HasPrefix(out, blockedPrefix+"\n") || !strings.Contains(out, "sess-a\n") || !strings.Contains(out, "sess-b\n") {
			t.Fatalf("out = %q, want the exact blocked text plus both identities", out)
		}
	})
}

// TestNoBadAdviceOnUnsupportedSchemas pins FIND-179 across the four ordinary
// commands this checkpoint owns: schema outside [33, 41] exits 1 with the
// terminal message and never recommends db upgrade.
func TestNoBadAdviceOnUnsupportedSchemas(t *testing.T) {
	h := newUpgradeHarness(t)
	dbPath := h.paths.DatabaseFile
	before := replaceFixture(t, dbPath, 32, nil)

	t.Run("run", func(t *testing.T) {
		t.Setenv("SLACK_BOT_TOKEN", "xoxb-seam-token")
		t.Setenv("SLACK_APP_TOKEN", "xapp-seam-token")
		t.Setenv("DEEPSEEK_API_KEY", "seam-model-key")
		code, _, stderr := executeUpgradeCLI(t, h, []string{"run"}, "")
		assertTerminalRefusal(t, code, stderr)
	})

	t.Run("init existing database", func(t *testing.T) {
		const wizardStdin = "\n\n\nxoxb-token\nxapp-token\nU12345678\n\n\n\n\nmodel-key\ny\n"
		code, _, stderr := executeUpgradeCLI(t, h, []string{"init"}, wizardStdin)
		assertTerminalRefusal(t, code, stderr)
	})

	t.Run("jobs reconcile", func(t *testing.T) {
		_, err := h.application.ReconcileJob(context.Background(), "missing-job", 0)
		if err == nil {
			t.Fatal("reconcile on an unsupported schema must fail")
		}
		assertTerminalRefusalText(t, err.Error())
	})

	t.Run("knowledge rebuild-index", func(t *testing.T) {
		cfg, err := config.Load(h.paths.ConfigFile)
		if err != nil {
			t.Fatal(err)
		}
		cfg.Orchestration.Knowledge.Enabled = true
		if err := config.Save(h.paths.ConfigFile, cfg); err != nil {
			t.Fatal(err)
		}
		code, _, stderr := executeUpgradeCLI(t, h, []string{"knowledge", "rebuild-index"}, "")
		assertTerminalRefusal(t, code, stderr)
	})

	if digestOf(t, dbPath) != before {
		t.Fatal("the v32 fixture changed during the no-bad-advice gates")
	}
}

func assertTerminalRefusal(t *testing.T, code int, stderr string) {
	t.Helper()
	if code != 1 {
		t.Fatalf("exit=%d stderr=%q, want 1", code, stderr)
	}
	assertTerminalRefusalText(t, stderr)
}

func assertTerminalRefusalText(t *testing.T, text string) {
	t.Helper()
	if !strings.Contains(text, terminalRangeText) {
		t.Fatalf("text = %q, want the terminal message", text)
	}
	if strings.Contains(text, "run local-agent db upgrade") {
		t.Fatalf("text = %q, must never recommend db upgrade", text)
	}
}

func digestOf(t *testing.T, dbPath string) string {
	t.Helper()
	data, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// TestDBUpgradeBoundaryMatrix drives the five-way boundary matrix over the
// static version edges: classification, prompts, lock, backup, mapping.
func TestDBUpgradeBoundaryMatrix(t *testing.T) {
	cases := []struct {
		name        string
		build       func(t *testing.T, h *upgradeHarness)
		stdin       string
		wantExit    int
		wantKindOut string
		prompts     int
		lock        bool
		backup      bool
	}{
		{
			name: "v32 below floor",
			build: func(t *testing.T, h *upgradeHarness) {
				replaceFixture(t, h.paths.DatabaseFile, 32, nil)
			},
			stdin:    "",
			wantExit: 2,
			prompts:  0, lock: false, backup: false,
		},
		{
			name: "v33 floor",
			build: func(t *testing.T, h *upgradeHarness) {
				replaceFixture(t, h.paths.DatabaseFile, 33, nil)
			},
			stdin:       "y\ny\n",
			wantExit:    0,
			wantKindOut: freshSummaryPrefix,
			prompts:     2, lock: true, backup: true,
		},
		{
			name: "v40 ceiling",
			build: func(t *testing.T, h *upgradeHarness) {
				replaceFixture(t, h.paths.DatabaseFile, 40, nil)
			},
			stdin:       "y\ny\n",
			wantExit:    0,
			wantKindOut: "will migrate v40 to v43; backup will be written under ",
			prompts:     2, lock: true, backup: true,
		},
		{
			name: "v43 adoption",
			build: func(t *testing.T, h *upgradeHarness) {
				replaceFixture(t, h.paths.DatabaseFile, 43, nil)
			},
			stdin:       "y\ny\n",
			wantExit:    0,
			wantKindOut: "database is already at v43 but was never rolled out through local-agent db upgrade; will record a baseline and cutoff now and back up first, under ",
			prompts:     2, lock: true, backup: true,
		},
		{
			name:        "v43 complete",
			build:       func(*testing.T, *upgradeHarness) {},
			stdin:       "",
			wantExit:    0,
			wantKindOut: databaseAlreadyComplete,
			prompts:     0, lock: false, backup: false,
		},
		{
			name: "v44 future",
			build: func(t *testing.T, h *upgradeHarness) {
				replaceFixture(t, h.paths.DatabaseFile, 44, nil)
			},
			stdin:    "",
			wantExit: 2,
			prompts:  0, lock: false, backup: false,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			h := newUpgradeHarness(t)
			testCase.build(t, h)

			code, out, stderr := executeUpgradeCLI(t, h, []string{"db", "upgrade"}, testCase.stdin)
			if code != testCase.wantExit {
				t.Fatalf("exit=%d stderr=%q, want %d", code, stderr, testCase.wantExit)
			}
			if got := strings.Count(out, "[y/N]"); got != testCase.prompts {
				t.Fatalf("prompts=%d out=%q, want %d", got, out, testCase.prompts)
			}
			if testCase.wantKindOut != "" && !strings.Contains(out, testCase.wantKindOut) {
				t.Fatalf("out = %q, want summary %q", out, testCase.wantKindOut)
			}
			lockTaken := h.lockerLog.count("lock:") > 0
			if lockTaken != testCase.lock {
				t.Fatalf("lock taken=%v events=%q, want %v", lockTaken, h.lockerLog.joined(), testCase.lock)
			}
			backupMade := h.backupLog.count("backupper.into") > 0
			if backupMade != testCase.backup {
				t.Fatalf("backup made=%v, want %v", backupMade, testCase.backup)
			}
		})
	}
}
