package memoryprojector

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

type knowledgeTestClock struct{ now time.Time }

func (c *knowledgeTestClock) Now() time.Time { return c.now }

func knowledgeTestClaim(subject, value, scopeID string, scopeKind domain.KnowledgeScopeKind) domain.KnowledgeClaim {
	return domain.KnowledgeClaim{
		ID:          domain.KnowledgeClaimID("kclaim_" + strings.Repeat("a", 24)),
		Subject:     subject,
		Predicate:   domain.KnowledgePredicateRunsOn,
		Value:       domain.KnowledgeValue{Kind: domain.KnowledgeValueString, Text: value},
		ScopeKind:   scopeKind,
		ScopeID:     scopeID,
		SourceClass: domain.KnowledgeSourceHuman,
		SourceRef:   "slack-human:evt-1",
		AuthorID:    "U00000001",
		Status:      domain.KnowledgeClaimAsserted,
		Revision:    1,
		CreatedAt:   time.Unix(1_700_000_000, 0).UTC(),
		UpdatedAt:   time.Unix(1_700_000_000, 0).UTC(),
	}
}

func knowledgeTestPreference() domain.KnowledgePreference {
	return domain.KnowledgePreference{
		ID:        1,
		OwnerKey:  "slack:T00000001:dm:D00000001:U00000001",
		Key:       "language",
		Value:     domain.KnowledgeValue{Kind: domain.KnowledgeValueString, Text: "Spanish"},
		Status:    domain.KnowledgePreferenceActive,
		SourceRef: "slack-human:evt-2",
		Revision:  1,
	}
}

func knowledgeTestDocument() domain.KnowledgeDocument {
	return domain.KnowledgeDocument{
		ID:            domain.KnowledgeDocumentID("kdoc_" + strings.Repeat("b", 24)),
		Subject:       "api",
		ScopeKind:     domain.KnowledgeScopeProject,
		ScopeID:       "workspace",
		ContentDigest: strings.Repeat("c", 64),
		ContentHandle: "result:doc-1",
		Provenance:    domain.KnowledgeProvenanceCurated,
		Status:        domain.KnowledgeDocumentActive,
		Revision:      1,
	}
}

func knowledgeTestEvidence() port.KnowledgeEvidenceRef {
	return port.KnowledgeEvidenceRef{
		ClaimID:        domain.KnowledgeClaimID("kclaim_" + strings.Repeat("a", 24)),
		RevisionNumber: 1,
		Kind:           domain.KnowledgeEvidenceSource,
		ExchangeTS:     "1723543200.123456",
	}
}

func knowledgeTestSnapshot() port.ProjectionSnapshot {
	return port.ProjectionSnapshot{
		Knowledge: port.KnowledgeSnapshot{
			Claims:      []domain.KnowledgeClaim{knowledgeTestClaim("database", "PostgreSQL 17", "workspace", domain.KnowledgeScopeProject)},
			Preferences: []domain.KnowledgePreference{knowledgeTestPreference()},
			Documents:   []domain.KnowledgeDocument{knowledgeTestDocument()},
			Evidence:    []port.KnowledgeEvidenceRef{knowledgeTestEvidence()},
		},
	}
}

func projectKnowledgeBundle(t *testing.T, p *Projector, snapshot port.ProjectionSnapshot) string {
	t.Helper()
	dir := t.TempDir()
	bundle := filepath.Join(dir, "memory")
	if err := p.Project(t.Context(), &stubProjectionReader{snapshot: snapshot}, bundle); err != nil {
		t.Fatal(err)
	}
	return bundle
}

