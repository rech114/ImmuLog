// SPDX-License-Identifier: Apache-2.0

package feed

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"immulog/core/gitx"
)

// ── Test fixtures ───────────────────────────────────────────────

func newStore(t *testing.T) (*Store, string) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")

	dir := filepath.Join(t.TempDir(), "repo.git")
	if err := gitx.Init(context.Background(), dir); err != nil {
		t.Fatalf("Init: %v", err)
	}
	rawGit(t, dir, "config", "user.name", "alice")
	rawGit(t, dir, "config", "user.email", "alice@example.com")

	repo := gitx.Open(dir)
	s, err := New(context.Background(), repo, FeedID("alice\x00alice@example.com\x00"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, dir
}

// rawGit simulates an **external attacker**: bypassing our code and operating on
// the repository directly. That is how the force push in the threat model happens,
func rawGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return rawGitIn(t, dir, "", args...)
}

func rawGitIn(t *testing.T, dir, stdin string, args ...string) string {
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
	return strings.TrimSpace(string(out))
}

func mustSend(t *testing.T, s *Store, body string) Message {
	t.Helper()
	m, err := s.Send(context.Background(), body)
	if err != nil {
		t.Fatalf("Send(%q): %v", body, err)
	}
	return m
}

// so the tests have to take the same path.

func TestSendIncrementsSeqFromOne(t *testing.T) {
	s, _ := newStore(t)
	for want := 1; want <= 3; want++ {
		m := mustSend(t, s, "message "+string(rune('0'+want)))
		if m.Seq != want {
			t.Fatalf("Seq = %d, expected %d", m.Seq, want)
		}
		if len(m.OID) != 40 {
			t.Fatalf("malformed OID: %q", m.OID)
		}
		if m.Author != "alice" {
			t.Fatalf("the author should come from git config, got %q", m.Author)
		}
	}
}

// Regression guard: every message trailer must be parseable by git.
// An empty body once made the commit message start with a blank line, which made Seq read back as an empty string.
func TestEveryMessageHasParseableSeqTrailer(t *testing.T) {
	s, dir := newStore(t)
	mustSend(t, s, "plain")
	mustSend(t, s, "multi\nsecond line")
	target := mustSend(t, s, "to be retracted")

	// Each message Seq must read back verbatim through git: 1,2,3,4
	ref := FeedRef(s.Pub())
	out := rawGit(t, dir, "log", "--format=%(trailers:key=ImmuLog-Seq,valueonly)", ref)
	var seqs []string
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if v := strings.TrimSpace(l); v != "" {
			seqs = append(seqs, v)
		}
	}
	if len(seqs) != 3 {
		t.Fatalf("all 3 messages should carry a parseable Seq, actually %v (out=%q)", seqs, out)
	}

	// The retraction event likewise
	if _, err := s.Retract(context.Background(), target.OID, ""); err != nil {
		t.Fatal(err)
	}
	got, _ := s.History(context.Background(), 10)
	for i, m := range got {
		if m.Seq == 0 {
			t.Fatalf("message %d has no parseable Seq: %+v", i, m)
		}
	}
}

func TestHistoryIsNewestFirstAndBodyIsClean(t *testing.T) {
	s, _ := newStore(t)
	mustSend(t, s, "first")
	mustSend(t, s, "second")

	got, err := s.History(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 back, got %d", len(got))
	}
	if got[0].Body != "second" || got[1].Body != "first" {
		t.Fatalf("newest should come first: %q, %q", got[0].Body, got[1].Body)
	}
	// The trailer block must be stripped; no metadata may remain in the body
	for _, m := range got {
		if strings.Contains(m.Body, "ImmuLog-") {
			t.Errorf("trailer left in the body: %q", m.Body)
		}
		if m.Kind != KindMsg {
			t.Errorf("Kind = %q, expected msg", m.Kind)
		}
	}
}

