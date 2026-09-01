package slack

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dauno/slack-local-agent/internal/blockkit"
)

func TestConfirmationPromptLayoutFingerprintTracksCanonicalLayout(t *testing.T) {
	raw, err := fs.ReadFile(viewsFS, "views/templates/confirmation/prompt.json")
	if err != nil {
		t.Fatal(err)
	}

	base := confirmationFingerprintEngine(t, raw)
	baseDigest := confirmationFingerprint(t, base)

	labelChanged := strings.Replace(string(raw), `"text": "Confirmation required"`, `"text": "Confirmation needed"`, 1)
	if labelChanged == string(raw) {
		t.Fatal("confirmation label was not found")
	}
	if digest := confirmationFingerprint(t, confirmationFingerprintEngine(t, []byte(labelChanged))); digest == baseDigest {
		t.Fatal("changing a visible label did not change the layout fingerprint")
	}

	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	layout, ok := document["layout"].([]any)
	if !ok || len(layout) < 2 {
		t.Fatal("confirmation template has fewer than two layout blocks")
	}
	layout[0], layout[1] = layout[1], layout[0]
	reordered, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if digest := confirmationFingerprint(t, confirmationFingerprintEngine(t, reordered)); digest == baseDigest {
		t.Fatal("changing layout block order did not change the layout fingerprint")
	}

	var indented bytes.Buffer
	if err := json.Indent(&indented, raw, "", "  "); err != nil {
		t.Fatal(err)
	}
	if digest := confirmationFingerprint(t, confirmationFingerprintEngine(t, indented.Bytes())); digest != baseDigest {
		t.Fatal("reindenting the template changed the layout fingerprint")
	}
}

func confirmationFingerprintEngine(t *testing.T, content []byte) *blockkit.Engine {
	t.Helper()
	root := t.TempDir()
	templateDir := filepath.Join(root, "templates", "confirmation")
	if err := os.MkdirAll(templateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(templateDir, "prompt.json"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	engine, err := blockkit.New(os.DirFS(root))
	if err != nil {
		t.Fatalf("load temporary confirmation template: %v", err)
	}
	if err := engine.Register(confirmationPromptView{}); err != nil {
		t.Fatalf("register temporary confirmation template: %v", err)
	}
	return engine
}

func confirmationFingerprint(t *testing.T, engine *blockkit.Engine) string {
	t.Helper()
	digest, ok := engine.LayoutSHA256(confirmationPromptTemplateName)
	if !ok || digest == "" {
		t.Fatalf("confirmation layout fingerprint = %q, %t", digest, ok)
	}
	return digest
}