func readBundleFile(t *testing.T, bundle, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(bundle, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestKnowledgeProjectionRendersAllClasses(t *testing.T) {
	p := New()
	bundle := projectKnowledgeBundle(t, p, knowledgeTestSnapshot())
	for _, name := range []string{"knowledge/index.md", "knowledge/claims.md", "knowledge/preferences.md", "knowledge/documents.md", "knowledge/evidence.md"} {
		if _, err := os.Stat(filepath.Join(bundle, name)); err != nil {
			t.Fatalf("missing %s: %v", name, err)
		}
	}
	claims := readBundleFile(t, bundle, "knowledge/claims.md")
	for _, want := range []string{"database", "runs_on", "PostgreSQL 17", "scope: project:workspace", "human", "slack-human:evt-1", "asserted", "revision: 1"} {
		if !strings.Contains(claims, want) {
			t.Fatalf("claims.md missing %q:\n%s", want, claims)
		}
	}
	preferences := readBundleFile(t, bundle, "knowledge/preferences.md")
	if !strings.Contains(preferences, "language") || !strings.Contains(preferences, "Spanish") || !strings.Contains(preferences, "active") {
		t.Fatalf("preferences.md incomplete:\n%s", preferences)
	}
	documents := readBundleFile(t, bundle, "knowledge/documents.md")
	if !strings.Contains(documents, "api") || !strings.Contains(documents, strings.Repeat("c", 64)) || !strings.Contains(documents, "curated") {
		t.Fatalf("documents.md incomplete:\n%s", documents)
	}
	evidence := readBundleFile(t, bundle, "knowledge/evidence.md")
	if !strings.Contains(evidence, "source") || !strings.Contains(evidence, "1723543200.123456") {
		t.Fatalf("evidence.md incomplete:\n%s", evidence)
	}
	rootIndex := readBundleFile(t, bundle, "index.md")
	if !strings.Contains(rootIndex, "knowledge/index.md") {
		t.Fatalf("root index missing knowledge link:\n%s", rootIndex)
	}
}

func TestKnowledgeProjectionHidesPrivacyIdentity(t *testing.T) {
	p := New()
	userClaim := knowledgeTestClaim("api", "value", "slack:T00000001:dm:D00000001:U00000001", domain.KnowledgeScopeUser)
	userClaim.ID = domain.KnowledgeClaimID("kclaim_" + strings.Repeat("a", 24))
	projectClaim := knowledgeTestClaim("database", "PostgreSQL 17", "workspace", domain.KnowledgeScopeProject)
	projectClaim.ID = domain.KnowledgeClaimID("kclaim_" + strings.Repeat("b", 24))
	snapshot := knowledgeTestSnapshot()
	snapshot.Knowledge.Claims = []domain.KnowledgeClaim{userClaim, projectClaim}
	bundle := projectKnowledgeBundle(t, p, snapshot)
	text := readBundleFile(t, bundle, "knowledge/claims.md")
	for _, forbidden := range []string{"U00000001", "slack:T00000001", "T00000001"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("claims.md leaks identity %q:\n%s", forbidden, text)
		}
	}
	preferences := readBundleFile(t, bundle, "knowledge/preferences.md")
	if strings.Contains(preferences, "slack:T00000001") || strings.Contains(preferences, "evt-2") {
		t.Fatalf("preferences.md leaks owner or source identity:\n%s", preferences)
	}
	documents := readBundleFile(t, bundle, "knowledge/documents.md")
	if strings.Contains(documents, "result:doc-1") {
		t.Fatalf("documents.md exposes content_handle:\n%s", documents)
	}
	evidence := readBundleFile(t, bundle, "knowledge/evidence.md")
	if strings.Contains(evidence, "dm:D") || strings.Contains(evidence, "U00000001") {
		t.Fatalf("evidence.md leaks conversation or author identity:\n%s", evidence)
	}
	// Project and workstream identities are useful and allowed.
	if !strings.Contains(readBundleFile(t, bundle, "knowledge/claims.md"), "project:workspace") {
		t.Fatal("project identity must be visible where useful")
	}
}

func TestKnowledgeProjectionEscapesUntrustedContent(t *testing.T) {
	p := New()
	claim := knowledgeTestClaim("api#anchor [docs](https://example.com) `code`", "a\n---\ntitle: injected\n# heading [x](y) `z`", "workspace", domain.KnowledgeScopeProject)
	preference := knowledgeTestPreference()
	preference.Key = "k# [x](y) `z`"
	preference.Value = domain.KnowledgeValue{Kind: domain.KnowledgeValueString, Text: "v\n---\nowned: true"}
	snapshot := port.ProjectionSnapshot{Knowledge: port.KnowledgeSnapshot{
		Claims: []domain.KnowledgeClaim{claim}, Preferences: []domain.KnowledgePreference{preference},
	}}
	bundle := projectKnowledgeBundle(t, p, snapshot)
	for _, name := range []string{"knowledge/claims.md", "knowledge/preferences.md"} {
		text := readBundleFile(t, bundle, name)
		if strings.Contains(text, "\n---\n") {
			t.Fatalf("%s contains injected frontmatter separator:\n%s", name, text)
		}
		if strings.Contains(text, "\nowned:") || strings.Contains(text, "\ntitle:") {
			t.Fatalf("%s contains a frontmatter key at line start:\n%s", name, text)
		}
		if strings.Contains(text, "\n# ") {
			t.Fatalf("%s contains injected heading:\n%s", name, text)
		}
	}
	claims := readBundleFile(t, bundle, "knowledge/claims.md")
	for _, escaped := range []string{`\[`, `\]`, `\(`, `\)`, "\\`code\\`", "\\#"} {
		if !strings.Contains(claims, escaped) {
			t.Fatalf("claims.md did not escape %q:\n%s", escaped, claims)
		}
	}
}

func TestKnowledgeProjectionComputesEffectiveExpiry(t *testing.T) {
	p := New()
	p.clock = &knowledgeTestClock{now: time.Unix(1_800_000_000, 0).UTC()}
	claim := knowledgeTestClaim("expiring", "value", "workspace", domain.KnowledgeScopeProject)
	claim.ValidUntil = time.Unix(1_700_000_000, 0).UTC()
	snapshot := port.ProjectionSnapshot{Knowledge: port.KnowledgeSnapshot{Claims: []domain.KnowledgeClaim{claim}}}
	bundle := projectKnowledgeBundle(t, p, snapshot)
	claims := readBundleFile(t, bundle, "knowledge/claims.md")
	if !strings.Contains(claims, "status: `expired`") {
		t.Fatalf("effective expiry not computed:\n%s", claims)
	}
	if !strings.Contains(claims, "valid_until:") {
		t.Fatalf("validity window not projected:\n%s", claims)
	}
}

func TestKnowledgeProjectionInvalidRowPreservesPriorBundle(t *testing.T) {
	dir := t.TempDir()
	bundle := filepath.Join(dir, "memory")
	p := New()
	if err := p.Project(t.Context(), &stubProjectionReader{snapshot: knowledgeTestSnapshot()}, bundle); err != nil {
		t.Fatal(err)
	}
	before, err := collectBundleBytes(bundle)
	if err != nil {
		t.Fatal(err)
	}
	invalid := knowledgeTestSnapshot()
	invalid.Knowledge.Claims = []domain.KnowledgeClaim{{ID: "kclaim_bad", Subject: "", Status: domain.KnowledgeClaimAsserted}}
	if err := p.Project(t.Context(), &stubProjectionReader{snapshot: invalid}, bundle); err == nil {
		t.Fatal("Project accepted an invalid persisted row")
	}
	after, err := collectBundleBytes(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != len(after) {
		t.Fatalf("prior bundle changed after failed projection: %d files vs %d", len(before), len(after))
	}
	for name, data := range before {
		if string(after[name]) != string(data) {
			t.Fatalf("prior bundle file %s changed after failed projection", name)
		}
	}
}

func TestKnowledgeProjectionInvalidUTF8PreservesPriorBundle(t *testing.T) {
	dir := t.TempDir()
	bundle := filepath.Join(dir, "memory")
	p := New()
	if err := p.Project(t.Context(), &stubProjectionReader{snapshot: knowledgeTestSnapshot()}, bundle); err != nil {
		t.Fatal(err)
	}
	before, err := collectBundleBytes(bundle)
	if err != nil {
		t.Fatal(err)
	}
	invalid := knowledgeTestSnapshot()
	claim := invalid.Knowledge.Claims[0]
	claim.Value = domain.KnowledgeValue{Kind: domain.KnowledgeValueString, Text: "abc\xfe\xfe"}
	invalid.Knowledge.Claims = []domain.KnowledgeClaim{claim}
	if err := p.Project(t.Context(), &stubProjectionReader{snapshot: invalid}, bundle); err == nil {
		t.Fatal("Project accepted invalid UTF-8")
	}
	after, err := collectBundleBytes(bundle)
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range before {
		if string(after[name]) != string(data) {
			t.Fatalf("prior bundle file %s changed after failed projection", name)
		}
	}
}

func collectBundleBytes(bundle string) (map[string][]byte, error) {
	files := map[string][]byte{}
	err := filepath.WalkDir(bundle, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(bundle, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files[rel] = data
		return nil
	})
	return files, err
}

func TestKnowledgeProjectionFinalPermissions(t *testing.T) {
	p := New()
	bundle := projectKnowledgeBundle(t, p, knowledgeTestSnapshot())
	info, err := os.Stat(filepath.Join(bundle, "knowledge"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("knowledge dir mode = %o, want 700", perm)
	}
	fileInfo, err := os.Stat(filepath.Join(bundle, "knowledge", "claims.md"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := fileInfo.Mode().Perm(); perm != 0o600 {
		t.Fatalf("claims.md mode = %o, want 600", perm)
	}
}

func TestKnowledgeProjectionRejectsSymlinkTarget(t *testing.T) {
	p := New()
	bundle := projectKnowledgeBundle(t, p, knowledgeTestSnapshot())
	target := filepath.Join(t.TempDir(), "outside.md")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(bundle, "knowledge", "claims.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(bundle, "knowledge", "claims.md")); err != nil {
		t.Fatal(err)
	}
	if err := p.Project(t.Context(), &stubProjectionReader{snapshot: knowledgeTestSnapshot()}, bundle); err == nil {
		t.Fatal("Project overwrote a symlink target")
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "keep" {
		t.Fatalf("symlink target modified: %q, %v", data, err)
	}
	if _, err := os.Lstat(filepath.Join(bundle, "knowledge", "claims.md")); err != nil || os.IsNotExist(err) {
		t.Fatalf("aborted promotion touched the live bundle: %v", err)
	}
}

func TestKnowledgeProjectionRejectsSymlinkDirectory(t *testing.T) {
	p := New()
	bundle := projectKnowledgeBundle(t, p, knowledgeTestSnapshot())
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(bundle, "knowledge")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(bundle, "knowledge")); err != nil {
		t.Fatal(err)
	}
	if err := p.Project(t.Context(), &stubProjectionReader{snapshot: knowledgeTestSnapshot()}, bundle); err == nil {
		t.Fatal("Project rendered through a symlinked knowledge directory")
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("symlink directory was modified: %d entries", len(entries))
	}
	if info, err := os.Lstat(filepath.Join(bundle, "knowledge")); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("aborted promotion modified the live bundle: %v", err)
	}
}

func TestKnowledgeProjectionRejectsSymlinkStaging(t *testing.T) {
	p := New()
	bundle := projectKnowledgeBundle(t, p, knowledgeTestSnapshot())
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	staging := filepath.Join(filepath.Dir(bundle), ".okf-staging-"+filepath.Base(bundle))
	if err := os.Symlink(outside, staging); err != nil {
		t.Fatal(err)
	}
	if err := p.Project(t.Context(), &stubProjectionReader{snapshot: knowledgeTestSnapshot()}, bundle); err == nil {
		t.Fatal("Project accepted a symlinked staging path")
	}
}

func TestKnowledgeProjectionConcurrentPromotionsDoNotCorrupt(t *testing.T) {
	p := New()
	alpha := knowledgeTestSnapshot()
	alpha.Knowledge.Claims = []domain.KnowledgeClaim{knowledgeTestClaim("alpha subject", "alpha value", "workspace", domain.KnowledgeScopeProject)}
	beta := knowledgeTestSnapshot()
	beta.Knowledge.Claims = []domain.KnowledgeClaim{knowledgeTestClaim("beta subject", "beta value", "workspace", domain.KnowledgeScopeProject)}
	expectedA := renderExpected(t, p, alpha)
	expectedB := renderExpected(t, p, beta)

	dir := t.TempDir()
	bundle := filepath.Join(dir, "memory")
	readers := []port.ProjectionReader{
		&stubProjectionReader{snapshot: alpha},
		&stubProjectionReader{snapshot: beta},
	}
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Go(func() {
			if err := p.Project(context.Background(), readers[i%2], bundle); err != nil {
				t.Errorf("concurrent projection: %v", err)
			}
		})
	}
	wg.Wait()

	claims := readBundleFile(t, bundle, "knowledge/claims.md")
	if claims != expectedA && claims != expectedB {
		t.Fatalf("bundle is a corrupted mix, not one complete snapshot:\n%s", claims)
	}
	for _, leftover := range []string{".okf-staging-memory", ".okf-backup-memory"} {
		if _, err := os.Lstat(filepath.Join(dir, leftover)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("staging/backup leftover %q: %v", leftover, err)
		}
	}
}

func renderExpected(t *testing.T, p *Projector, snapshot port.ProjectionSnapshot) string {
	t.Helper()
	dir := t.TempDir()
	if err := p.renderBundle(dir, snapshot); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "knowledge", "claims.md"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

type stubProjectionReader struct {
	snapshot port.ProjectionSnapshot
}

func (r *stubProjectionReader) ReadProjectionSnapshot(_ context.Context) (port.ProjectionSnapshot, error) {
	return r.snapshot, nil
}

type errorProjectionReader struct {
	err error
}

func (r *errorProjectionReader) ReadProjectionSnapshot(_ context.Context) (port.ProjectionSnapshot, error) {
	return port.ProjectionSnapshot{}, r.err
}

func TestKnowledgeProjectionForgetRemovesProjectedContent(t *testing.T) {
	p := New()
	snapshot := knowledgeTestSnapshot()
	bundle := projectKnowledgeBundle(t, p, snapshot)
	claims := readBundleFile(t, bundle, "knowledge/claims.md")
	if !strings.Contains(claims, "PostgreSQL 17") {
		t.Fatalf("initial projection missing content:\n%s", claims)
	}
	// Forget: SQLite deletes the row and leaves only a tombstone, which is
	// never projected. The next snapshot carries no knowledge rows.
	if err := p.Project(t.Context(), &stubProjectionReader{snapshot: port.ProjectionSnapshot{}}, bundle); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(bundle, "knowledge")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("knowledge tree survived forget: %v", err)
	}
	text := readBundleFile(t, bundle, "index.md")
	if strings.Contains(text, "knowledge") || strings.Contains(text, "PostgreSQL") {
		t.Fatalf("forgotten content or knowledge link still projected:\n%s", text)
	}
}

func TestKnowledgeProjectionRejectsMalformedPersistedIDs(t *testing.T) {
	p := New()
	cases := map[string]func(*port.ProjectionSnapshot){
		"claim id": func(s *port.ProjectionSnapshot) {
			s.Knowledge.Claims[0].ID = domain.KnowledgeClaimID("kclaim_`code`")
		},
		"supersedes id": func(s *port.ProjectionSnapshot) {
			s.Knowledge.Claims[0].SupersedesID = domain.KnowledgeClaimID("kclaim_`code`")
		},
		"evidence claim id": func(s *port.ProjectionSnapshot) {
			s.Knowledge.Evidence[0].ClaimID = domain.KnowledgeClaimID("kclaim_`code`")
		},
		"document id": func(s *port.ProjectionSnapshot) {
			s.Knowledge.Documents[0].ID = domain.KnowledgeDocumentID("kdoc_`code`")
		},
	}
	for name, mutate := range cases {
		snapshot := knowledgeTestSnapshot()
		mutate(&snapshot)
		dir := t.TempDir()
		if err := p.Project(t.Context(), &stubProjectionReader{snapshot: snapshot}, filepath.Join(dir, "memory")); err == nil {
			t.Fatalf("%s: Project accepted a malformed persisted id", name)
		}
		if _, err := os.Stat(filepath.Join(dir, "memory", "knowledge", "claims.md")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s: failed projection promoted a live bundle", name)
		}
	}
}

func TestKnowledgeProjectionRendersClaimAndDocumentIdentity(t *testing.T) {
	p := New()
	claim := knowledgeTestClaim("database", "pg-02", "workspace", domain.KnowledgeScopeProject)
	claim.ID = domain.KnowledgeClaimID("kclaim_" + strings.Repeat("c", 24))
	claim.SupersedesID = domain.KnowledgeClaimID("kclaim_" + strings.Repeat("d", 24))
	document := knowledgeTestDocument()
	snapshot := knowledgeTestSnapshot()
	snapshot.Knowledge.Claims = []domain.KnowledgeClaim{claim}
	snapshot.Knowledge.Documents = []domain.KnowledgeDocument{document}
	snapshot.Knowledge.Evidence = []port.KnowledgeEvidenceRef{{
		ClaimID: claim.ID, RevisionNumber: 2, Kind: domain.KnowledgeEvidenceDecision, ExchangeTS: "1723543200.123456",
	}}
	bundle := projectKnowledgeBundle(t, p, snapshot)

	claims := readBundleFile(t, bundle, "knowledge/claims.md")
	for _, want := range []string{"- id: `kclaim_" + strings.Repeat("c", 24) + "`", "- supersedes: `kclaim_" + strings.Repeat("d", 24) + "`"} {
		if !strings.Contains(claims, want) {
			t.Fatalf("claims.md missing resolvable identity %q:\n%s", want, claims)
		}
	}
	evidence := readBundleFile(t, bundle, "knowledge/evidence.md")
	if !strings.Contains(evidence, "kclaim_"+strings.Repeat("c", 24)) || !strings.Contains(evidence, "revision 2") {
		t.Fatalf("evidence does not resolve to the rendered claim id:\n%s", evidence)
	}
	documents := readBundleFile(t, bundle, "knowledge/documents.md")
	if !strings.Contains(documents, "- id: `kdoc_"+strings.Repeat("b", 24)+"`") || !strings.Contains(documents, "- scope: project:workspace") {
		t.Fatalf("documents.md missing identity or scope:\n%s", documents)
	}
}

func TestPromotionFailureRollsBackPreviousBundle(t *testing.T) {
	p := New()
	bundle := projectKnowledgeBundle(t, p, knowledgeTestSnapshot())
	before, err := collectBundleBytes(bundle)
	if err != nil {
		t.Fatal(err)
	}

	failing := New()
	promoteFailed := false
	failing.renameFn = func(oldpath, newpath string) error {
		if !promoteFailed && newpath == bundle {
			promoteFailed = true
			return errors.New("injected promotion failure")
		}
		return os.Rename(oldpath, newpath)
	}
	next := knowledgeTestSnapshot()
	next.Knowledge.Claims = []domain.KnowledgeClaim{knowledgeTestClaim("gamma subject", "gamma value", "workspace", domain.KnowledgeScopeProject)}
	if err := failing.Project(t.Context(), &stubProjectionReader{snapshot: next}, bundle); err == nil {
		t.Fatal("injected promotion failure was swallowed")
	}
	after, err := collectBundleBytes(bundle)
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range before {
		if string(after[name]) != string(data) {
			t.Fatalf("live bundle file %s changed after failed promotion", name)
		}
	}
	for _, leftover := range []string{".okf-backup-memory", ".okf-staging-memory"} {
		if _, err := os.Lstat(filepath.Join(filepath.Dir(bundle), leftover)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("promotion failure left %q behind", leftover)
		}
	}
}

func TestPromotionFailureWithRollbackFailurePreservesBackup(t *testing.T) {
	p := New()
	bundle := projectKnowledgeBundle(t, p, knowledgeTestSnapshot())
	before, err := collectBundleBytes(bundle)
	if err != nil {
		t.Fatal(err)
	}

	failing := New()
	promoteFailed := false
	failing.renameFn = func(oldpath, newpath string) error {
		if !promoteFailed && newpath == bundle {
			promoteFailed = true
			return errors.New("injected promotion failure")
		}
		if strings.Contains(oldpath, ".okf-backup-") {
			return errors.New("injected rollback failure")
		}
		return os.Rename(oldpath, newpath)
	}
	next := knowledgeTestSnapshot()
	next.Knowledge.Claims = []domain.KnowledgeClaim{knowledgeTestClaim("delta subject", "delta value", "workspace", domain.KnowledgeScopeProject)}
	err = failing.Project(t.Context(), &stubProjectionReader{snapshot: next}, bundle)
	if err == nil || !strings.Contains(err.Error(), "rollback failed") {
		t.Fatalf("rollback failure not reported explicitly: %v", err)
	}
	backupDir := filepath.Join(filepath.Dir(bundle), ".okf-backup-memory")
	backup, err := collectBundleBytes(backupDir)
	if err != nil {
		t.Fatalf("previous bundle not preserved at backup path: %v", err)
	}
	for name, data := range before {
		if string(backup[name]) != string(data) {
			t.Fatalf("backup file %s differs from the previous bundle", name)
		}
	}
}

func TestStagingCleanupFailuresKeepTypedErrorUntilHealed(t *testing.T) {
	p := New()
	bundle := projectKnowledgeBundle(t, p, knowledgeTestSnapshot())
	first, err := collectBundleBytes(bundle)
	if err != nil {
		t.Fatal(err)
	}
	stagingRemovals := 0
	failing := New()
	// The first promotion fails (with a successful rollback) and every
	// staging removal fails until the filesystem heals, leaving a staging
	// residue that can retain content the next projection forgets.
	promoteFailed := false
	failing.renameFn = func(oldpath, newpath string) error {
		if !promoteFailed && strings.Contains(oldpath, ".okf-staging-") && newpath == bundle {
			promoteFailed = true
			return errors.New("injected promotion failure")
		}
		return os.Rename(oldpath, newpath)
	}
	failing.removeAllFn = func(path string) error {
		if strings.Contains(path, ".okf-staging-") {
			if _, lerr := os.Lstat(path); lerr == nil {
				stagingRemovals++
				if stagingRemovals <= 4 {
					return errors.New("injected staging removal failure")
				}
			}
		}
		return os.RemoveAll(path)
	}
	next := knowledgeTestSnapshot()
	next.Knowledge.Claims = []domain.KnowledgeClaim{knowledgeTestClaim("theta subject", "theta value", "workspace", domain.KnowledgeScopeProject)}
	// The first failure is the joined promotion+cleanup error; the live
	// bundle is still the previous content and staging residue remains.
	err = failing.Project(t.Context(), &stubProjectionReader{snapshot: next}, bundle)
	if !errors.Is(err, port.ErrProjectionCleanup) {
		t.Fatalf("first joined error = %v, want ErrProjectionCleanup in the chain", err)
	}
	// Every retry keeps the typed error while the staging residue cannot
	// be removed, without touching the live bundle or creating a backup.
	for i := range 3 {
		if err := failing.Project(t.Context(), &stubProjectionReader{snapshot: next}, bundle); !errors.Is(err, port.ErrProjectionCleanup) {
			t.Fatalf("retry %d error = %v, want ErrProjectionCleanup", i+1, err)
		}
	}
	after, err := collectBundleBytes(bundle)
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range first {
		if string(after[name]) != string(data) {
			t.Fatalf("live bundle file %s modified during failed staging retries", name)
		}
	}
	// Once the filesystem heals, the same projection removes the staging
	// residue and completes.
	if err := failing.Project(t.Context(), &stubProjectionReader{snapshot: next}, bundle); err != nil {
		t.Fatalf("healed projection error = %v", err)
	}
	if claims := readBundleFile(t, bundle, "knowledge/claims.md"); !strings.Contains(claims, "theta value") {
		t.Fatalf("healed projection lost the promoted content:\n%s", claims)
	}
	for _, leftover := range []string{".okf-backup-memory", ".okf-staging-memory"} {
		if _, err := os.Lstat(filepath.Join(filepath.Dir(bundle), leftover)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("healed projection left %q behind", leftover)
		}
	}
}

func TestRecoverPreservesBackupWhenLivePathIsRegularFile(t *testing.T) {
	p := New()
	bundle := projectKnowledgeBundle(t, p, knowledgeTestSnapshot())
	before, err := collectBundleBytes(bundle)
	if err != nil {
		t.Fatal(err)
	}
	// Move the real bundle to the backup position and plant a regular file
	// at the live path: the backup is the only valid copy.
	backupDir := filepath.Join(filepath.Dir(bundle), ".okf-backup-memory")
	if err := os.Rename(bundle, backupDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bundle, []byte("not a bundle"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := p.Recover(bundle); err == nil {
		t.Fatal("Recover discarded the backup despite a regular file at the live path")
	}
	backup, err := collectBundleBytes(backupDir)
	if err != nil {
		t.Fatalf("backup was discarded despite invalid live path: %v", err)
	}
	for name, data := range before {
		if string(backup[name]) != string(data) {
			t.Fatalf("backup file %s modified", name)
		}
	}
	if content, err := os.ReadFile(bundle); err != nil || string(content) != "not a bundle" {
		t.Fatalf("live path file modified: %q, %v", content, err)
	}
}

func TestRecoverPreservesBackupPathFile(t *testing.T) {
	p := New()
	bundle := projectKnowledgeBundle(t, p, knowledgeTestSnapshot())
	before, err := collectBundleBytes(bundle)
	if err != nil {
		t.Fatal(err)
	}
	backupDir := filepath.Join(filepath.Dir(bundle), ".okf-backup-memory")
	if err := os.WriteFile(backupDir, []byte("backup path marker"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := p.Recover(bundle); err == nil {
		t.Fatal("Recover treated a regular file at the backup path as a bundle")
	}
	if content, err := os.ReadFile(backupDir); err != nil || string(content) != "backup path marker" {
		t.Fatalf("backup path file modified: %q, %v", content, err)
	}
	after, err := collectBundleBytes(bundle)
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range before {
		if string(after[name]) != string(data) {
			t.Fatalf("live bundle file %s modified", name)
		}
	}
}

func TestRollbackFailureKeepsTypedErrorForStagingResidueUntilHealed(t *testing.T) {
	p := New()
	bundle := projectKnowledgeBundle(t, p, knowledgeTestSnapshot())
	first, err := collectBundleBytes(bundle)
	if err != nil {
		t.Fatal(err)
	}
	renameFailures := 2 // one promotion failure, then one rollback failure
	stagingRemovals := 0
	failing := New()
	failing.renameFn = func(oldpath, newpath string) error {
		if newpath != bundle {
			return os.Rename(oldpath, newpath)
		}
		if renameFailures > 0 {
			renameFailures--
			if strings.Contains(oldpath, ".okf-staging-") {
				return errors.New("injected promotion failure")
			}
			return errors.New("injected rollback failure")
		}
		return os.Rename(oldpath, newpath)
	}
	failing.removeAllFn = func(path string) error {
		if strings.Contains(path, ".okf-staging-") {
			if _, lerr := os.Lstat(path); lerr == nil {
				stagingRemovals++
				if stagingRemovals <= 4 {
					return errors.New("injected staging removal failure")
				}
			}
		}
		return os.RemoveAll(path)
	}
	next := knowledgeTestSnapshot()
	next.Knowledge.Claims = []domain.KnowledgeClaim{knowledgeTestClaim("iota subject", "iota value", "workspace", domain.KnowledgeScopeProject)}
	// Promotion and rollback both fail and staging cleanup fails too: the
	// combined error must keep the typed cleanup error in its chain, the
	// previous bundle survives only at the backup path, and the staging
	// residue remains.
	err = failing.Project(t.Context(), &stubProjectionReader{snapshot: next}, bundle)
	if err == nil || !strings.Contains(err.Error(), "rollback failed") {
		t.Fatalf("rollback failure not reported explicitly: %v", err)
	}
	if !errors.Is(err, port.ErrProjectionCleanup) {
		t.Fatalf("combined error = %v, want ErrProjectionCleanup in the chain", err)
	}
	if _, err := os.Lstat(bundle); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("live bundle should be missing after failed promotion and rollback: %v", err)
	}
	backupDir := filepath.Join(filepath.Dir(bundle), ".okf-backup-memory")
	backup, err := collectBundleBytes(backupDir)
	if err != nil {
		t.Fatalf("previous bundle not preserved at backup path: %v", err)
	}
	for name, data := range first {
		if string(backup[name]) != string(data) {
			t.Fatalf("backup file %s differs from the previous bundle", name)
		}
	}
	// Every retry keeps the typed error while the staging residue cannot
	// be removed, and never touches the backup.
	for i := range 3 {
		if err := failing.Project(t.Context(), &stubProjectionReader{snapshot: next}, bundle); !errors.Is(err, port.ErrProjectionCleanup) {
			t.Fatalf("retry %d error = %v, want ErrProjectionCleanup", i+1, err)
		}
		if _, err := collectBundleBytes(backupDir); err != nil {
			t.Fatalf("backup lost during failed staging retries: %v", err)
		}
	}
	// Once the filesystem heals, the same projection removes the staging
	// residue, restores the previous bundle, and promotes the new one.
	if err := failing.Project(t.Context(), &stubProjectionReader{snapshot: next}, bundle); err != nil {
		t.Fatalf("healed projection error = %v", err)
	}
	if claims := readBundleFile(t, bundle, "knowledge/claims.md"); !strings.Contains(claims, "iota value") {
		t.Fatalf("healed projection lost the promoted content:\n%s", claims)
	}
	for _, leftover := range []string{".okf-backup-memory", ".okf-staging-memory"} {
		if _, err := os.Lstat(filepath.Join(filepath.Dir(bundle), leftover)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("healed projection left %q behind", leftover)
		}
	}
}

func TestRetryAfterFailedRollbackRestoresPreviousBundleBeforeRender(t *testing.T) {
	p := New()
	bundle := projectKnowledgeBundle(t, p, knowledgeTestSnapshot())
	first, err := collectBundleBytes(bundle)
	if err != nil {
		t.Fatal(err)
	}

	// Promotion and rollback both fail: the previous bundle survives only
	// at the backup path and the live bundle is missing.
	broken := New()
	broken.renameFn = func(oldpath, newpath string) error {
		if newpath == bundle {
			return errors.New("injected promotion/rollback failure")
		}
		return os.Rename(oldpath, newpath)
	}
	next := knowledgeTestSnapshot()
	next.Knowledge.Claims = []domain.KnowledgeClaim{knowledgeTestClaim("eta subject", "eta value", "workspace", domain.KnowledgeScopeProject)}
	err = broken.Project(t.Context(), &stubProjectionReader{snapshot: next}, bundle)
	if err == nil || !strings.Contains(err.Error(), "rollback failed") {
		t.Fatalf("rollback failure not reported explicitly: %v", err)
	}
	backupDir := filepath.Join(filepath.Dir(bundle), ".okf-backup-memory")
	if _, err := os.Lstat(bundle); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("live bundle should be missing after failed promotion and rollback: %v", err)
	}
	if _, err := collectBundleBytes(backupDir); err != nil {
		t.Fatalf("previous bundle not preserved at backup path: %v", err)
	}

	// The retry fails at the snapshot read. Recovery at the start of the
	// projection must restore the previous bundle in place first, so the
	// failed retry leaves the previous content intact instead of an empty
	// live bundle with the only copy deleted.
	failingReader := &errorProjectionReader{err: errors.New("injected snapshot read failure")}
	if err := p.Project(t.Context(), failingReader, bundle); err == nil {
		t.Fatal("retry with failing reader unexpectedly succeeded")
	}
	after, err := collectBundleBytes(bundle)
	if err != nil {
		t.Fatalf("live bundle missing after failed retry: %v", err)
	}
	for name, data := range first {
		if string(after[name]) != string(data) {
			t.Fatalf("restored live bundle file %s differs from the previous bundle", name)
		}
	}
	for _, leftover := range []string{".okf-backup-memory", ".okf-staging-memory"} {
		if _, err := os.Lstat(filepath.Join(filepath.Dir(bundle), leftover)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("failed retry left %q behind", leftover)
		}
	}
}

func TestBackupCleanupFailureReportsTypedErrorAndRecoverRemovesResidue(t *testing.T) {
	p := New()
	bundle := projectKnowledgeBundle(t, p, knowledgeTestSnapshot())
	before, err := collectBundleBytes(bundle)
	if err != nil {
		t.Fatal(err)
	}

	failing := New()
	backupCalls := 0
	failing.removeAllFn = func(path string) error {
		if strings.Contains(path, ".okf-backup-") {
			backupCalls++
			if backupCalls >= 2 {
				return errors.New("injected backup cleanup failure")
			}
		}
		return os.RemoveAll(path)
	}
	next := knowledgeTestSnapshot()
	next.Knowledge.Claims = []domain.KnowledgeClaim{knowledgeTestClaim("epsilon subject", "epsilon value", "workspace", domain.KnowledgeScopeProject)}
	err = failing.Project(t.Context(), &stubProjectionReader{snapshot: next}, bundle)
	if !errors.Is(err, port.ErrProjectionCleanup) {
		t.Fatalf("cleanup failure error = %v, want ErrProjectionCleanup", err)
	}
	claims := readBundleFile(t, bundle, "knowledge/claims.md")
	if !strings.Contains(claims, "epsilon value") {
		t.Fatalf("live bundle was not the promoted snapshot:\n%s", claims)
	}
	backupDir := filepath.Join(filepath.Dir(bundle), ".okf-backup-memory")
	backup, err := collectBundleBytes(backupDir)
	if err != nil {
		t.Fatalf("backup residue missing after failed cleanup: %v", err)
	}
	for name, data := range before {
		if string(backup[name]) != string(data) {
			t.Fatalf("backup file %s differs from the previous bundle", name)
		}
	}

	// Startup recovery removes the residue without requiring a knowledge
	// mutation.
	if err := p.Recover(bundle); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(backupDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("residue backup survived recovery: %v", err)
	}
	if claims := readBundleFile(t, bundle, "knowledge/claims.md"); !strings.Contains(claims, "epsilon value") {
		t.Fatalf("recovery touched the live bundle:\n%s", claims)
	}
}

func TestRecoverRestoresBackupWhenLiveBundleMissing(t *testing.T) {
	p := New()
	bundle := projectKnowledgeBundle(t, p, knowledgeTestSnapshot())
	before, err := collectBundleBytes(bundle)
	if err != nil {
		t.Fatal(err)
	}
	backupDir := filepath.Join(filepath.Dir(bundle), ".okf-backup-memory")
	// Simulate a crash between backup and promotion: the live bundle is
	// missing and the backup holds the only copy.
	if err := os.Rename(bundle, backupDir); err != nil {
		t.Fatal(err)
	}
	if err := p.Recover(bundle); err != nil {
		t.Fatal(err)
	}
	after, err := collectBundleBytes(bundle)
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range before {
		if string(after[name]) != string(data) {
			t.Fatalf("restored bundle file %s differs from the previous bundle", name)
		}
	}
	if _, err := os.Lstat(backupDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("restored backup still present: %v", err)
	}
}

func TestRecoverRejectsSymlinkedResiduePaths(t *testing.T) {
	p := New()
	bundle := projectKnowledgeBundle(t, p, knowledgeTestSnapshot())
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	backupDir := filepath.Join(filepath.Dir(bundle), ".okf-backup-memory")
	if err := os.Symlink(outside, backupDir); err != nil {
		t.Fatal(err)
	}
	if err := p.Recover(bundle); err == nil {
		t.Fatal("Recover followed a symlinked backup path")
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("symlinked backup target modified: %d entries", len(entries))
	}
}

func TestRepeatedCleanupFailuresKeepTypedErrorUntilHealed(t *testing.T) {
	p := New()
	bundle := projectKnowledgeBundle(t, p, knowledgeTestSnapshot())
	first, err := collectBundleBytes(bundle)
	if err != nil {
		t.Fatal(err)
	}
	removals := 0
	failing := New()
	failing.removeAllFn = func(path string) error {
		if strings.Contains(path, ".okf-backup-") {
			if _, lerr := os.Lstat(path); lerr == nil {
				removals++
				if removals <= 4 {
					return errors.New("injected backup removal failure")
				}
			}
		}
		return os.RemoveAll(path)
	}
	next := knowledgeTestSnapshot()
	next.Knowledge.Claims = []domain.KnowledgeClaim{knowledgeTestClaim("zeta subject", "zeta value", "workspace", domain.KnowledgeScopeProject)}
	// The first promotion publishes the new live bundle but fails backup
	// cleanup: the residue backup keeps the previous content.
	if err := failing.Project(t.Context(), &stubProjectionReader{snapshot: next}, bundle); !errors.Is(err, port.ErrProjectionCleanup) {
		t.Fatalf("first cleanup failure error = %v, want ErrProjectionCleanup", err)
	}
	// Every subsequent attempt must keep the typed error while the residue
	// backup cannot be removed (internal recovery runs under the mutex at
	// the start of each Project), never degrade into a generic failure,
	// and never touch the live bundle.
	for i := range 2 {
		if err := failing.Project(t.Context(), &stubProjectionReader{snapshot: next}, bundle); !errors.Is(err, port.ErrProjectionCleanup) {
			t.Fatalf("retry %d error = %v, want ErrProjectionCleanup", i+1, err)
		}
	}
	if claims := readBundleFile(t, bundle, "knowledge/claims.md"); !strings.Contains(claims, "zeta value") {
		t.Fatalf("live bundle modified during failed recovery:\n%s", claims)
	}
	backupDir := filepath.Join(filepath.Dir(bundle), ".okf-backup-memory")
	backup, err := collectBundleBytes(backupDir)
	if err != nil {
		t.Fatalf("residue backup missing after failed recovery: %v", err)
	}
	for name, data := range first {
		if string(backup[name]) != string(data) {
			t.Fatalf("backup file %s differs from the previous bundle", name)
		}
	}
	// Once the filesystem heals, the same projection removes the residue
	// and completes.
	if err := failing.Project(t.Context(), &stubProjectionReader{snapshot: next}, bundle); err != nil {
		t.Fatalf("healed projection error = %v", err)
	}
	if _, err := os.Lstat(backupDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("residue backup survived the healed projection: %v", err)
	}
	if claims := readBundleFile(t, bundle, "knowledge/claims.md"); !strings.Contains(claims, "zeta value") {
		t.Fatalf("healed projection lost the promoted content:\n%s", claims)
	}
}

func TestRecoverRejectsDescendantSymlinkInLiveBundle(t *testing.T) {
	p := New()
	bundle := projectKnowledgeBundle(t, p, knowledgeTestSnapshot())
	// Simulate a failed cleanup residue backup holding the only recoverable
	// copy of the previous bundle.
	backupDir := filepath.Join(filepath.Dir(bundle), ".okf-backup-memory")
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(backupDir, "previous.md")
	if err := os.WriteFile(marker, []byte("previous bundle marker"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Plant a symlink inside the live bundle, deeper than the root: the
	// live path itself looks fine but the tree cannot be trusted.
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	planted := filepath.Join(bundle, "knowledge", "planted")
	if err := os.Symlink(outside, planted); err != nil {
		t.Fatal(err)
	}
	if err := p.Recover(bundle); err == nil {
		t.Fatal("Recover discarded the backup despite a descendant symlink in the live bundle")
	}
	if _, err := os.Lstat(marker); err != nil {
		t.Fatalf("backup was discarded despite descendant symlink: %v", err)
	}
	if _, err := os.Lstat(planted); err != nil {
		t.Fatalf("descendant symlink was modified: %v", err)
	}
	if _, err := os.ReadDir(outside); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("symlink target modified: %d entries", len(entries))
	}
}

func TestRecoverRejectsSymlinkedLiveBundle(t *testing.T) {
	p := New()
	bundle := projectKnowledgeBundle(t, p, knowledgeTestSnapshot())
	before, err := collectBundleBytes(bundle)
	if err != nil {
		t.Fatal(err)
	}
	// Move the real bundle to the backup position, then plant a symlink at
	// the live path: recovery must refuse to discard the backup.
	backupDir := filepath.Join(filepath.Dir(bundle), ".okf-backup-memory")
	if err := os.Rename(bundle, backupDir); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, bundle); err != nil {
		t.Fatal(err)
	}
	if err := p.Recover(bundle); err == nil {
		t.Fatal("Recover accepted a symlinked live bundle path")
	}
	backup, err := collectBundleBytes(backupDir)
	if err != nil {
		t.Fatalf("backup was discarded despite symlinked live path: %v", err)
	}
	for name, data := range before {
		if string(backup[name]) != string(data) {
			t.Fatalf("backup file %s modified", name)
		}
	}
}