func TestMultiLineBodyRoundTrips(t *testing.T) {
	s, _ := newStore(t)
	body := "first line\nsecond line\n\nfourth line"
	mustSend(t, s, body)

	got, _ := s.History(context.Background(), 1)
	if len(got) != 1 || got[0].Body != body {
		t.Fatalf("the multi-line body did not round trip: %q", got[0].Body)
	}
}

// A forged trailer inside the body must not take effect -- git takes the last occurrence.
func TestFakeTrailersInBodyAreInert(t *testing.T) {
	s, _ := newStore(t)
	m := mustSend(t, s, "body\n\nImmuLog-Seq: 999\nImmuLog-Retracts: "+strings.Repeat("f", 40))

	got, _ := s.History(context.Background(), 1)
	if got[0].Seq != 1 {
		t.Fatalf("the injected Seq took effect: %d", got[0].Seq)
	}
	if got[0].Kind != KindMsg {
		t.Fatalf("the injected Retracts changed the Kind: %q", got[0].Kind)
	}
	if !strings.Contains(got[0].Body, "ImmuLog-Seq: 999") {
		t.Errorf("the body should be kept verbatim (just inert): %q", got[0].Body)
	}
	_ = m
}

func TestSendRejectsTooLong(t *testing.T) {
	s, _ := newStore(t)
	_, err := s.Send(context.Background(), strings.Repeat("x", MaxBody+1))
	if !errors.Is(err, ErrTooLong) {
		t.Fatalf("expected ErrTooLong, got %v", err)
	}
}

// An empty body must be rejected: otherwise the commit message starts with a blank line and git's trailer parsing breaks.
func TestSendRejectsEmptyBody(t *testing.T) {
	s, _ := newStore(t)
	for _, body := range []string{"", "   ", "\n\n", "\t"} {
		if _, err := s.Send(context.Background(), body); !errors.Is(err, ErrEmpty) {
			t.Fatalf("Send(%q) should return ErrEmpty, got %v", body, err)
		}
	}
	if n, _ := s.repo.Count(context.Background(), s.ref); n != 0 {
		t.Fatalf("a rejected message must not be stored; chain length = %d", n)
	}
}

// ── Retraction ──────────────────────────────────────────────────

// Retraction is an **appended event**: the chain grows and the original object stays.
func TestRetractIsAppendNotDelete(t *testing.T) {
	s, _ := newStore(t)
	target := mustSend(t, s, "something I got wrong")

	r, err := s.Retract(context.Background(), target.OID, "wrong channel")
	if err != nil {
		t.Fatalf("Retract: %v", err)
	}
	if r.Kind != KindRetract || r.Retracts != target.OID {
		t.Fatalf("malformed retraction event: %+v", r)
	}

	got, _ := s.History(context.Background(), 10)
	if len(got) != 2 {
		t.Fatalf("the chain should grow to 2, got %d", len(got))
	}
	// The retracted message must still be readable
	var found bool
	for _, m := range got {
		if m.OID == target.OID && m.Body == "something I got wrong" {
			found = true
		}
	}
	if !found {
		t.Fatal("the original message was deleted -- this breaks the design rule (retract = append, never delete)")
	}
}

func TestRetractRequiresTarget(t *testing.T) {
	s, _ := newStore(t)
	if _, err := s.Retract(context.Background(), "", "x"); !errors.Is(err, ErrNoTarget) {
		t.Fatal("a missing target should return ErrNoTarget")
	}
}

// A retraction target is a string from a request body and must be validated strictly -- otherwise it is another injection surface.
func TestRetractRejectsMalformedTarget(t *testing.T) {
	s, _ := newStore(t)
	mustSend(t, s, "placeholder")
	for _, bad := range []string{
		"not-an-oid",
		strings.Repeat("z", 40),
		strings.Repeat("a", 39),
		strings.Repeat("a", 41),
		"aaaa\nImmuLog-Seq: 999",
	} {
		if _, err := s.Retract(context.Background(), bad, "x"); !errors.Is(err, ErrBadTarget) {
			t.Fatalf("Retract(%q) should return ErrBadTarget, got %v", bad, err)
		}
	}
}

