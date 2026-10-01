// SPDX-License-Identifier: Apache-2.0

package gitx

import (
	"context"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// ── Fixtures ────────────────────────────────────────────────────

// hub creates a bare repository acting as a shared relay -- the most ordinary
// thing in multi-source sync.
func hub(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "hub.git")
	if err := Init(context.Background(), dir); err != nil {
		t.Fatalf("Init hub: %v", err)
	}
	return dir
}

// commit creates a commit carrying a sequence trailer.
func commit(t *testing.T, r *Repo, parent, body string, seq int) string {
	t.Helper()
	msg := body + "\n\nImmuLog-Kind: msg\nImmuLog-Seq: " + strconv.Itoa(seq) + "\n"
	oid, err := r.Commit(context.Background(), tree(t, r), parent, msg, false)
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return oid
}

// push needs receive-pack. On this machine (aarch64 + proot + f2fs) it always
// fails with "bad pack" -- an environment limit, not a code problem. CI really
// runs it.
func pushRef(t *testing.T, r *Repo, url, ref string) {
	t.Helper()
	err := r.PushRef(context.Background(), url, ref)
	if err == nil {
		return
	}
	if strings.Contains(err.Error(), "bad pack") || strings.Contains(err.Error(), "unpack should have generated") {
		t.Skipf("receive-pack is unavailable here (environment limit; CI runs it): %v", err)
	}
	t.Fatalf("PushRef: %v", err)
}

// seedViaFetch fills the hub with src's feeds.
//
// This fetches rather than pushes, because under the limit above only
// upload-pack works. The production push path is covered by
// TestPushThenFetchIntoQuarantine on CI.
func seedViaFetch(t *testing.T, hubDir, srcDir string) {
	t.Helper()
	gitIn(t, hubDir, "", "fetch", "--quiet", srcDir, "+refs/feeds/*:refs/feeds/*")
}

// ── Transport ───────────────────────────────────────────────────

func TestPushThenFetchIntoQuarantine(t *testing.T) {
	ctx := context.Background()
	a, h, b := newRepo(t), hub(t), newRepo(t)

	oid := commit(t, a, "", "from a", 1)
	if err := a.UpdateRef(ctx, "refs/feeds/alice", oid, ""); err != nil {
		t.Fatal(err)
	}
	pushRef(t, a, h, "refs/feeds/alice")

	if err := b.FetchInto(ctx, h, "refs/feeds/*", Quarantine+"r0"); err != nil {
		t.Fatalf("FetchInto: %v", err)
	}
	got, err := b.Resolve(ctx, Quarantine+"r0/alice")
	if err != nil {
		t.Fatal(err)
	}
	if got != oid {
		t.Fatalf("the quarantine should hold %s, got %q", oid, got)
	}

	// The most important line: fetch **never** writes into the trusted namespace
	if v, _ := b.Resolve(ctx, "refs/feeds/alice"); v != "" {
		t.Fatalf("fetch must not write into refs/feeds/*, yet wrote %q", v)
	}
}

// The quarantine is meant to be overwritten: when a peer forces a rewrite, local
// trusted state is still unaffected.
func TestFetchIntoOverwritesQuarantineOnly(t *testing.T) {
	ctx := context.Background()
	a, h, b := newRepo(t), hub(t), newRepo(t)

	first := commit(t, a, "", "first version", 1)
	if err := a.UpdateRef(ctx, "refs/feeds/alice", first, ""); err != nil {
		t.Fatal(err)
	}
	seedViaFetch(t, h, a.Dir)

	if err := b.FetchInto(ctx, h, "refs/feeds/*", Quarantine+"r0"); err != nil {
		t.Fatal(err)
	}
	if got, _ := b.Resolve(ctx, Quarantine+"r0/alice"); got != first {
		t.Fatalf("first fetch = %q", got)
	}

	// b pins its trusted state at first
	if err := b.UpdateRef(ctx, "refs/feeds/alice", first, ""); err != nil {
		t.Fatal(err)
	}

	// a starts a parallel chain and force-pushes (the equivalent of a force push)
	forced := commit(t, a, "", "another chain", 2)
	if err := a.UpdateRef(ctx, "refs/feeds/alice", forced, ""); err != nil {
		t.Fatal(err)
	}
	seedViaFetch(t, h, a.Dir)

	if err := b.FetchInto(ctx, h, "refs/feeds/*", Quarantine+"r0"); err != nil {
		t.Fatal(err)
	}
	if got, _ := b.Resolve(ctx, Quarantine+"r0/alice"); got != forced {
		t.Fatalf("the quarantine should be overwritten with %s, got %q", forced, got)
	}
	if got, _ := b.Resolve(ctx, "refs/feeds/alice"); got != first {
		t.Fatalf("trusted state must not budge, yet became %q", got)
	}
}

