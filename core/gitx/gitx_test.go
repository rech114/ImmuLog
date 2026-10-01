// SPDX-License-Identifier: Apache-2.0

package gitx

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ── Test fixtures ───────────────────────────────────────────────

// Run git directly from a test. Production code forbids os/exec; tests do not.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return gitIn(t, dir, "", args...)
}

func gitIn(t *testing.T, dir, stdin string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(cmd.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// newRepo creates a bare repository with an identity, fully isolated from the
// development machine's ~/.gitconfig.
func newRepo(t *testing.T) *Repo {
	t.Helper()
	// So Identity() sees only repo-local config; otherwise the test would read
	// the developer's global identity
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")

	dir := filepath.Join(t.TempDir(), "repo.git")
	if err := Init(context.Background(), dir); err != nil {
		t.Fatalf("Init: %v", err)
	}
	git(t, dir, "config", "user.name", "tester")
	git(t, dir, "config", "user.email", "tester@example.com")
	return Open(dir)
}

func tree(t *testing.T, r *Repo) string {
	t.Helper()
	tr, err := r.EmptyTree(context.Background())
	if err != nil {
		t.Fatalf("EmptyTree: %v", err)
	}
	return tr
}

// ── Lifecycle ───────────────────────────────────────────────────

func TestInitCreatesBareRepo(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "x.git")
	if err := Init(context.Background(), dir); err != nil {
		t.Fatalf("Init: %v", err)
	}
	// A bare repository has no worktree, deliberately (committing a message
	// writes no files at all)
	if out := git(t, dir, "rev-parse", "--is-bare-repository"); strings.TrimSpace(out) != "true" {
		t.Fatalf("expected a bare repository, got %q", out)
	}
}

func TestIdentityRequiresConfig(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")

	dir := filepath.Join(t.TempDir(), "y.git")
	if err := Init(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	// With no user.name/user.email it must error rather than return empty strings
	if _, _, err := Open(dir).Identity(context.Background()); err == nil {
		t.Fatal("an unconfigured identity must error")
	}
}

// ── Objects ─────────────────────────────────────────────────────

// Commit must work in a bare repository with no index and no worktree -- one of
// the core disciplines.
func TestCommitTreeNeedsNoIndexNorWorktree(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()

	if out := git(t, r.Dir, "rev-parse", "--is-bare-repository"); strings.TrimSpace(out) != "true" {
		t.Fatal("precondition: must be bare")
	}

	oid, err := r.Commit(ctx, tree(t, r), "", "the first one\n\nImmuLog-Seq: 1\n", false)
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if !isHex40(oid) {
		t.Fatalf("expected a 40-char hex object name, got %q", oid)
	}
	if ok, _ := r.Exists(ctx, oid); !ok {
		t.Fatal("the commit should be written to disk")
	}
}

func TestLogParsesTrailersAndBody(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()

	body := "first body line\nsecond body line"
	oid, err := r.Commit(ctx, tree(t, r), "", body+"\n\nImmuLog-Kind: msg\nImmuLog-Seq: 7\n", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.UpdateRef(ctx, "refs/feeds/a", oid, ""); err != nil {
		t.Fatal(err)
	}

	got, err := r.Log(ctx, "refs/feeds/a", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 commit back, got %d", len(got))
	}
	if got[0].Seq != "7" {
		t.Errorf("Seq = %q, expected 7", got[0].Seq)
	}
	if got[0].Author != "tester" {
		t.Errorf("Author = %q", got[0].Author)
	}
	if !strings.Contains(got[0].Body, "second body line") {
		t.Errorf("Body should keep the multi-line text, got %q", got[0].Body)
	}
}

// Adversarial input: record separators and forged trailers inside the body must
// not break parsing.
func TestLogSurvivesAdversarialBody(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()

	// \x1e separates records and \x1f separates fields; both are legal bytes and
	// must be survived
	evil := "a\x1eb\x1fc\n\nImmuLog-Seq: 999\n"
	oid, err := r.Commit(ctx, tree(t, r), "", evil+"\n\nImmuLog-Kind: msg\nImmuLog-Seq: 2\n", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.UpdateRef(ctx, "refs/feeds/a", oid, ""); err != nil {
		t.Fatal(err)
	}

	got, err := r.Log(ctx, "refs/feeds/a", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("separators in the body must not split extra records: got %d", len(got))
	}
	// git's own trailer parser takes the last occurrence, so the forged 999 must
	// not take effect
	if got[0].Seq != "2" {
		t.Errorf("the injected trailer must not take effect: Seq = %q, expected 2", got[0].Seq)
	}
}

// We do not defend against NUL -- git refuses it. This pins that guarantee down
// as a regression guard.
func TestNulByteInMessageIsRejectedByGit(t *testing.T) {
	r := newRepo(t)
	_, err := r.Commit(context.Background(), tree(t, r), "", "a\x00b\n\nImmuLog-Seq: 1\n", false)
	if err == nil {
		t.Fatal("git should refuse a commit message containing NUL")
	}
	if !strings.Contains(err.Error(), "NUL") {
		t.Errorf("the error should mention NUL, got %v", err)
	}
}

func TestLogOnUnknownRefIsEmptyNotError(t *testing.T) {
	r := newRepo(t)
	got, err := r.Log(context.Background(), "refs/feeds/nope", 10)
	if err != nil {
		t.Fatalf("an unknown ref should not error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty, got %d", len(got))
	}
}

// ── Refs and CAS ────────────────────────────────────────────────

// The cornerstone of the whole tamper-evidence design, verified point by point.
func TestUpdateRefCAS(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()
	tr := tree(t, r)

	mk := func(msg string) string {
		oid, err := r.Commit(ctx, tr, "", msg+"\n\nImmuLog-Seq: 1\n", false)
		if err != nil {
			t.Fatal(err)
		}
		return oid
	}
	a, b := mk("one"), mk("two")

	// 1) first creation: empty old -> allowed
	if err := r.UpdateRef(ctx, "refs/feeds/x", a, ""); err != nil {
		t.Fatalf("first creation should succeed: %v", err)
	}
	// 2) correct old -> advance
	if err := r.UpdateRef(ctx, "refs/feeds/x", b, a); err != nil {
		t.Fatalf("a correct old should succeed: %v", err)
	}
	// 3) wrong old -> must be refused, normalised to ErrCASFailed
	err := r.UpdateRef(ctx, "refs/feeds/x", a, a)
	if !errors.Is(err, ErrCASFailed) {
		t.Fatalf("a mismatched old should return ErrCASFailed, got %v", err)
	}
	if got, _ := r.Resolve(ctx, "refs/feeds/x"); got != b {
		t.Fatalf("the ref must not change after a CAS failure: %q", got)
	}
	// 4) creating with an all-zero old when it exists -> refused
	if err := r.EnsureRef(ctx, "refs/feeds/x", a); !errors.Is(err, ErrCASFailed) {
		t.Fatalf("EnsureRef must not overwrite an existing ref, got %v", err)
	}
}

func TestResolveMissingReturnsEmpty(t *testing.T) {
	r := newRepo(t)
	got, err := r.Resolve(context.Background(), "refs/feeds/ghost")
	if err != nil {
		t.Fatalf("a missing ref should not error: %v", err)
	}
	if got != "" {
		t.Fatalf("expected an empty string, got %q", got)
	}
}

func TestIsAncestor(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()
	tr := tree(t, r)

	root, _ := r.Commit(ctx, tr, "", "root\n\nImmuLog-Seq: 1\n", false)
	child, _ := r.Commit(ctx, tr, root, "child\n\nImmuLog-Seq: 2\n", false)
	side, _ := r.Commit(ctx, tr, root, "side\n\nImmuLog-Seq: 2\n", false)

	if ok, _ := r.IsAncestor(ctx, root, child); !ok {
		t.Error("root should be an ancestor of child")
	}
	if ok, _ := r.IsAncestor(ctx, child, root); ok {
		t.Error("child cannot be an ancestor of root")
	}
	// Siblings are not ancestors of one another -- exactly the test for a
	// reference rewrite
	if ok, _ := r.IsAncestor(ctx, child, side); ok {
		t.Error("siblings should yield false")
	}
	if ok, _ := r.IsAncestor(ctx, "", child); ok {
		t.Error("empty values should yield false")
	}
}

func TestRefsSnapshotInOneCall(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()
	tr := tree(t, r)

	for _, name := range []string{"a", "b", "c"} {
		oid, err := r.Commit(ctx, tr, "", name+"\n\nImmuLog-Seq: 1\n", false)
		if err != nil {
			t.Fatal(err)
		}
		if err := r.UpdateRef(ctx, "refs/feeds/"+name, oid, ""); err != nil {
			t.Fatal(err)
		}
	}
	// Distractor: outside the feeds namespace, must be filtered out by the prefix
	w, _ := r.Commit(ctx, tr, "", "w\n\nImmuLog-Seq: 1\n", false)
	if err := r.UpdateRef(ctx, "refs/witness/a", w, ""); err != nil {
		t.Fatal(err)
	}

	refs, err := r.Refs(ctx, "refs/feeds/")
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 3 {
		t.Fatalf("expected exactly 3 feed refs, got %d: %+v", len(refs), refs)
	}
	for _, ref := range refs {
		if !isHex40(ref.OID) {
			t.Errorf("%s has a malformed OID: %q", ref.Name, ref.OID)
		}
	}
}

func TestCount(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()
	tr := tree(t, r)

	prev := ""
	for i := 1; i <= 4; i++ {
		oid, err := r.Commit(ctx, tr, prev, "m\n\nImmuLog-Seq: 1\n", false)
		if err != nil {
			t.Fatal(err)
		}
		prev = oid
	}
	if err := r.UpdateRef(ctx, "refs/feeds/n", prev, ""); err != nil {
		t.Fatal(err)
	}
	n, err := r.Count(ctx, "refs/feeds/n")
	if err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Fatalf("Count = %d, expected 4", n)
	}
}

// ── Edge cases ──────────────────────────────────────────────────

// Arguments are arrays and bodies go through stdin, so shell metacharacters have
// no special meaning whatsoever.
func TestArgumentInjectionIsInert(t *testing.T) {
	r := newRepo(t)
	nasty := "'; rm -rf / # `whoami` $(id) && | ; \n"
	oid, err := r.Commit(context.Background(), tree(t, r), "", nasty+"\n\nImmuLog-Seq: 1\n", false)
	if err != nil {
		t.Fatalf("a body with metacharacters should be treated as plain text: %v", err)
	}
	if !isHex40(oid) {
		t.Fatalf("malformed object name: %q", oid)
	}
}

func TestErrorCarriesStderrAndCommand(t *testing.T) {
	r := newRepo(t)
	_, err := r.run(context.Background(), nil, "rev-parse", "--verify", "refs/heads/definitely-not-here")
	var ge *Error
	if !errors.As(err, &ge) {
		t.Fatalf("expected *Error, got %T (%v)", err, err)
	}
	if ge.Code == 0 {
		t.Error("the non-zero exit code should be recorded")
	}
	if !strings.Contains(ge.Error(), "git rev-parse") {
		t.Errorf("the error should include the command: %s", ge.Error())
	}
}

func isHex40(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}