// You may only retract messages in your own feed -- retraction is the author's right.
func TestRetractOnlyOwnMessages(t *testing.T) {
	repo, _ := node(t, "alice")
	ctx := context.Background()
	me := storeOf(t, repo, "alice")
	other := storeOf(t, repo, "bob")

	mine := mustSend(t, me, "what I said")
	theirs := mustSend(t, other, "what he said")

	if _, err := me.Retract(ctx, theirs.OID, "out of bounds"); !errors.Is(err, ErrNotMine) {
		t.Fatalf("must not retract a message in someone else's feed, got %v", err)
	}
	// A completely non-existent object is refused too
	if _, err := me.Retract(ctx, strings.Repeat("a", 40), "out of thin air"); !errors.Is(err, ErrNotMine) {
		t.Fatalf("a non-existent target should be refused, got %v", err)
	}
	// Your own is fine
	if _, err := me.Retract(ctx, mine.OID, "mine"); err != nil {
		t.Fatalf("retracting your own message should succeed: %v", err)
	}
}

// A retraction reason comes from a request body; one newline forges a trailer -- and git takes the last occurrence, so the forgery wins.
func TestRetractReasonCannotInjectTrailers(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	target := mustSend(t, s, "original message")

	nasty := "ok\nImmuLog-Seq: 999\nImmuLog-Retracts: " + strings.Repeat("f", 40) + "\n"
	if _, err := s.Retract(ctx, target.OID, nasty); err != nil {
		t.Fatalf("Retract: %v", err)
	}

	got, _ := s.History(ctx, 10)
	if len(got) != 2 {
		t.Fatalf("expected 2, got %d", len(got))
	}
	ev := got[0]
	if ev.Kind != KindRetract {
		t.Fatalf("the tip should be a retraction event: %+v", ev)
	}
	if ev.Seq != 2 {
		t.Fatalf("the injection took effect: Seq = %d, expected 2", ev.Seq)
	}
	if ev.Retracts != target.OID {
		t.Fatalf("the injected Retracts overwrote the real value: %q", ev.Retracts)
	}
	if len(ev.Retracts) != 40 {
		t.Fatalf("malformed Retracts: %q", ev.Retracts)
	}
}

// ── Integrity: witness anchors and reference rewrites ───────────