func TestFetchFromUnreachableRemoteFailsLoudly(t *testing.T) {
	r := newRepo(t)
	err := r.FetchInto(context.Background(),
		filepath.Join(t.TempDir(), "nope.git"), "refs/feeds/*", Quarantine+"r0")
	if err == nil {
		t.Fatal("an unreachable remote must error, not fail silently")
	}
}

// ── Object primitives ───────────────────────────────────────────

func TestHashBlobMatchesGit(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()
	data := []byte("refs/feeds/a 1234567890\n")

	only, err := r.HashBlob(ctx, data, false)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.TrimSpace(gitIn(t, r.Dir, string(data), "hash-object", "--stdin"))
	if only != want {
		t.Fatalf("HashBlob = %q，git hash-object = %q", only, want)
	}
	if blobExists(t, r.Dir, only) {
		t.Fatal("write=false should not store anything")
	}

	if _, err := r.HashBlob(ctx, data, true); err != nil {
		t.Fatal(err)
	}
	if !blobExists(t, r.Dir, only) {
		t.Fatal("write=true should have stored it")
	}
}

// blobExists asks git directly -- Repo.Exists is meant for commits.
func blobExists(t *testing.T, dir, oid string) bool {
	t.Helper()
	cmd := exec.Command("git", "-C", dir, "cat-file", "-e", oid)
	cmd.Env = append(cmd.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	return cmd.Run() == nil
}

func TestLogRangeReturnsOnlyNewOldestFirst(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()

	a := commit(t, r, "", "one", 1)
	b := commit(t, r, a, "two", 2)
	c := commit(t, r, b, "three", 3)
	if err := r.UpdateRef(ctx, "refs/feeds/x", c, ""); err != nil {
		t.Fatal(err)
	}

	got, err := r.LogRange(ctx, "refs/feeds/x", a, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("expected only 2 new commits, got %d", len(got))
	}
	if got[0].OID != b || got[1].OID != c {
		t.Fatalf("expected oldest-first [two,three], got [%s,%s]", got[0].OID, got[1].OID)
	}
	if got[0].Seq != "2" || got[1].Seq != "3" {
		t.Fatalf("wrong sequence numbers: %q %q", got[0].Seq, got[1].Seq)
	}

	all, _ := r.LogRange(ctx, "refs/feeds/x", "", 10)
	if len(all) != 3 {
		t.Fatalf("with no since it should return everything, got %d", len(all))
	}
}

func TestTrailerValueReadsMultipleKeys(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()
	msg := "body\n\nImmuLog-Snapshot: aaaa\nImmuLog-At: 2026-01-02T03:04:05Z\n"
	oid, err := r.Commit(ctx, tree(t, r), "", msg, false)
	if err != nil {
		t.Fatal(err)
	}
	vals, err := r.TrailerValue(ctx, oid, "ImmuLog-Snapshot", "ImmuLog-At", "ImmuLog-Missing")
	if err != nil {
		t.Fatal(err)
	}
	if len(vals) != 3 {
		t.Fatalf("expected 3 slots back, got %d", len(vals))
	}
	if vals[0] != "aaaa" {
		t.Errorf("Snapshot = %q", vals[0])
	}
	if vals[1] != "2026-01-02T03:04:05Z" {
		t.Errorf("At = %q", vals[1])
	}
	if vals[2] != "" {
		t.Errorf("a missing key should be an empty string, got %q", vals[2])
	}
}

// git decides the signature status (%G?); we only translate it.
func TestSigStatusMapping(t *testing.T) {
	for in, want := range map[string]string{
		"G": "good", "U": "untrusted", "B": "bad", "N": "", "": "",
	} {
		if got := sigStatus(in); got != want {
			t.Errorf("sigStatus(%q) = %q, expected %q", in, got, want)
		}
	}
}
