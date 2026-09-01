package blockkit

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestDescribeAndPreviewUseLoadedTemplates(t *testing.T) {
	engine, err := New(os.DirFS("testdata"))
	if err != nil {
		t.Fatal(err)
	}

	description, ok := engine.Describe("confirmation.prompt")
	if !ok || description.Name != "confirmation.prompt" || description.Surface != "message" {
		t.Fatalf("Describe() = %#v, %t", description, ok)
	}
	if _, ok := engine.Describe("missing"); ok {
		t.Fatal("Describe() found a missing template")
	}

	messagePreview, err := engine.Preview("confirmation.prompt", true)
	if err != nil {
		t.Fatal(err)
	}
	var message map[string]any
	if err := json.Unmarshal(messagePreview.JSON, &message); err != nil {
		t.Fatalf("message preview is not JSON: %v", err)
	}
	if _, ok := message["blocks"]; !ok {
		t.Fatalf("message preview = %s, want blocks", messagePreview.JSON)
	}

	modalPreview, err := engine.Preview("modal.settings", true)
	if err != nil {
		t.Fatal(err)
	}
	var modal map[string]any
	if err := json.Unmarshal(modalPreview.JSON, &modal); err != nil {
		t.Fatalf("modal preview is not JSON: %v", err)
	}
	for _, field := range []string{"type", "title", "blocks"} {
		if _, ok := modal[field]; !ok {
			t.Fatalf("modal preview = %s, missing %q", modalPreview.JSON, field)
		}
	}
	if modal["type"] != "modal" {
		t.Fatalf("modal type = %#v", modal["type"])
	}
}

func TestPreviewHonorsOptionalVariantAndListsUnknownNames(t *testing.T) {
	engine, err := New(os.DirFS("testdata"))
	if err != nil {
		t.Fatal(err)
	}
	minimal, err := engine.Preview("confirmation.prompt", false)
	if err != nil {
		t.Fatal(err)
	}
	maximal, err := engine.Preview("confirmation.prompt", true)
	if err != nil {
		t.Fatal(err)
	}
	if string(minimal.JSON) == string(maximal.JSON) {
		t.Fatal("minimal and maximal previews are identical")
	}

	_, err = engine.Preview("missing", true)
	if err == nil || !strings.Contains(err.Error(), "valid templates:") || !strings.Contains(err.Error(), "confirmation.prompt") {
		t.Fatalf("unknown Preview() error = %v", err)
	}
}