func TestVerifyOKAfterNormalAppend(t *testing.T) {
	s, _ := newStore(t)
	mustSend(t, s, "a")
	mustSend(t, s, "b")

	v, err := s.Verify(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !v.OK {
		t.Fatalf("a normal append should verify: %+v", v)
	}
	if v.Witness != v.Current {
		t.Fatalf("the witness anchor should have advanced to the tip: %s vs %s", v.Witness, v.Current)
	}
}

func TestVerifyDetectsRewrite(t *testing.T) {
	s, dir := newStore(t)
	mustSend(t, s, "a")
	b := mustSend(t, s, "b")

	// External attacker: start a parallel chain from a, then force the ref across
	parent := rawGit(t, dir, "rev-parse", b.OID+"^")
	tree := rawGit(t, dir, "hash-object", "-w", "-t", "tree", "--stdin")
	forged := rawGitIn(t, dir, "rewritten history\n\nImmuLog-Kind: msg\nImmuLog-Seq: 2\n",
		"commit-tree", tree, "-p", parent)
	if forged == b.OID {
		t.Fatal("precondition: the forgery should be a different object")
	}
	rawGit(t, dir, "update-ref", FeedRef(s.Pub()), forged)

	v, err := s.Verify(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if v.OK {
		t.Fatal("a rewritten reference must verify as inconsistent -- it is the entire reason this project exists")
	}
	if v.Reason != ReasonRewrite {
		t.Fatalf("reason should be %q, got %q", ReasonRewrite, v.Reason)
	}
	if v.Witness != b.OID {
		t.Errorf("the witness anchor must not be dragged along by the attacker: %s", v.Witness)
	}
}

func TestVerifyDetectsRollback(t *testing.T) {
	s, dir := newStore(t)
	a := mustSend(t, s, "a")
	b := mustSend(t, s, "b")

	// External attacker: point the tip back at an earlier commit
	rawGit(t, dir, "update-ref", FeedRef(s.Pub()), a.OID)

	v, err := s.Verify(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if v.OK {
		t.Fatal("a rollback must verify as inconsistent")
	}
	if v.Reason != ReasonRollback {
		t.Fatalf("reason should be %q, got %q", ReasonRollback, v.Reason)
	}
	if v.Witness != b.OID {
		t.Errorf("the witness anchor should still sit at b: %s", v.Witness)
	}
}

// An attacker cannot see or alter the witness anchor: it is outside the refs/feeds namespace.
func TestWitnessLivesOutsideFeedNamespace(t *testing.T) {
	s, dir := newStore(t)
	mustSend(t, s, "a")

	w, err := s.Witness(context.Background())
	if err != nil || w == "" {
		t.Fatalf("the witness anchor should exist: %q %v", w, err)
	}

	wrefs := strings.Fields(rawGit(t, dir, "for-each-ref", "--format=%(refname)", "refs/witness/"))
	if len(wrefs) != 1 || wrefs[0] != "refs/witness/"+s.Pub() {
		t.Fatalf("the witness anchor should live at refs/witness/%s, actually %v", s.Pub(), wrefs)
	}

	// The key property: whoever syncs only refs/feeds never obtains the witness anchor
	feeds := rawGit(t, dir, "for-each-ref", "--format=%(refname)", "refs/feeds/")
	if strings.Contains(feeds, "witness") {
		t.Fatalf("the witness anchor must not appear under refs/feeds: %q", feeds)
	}
	if strings.TrimSpace(feeds) != FeedRef(s.Pub()) {
		t.Fatalf("refs/feeds should hold exactly this machine's feed, actually %q", feeds)
	}
}

// ── Serialisation ───────────────────────────────────────────────

func TestConcurrentSendsAreSerialized(t *testing.T) {
	s, _ := newStore(t)
	const n = 24

	var wg sync.WaitGroup
	errs := make([]error, n)
	seqs := make([]int, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			m, err := s.Send(context.Background(), "concurrent "+string(rune('a'+i%26)))
			errs[i], seqs[i] = err, m.Seq
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent write %d failed: %v", i, err)
		}
	}
	sort.Ints(seqs)
	for i, got := range seqs {
		if got != i+1 {
			t.Fatalf("sequence numbers should be exactly 1..%d with no repeats, got %v", n, seqs)
		}
	}
	if v, _ := s.Verify(context.Background()); !v.OK {
		t.Fatal("the chain should still be consistent after concurrent writes")
	}
}

// ── Derivation ───────────────────────────────────────────────────

// FeedID output must be ref-safe: no character of an identity may become a path traversal.
func TestFeedIDIsRefSafe(t *testing.T) {
	for _, seed := range []string{
		"alice\x00a@b\x00",
		"../../etc/passwd",
		"a b\tc\nd",
		"'; rm -rf / #",
		"refs/heads/main",
		"",
	} {
		id := FeedID(seed)
		if len(id) != 16 {
			t.Fatalf("FeedID(%q) should be 16 chars, got %d", seed, len(id))
		}
		for _, c := range id {
			if !strings.ContainsRune("0123456789abcdef", c) {
				t.Fatalf("FeedID(%q) = %q contains non-hex characters", seed, id)
			}
		}
	}
	// Different identities must yield different feeds
	if FeedID("a") == FeedID("b") {
		t.Fatal("different identities must not share a feed")
	}
}

// Commits are not signed by default; configuring user.signingkey turns it on.
func TestSigningOffByDefault(t *testing.T) {
	s, _ := newStore(t)
	if s.Signed() {
		t.Fatal("with no user.signingkey it must not sign")
	}
}
