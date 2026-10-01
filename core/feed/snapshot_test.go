// SPDX-License-Identifier: Apache-2.0

package feed

import (
	"context"
	"strings"
	"testing"

	"immulog/core/gitx"
)

// ── Snapshots ───────────────────────────────────────────────────

func TestCaptureIsDeterministic(t *testing.T) {
	repo, _ := node(t, "alice")
	ctx := context.Background()
	mustSend(t, storeOf(t, repo, "alice"), "one")

	a, err := Capture(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := Capture(ctx, repo)
	if a.Digest != b.Digest {
		t.Fatalf("the digest must be stable for the same state: %q vs %q", a.Digest, b.Digest)
	}
	if len(a.Digest) != 40 {
		t.Fatalf("the digest should be a git object name, got %q", a.Digest)
	}
	if len(a.Refs) != 1 || a.Refs[0].OID == "" {
		t.Fatalf("the snapshot should list every feed anchor: %+v", a.Refs)
	}
}

func TestCaptureChangesWhenFeedAdvances(t *testing.T) {
	repo, _ := node(t, "alice")
	ctx := context.Background()
	s := storeOf(t, repo, "alice")

	mustSend(t, s, "one")
	before, _ := Capture(ctx, repo)
	mustSend(t, s, "two")
	after, _ := Capture(ctx, repo)

	if before.Digest == after.Digest {
		t.Fatal("the digest must change once the tip advances -- otherwise snapshots mean nothing")
	}
}

// An empty repository still needs a stable digest (the empty blob's object name), not an empty string.
func TestCaptureOnEmptyRepoIsStable(t *testing.T) {
	repo, _ := node(t, "empty")
	a, err := Capture(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if a.Digest == "" || len(a.Refs) != 0 {
		t.Fatalf("the empty-repository digest has the wrong shape: %+v", a)
	}
}

// The canonical text must sort by ref name -- otherwise a change in ref order changes the digest and cross-node comparison means nothing.
func TestSnapshotTextIsCanonicallyOrdered(t *testing.T) {
	repo, _ := node(t, "alice")
	ctx := context.Background()
	mustSend(t, storeOf(t, repo, "alice"), "one")
	mustSend(t, storeOf(t, repo, "bob"), "two")
	mustSend(t, storeOf(t, repo, "carol"), "three")

	snap, _ := Capture(ctx, repo)
	if len(snap.Refs) != 3 {
		t.Fatalf("expected 3 feeds, got %d", len(snap.Refs))
	}
	for i := 1; i < len(snap.Refs); i++ {
		if snap.Refs[i-1].Name >= snap.Refs[i].Name {
			t.Fatalf("refs must be ascending by name: %v", snap.Refs)
		}
	}
	// The text and Refs must agree -- the digest is computed over that text
	for _, r := range snap.Refs {
		if !strings.Contains(snap.Text, r.Name+" "+r.OID) {
			t.Fatalf("the canonical text is missing %s: %q", r.Name, snap.Text)
		}
	}
}

// ── Consistency comparison ──────────────────────────────────────

// One side being behind is not a contradiction -- only tips that are not ancestors of one another count as divergence.
func TestDivergedIgnoresStaleness(t *testing.T) {
	repo, _ := node(t, "alice")
	ctx := context.Background()
	s := storeOf(t, repo, "alice")

	mustSend(t, s, "one")
	old, _ := Capture(ctx, repo)
	mustSend(t, s, "two")
	fresh, _ := Capture(ctx, repo)

	d, err := Diverged(ctx, repo, old, fresh)
	if err != nil {
		t.Fatal(err)
	}
	if len(d) != 0 {
		t.Fatalf("a stale snapshot is not a contradiction, got %v", d)
	}
}

func TestDivergedSpotsParallelChains(t *testing.T) {
	repo, dir := node(t, "alice")
	ctx := context.Background()
	s := storeOf(t, repo, "alice")
	ref := FeedRef(s.Pub())

	first := mustSend(t, s, "starting point")
	mustSend(t, s, "the honest one")
	mine, _ := Capture(ctx, repo)

	// The attacker starts a parallel chain; pull the objects locally (comparison needs them present)
	liarDir := liar(t, dir, ref, first.OID)
	rawGit(t, dir, "fetch", "--quiet", liarDir, "+refs/feeds/*:refs/quarantine/x/*")
	forged := rawGit(t, dir, "rev-parse", "refs/quarantine/x/"+s.Pub())

	theirs := Snapshot{Refs: []gitx.RefInfo{{Name: ref, OID: forged}}}
	d, err := Diverged(ctx, repo, mine, theirs)
	if err != nil {
		t.Fatal(err)
	}
	if len(d) != 1 || d[0] != ref {
		t.Fatalf("a parallel chain must be judged as divergence, got %v", d)
	}
}

// With objects missing it must not guess -- it should error rather than quietly say "consistent".
func TestDivergedErrorsWhenObjectMissing(t *testing.T) {
	repo, _ := node(t, "alice")
	ctx := context.Background()
	s := storeOf(t, repo, "alice")
	mustSend(t, s, "one")
	mine, _ := Capture(ctx, repo)

	theirs := Snapshot{Refs: []gitx.RefInfo{
		{Name: FeedRef(s.Pub()), OID: strings.Repeat("a", 40)}, // an object that does not exist locally
	}}
	if _, err := Diverged(ctx, repo, mine, theirs); err == nil {
		t.Fatal("a missing object should error, not pretend the comparison succeeded")
	}
}
