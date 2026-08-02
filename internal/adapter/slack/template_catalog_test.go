package slack

import (
	"strings"
	"testing"
	"testing/fstest"
)

func TestEmbeddedTemplateCatalogLoadsExactlyRequiredNames(t *testing.T) {
	catalog, err := LoadTemplateCatalog()
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	want := []string{"agent_preview", "builder_modal", "confirmation_message", "onboarding_message"}
	got := catalog.Names()
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("catalog names = %v, want %v", got, want)
	}
	for _, name := range want {
		info, ok := catalog.Info(name)
		if !ok {
			t.Fatalf("catalog has no info for %q", name)
		}
		if info.Name != name || info.SchemaVersion != templateSchemaVersion {
			t.Fatalf("catalog info for %q = %#v", name, info)
		}
	}
}

func TestTemplateCatalogRejectsInvalidDocuments(t *testing.T) {
	tests := []struct {
		name string
		edit func(map[string][]byte)
	}{
		{
			name: "malformed JSON",
			edit: func(files map[string][]byte) { files["templates/builder_modal.json"] = []byte("{") },
		},
		{
			name: "duplicate keys",
			edit: func(files map[string][]byte) {
				files["templates/builder_modal.json"] = []byte(strings.Replace(string(files["templates/builder_modal.json"]), `"name": "builder_modal",`, `"name": "builder_modal", "name": "builder_modal",`, 1))
			},
		},
		{
			name: "trailing document",
			edit: func(files map[string][]byte) {
				files["templates/builder_modal.json"] = append(files["templates/builder_modal.json"], []byte("\n{}")...)
			},
		},
		{
			name: "unknown root field",
			edit: func(files map[string][]byte) {
				files["templates/builder_modal.json"] = []byte(strings.Replace(string(files["templates/builder_modal.json"]), `"surface": "modal",`, `"surface": "modal", "unexpected": true,`, 1))
			},
		},
		{
			name: "unknown payload field",
			edit: func(files map[string][]byte) {
				files["templates/builder_modal.json"] = []byte(strings.Replace(string(files["templates/builder_modal.json"]), `"type": "modal",`, `"type": "modal", "unexpected": true,`, 1))
			},
		},
		{
			name: "missing required template",
			edit: func(files map[string][]byte) { delete(files, "templates/onboarding_message.json") },
		},
		{
			name: "unknown template filename",
			edit: func(files map[string][]byte) {
				data := files["templates/onboarding_message.json"]
				delete(files, "templates/onboarding_message.json")
				files["templates/unknown.json"] = data
			},
		},
		{
			name: "unsupported schema version",
			edit: func(files map[string][]byte) {
				files["templates/builder_modal.json"] = []byte(strings.Replace(string(files["templates/builder_modal.json"]), `"schema_version": 1`, `"schema_version": 2`, 1))
			},
		},
		{
			name: "filename and name mismatch",
			edit: func(files map[string][]byte) {
				files["templates/builder_modal.json"] = []byte(strings.Replace(string(files["templates/builder_modal.json"]), `"name": "builder_modal"`, `"name": "agent_preview"`, 1))
			},
		},
		{
			name: "unknown nested field",
			edit: func(files map[string][]byte) {
				files["templates/builder_modal.json"] = []byte(strings.Replace(string(files["templates/builder_modal.json"]), `"emoji": false`, `"emoji": false, "unexpected": true`, 1))
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			files := embeddedTemplateFiles(t)
			test.edit(files)
			if _, err := LoadTemplateCatalogFromFS(templateMapFS(files)); err == nil {
				t.Fatal("invalid catalog was accepted")
			}
		})
	}
}

func embeddedTemplateFiles(t *testing.T) map[string][]byte {
	t.Helper()
	files := make(map[string][]byte, len(requiredTemplateNames))
	for _, name := range requiredTemplateNames {
		path := "templates/" + name + ".json"
		data, err := embeddedTemplates.ReadFile(path)
		if err != nil {
			t.Fatalf("read embedded fixture %q: %v", path, err)
		}
		files[path] = append([]byte(nil), data...)
	}
	return files
}

func templateMapFS(files map[string][]byte) fstest.MapFS {
	result := make(fstest.MapFS, len(files))
	for path, data := range files {
		result[path] = &fstest.MapFile{Data: append([]byte(nil), data...)}
	}
	return result
}
