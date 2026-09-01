package toolfactory

import (
	"testing"

	"github.com/Dauno/slack-local-agent/internal/tooldef"
)

func TestSchemaFromDeclarationHandlesEmptyAndUnencodableSchemas(t *testing.T) {
	if schema, err := schemaFromDeclaration(nil); schema != nil || err != nil {
		t.Fatalf("empty schema = %#v, err=%v", schema, err)
	}
	if _, err := schemaFromDeclaration(tooldef.Schema{"invalid": func() {}}); err == nil {
		t.Fatal("unencodable schema was accepted")
	}
}
