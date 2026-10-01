// SPDX-License-Identifier: Apache-2.0

package feed

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// recordingPublisher records the digests handed to an external service,
// standing in for a notary.
type recordingPublisher struct {
	got  []string
	body string
	err  error
}

func (p *recordingPublisher) Publish(_ context.Context, digest string) (string, error) {
	p.got = append(p.got, digest)
	if p.err != nil {
		return "", p.err
	}
	return p.body, nil
}

// ── The anchor chain ────────────────────────────────────────────

// Each anchor's parent is the previous one: rewriting an anchor in the middle
// requires rewriting every anchor after it.
func TestAnchorChainLinksToPrevious(t *testing.T) {
	repo, _ := node(t, "alice")
	ctx := context.Background()
	s := storeOf(t, repo, "alice")

	mustSend(t, s, "one")
	a1, err := AnchorNow(ctx, repo, nil, false)
	if err != nil {
		t.Fatalf("AnchorNow: %v", err)
	}
	if a1.Seq != 1 || a1.Prev != "" {
		t.Fatalf("the first anchor has the wrong shape: %+v", a1)
	}

	mustSend(t, s, "two")
	a2, err := AnchorNow(ctx, repo, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if a2.Seq != 2 || a2.Prev != a1.OID {
		t.Fatalf("the second anchor should point at the first: %+v", a2)
	}
	// Confirm at the git level that the parent link really landed
	if parent := rawGit(t, repo.Dir, "rev-parse", a2.OID+"^"); parent != a1.OID {
		t.Fatalf("the git parent should be %s, got %s", a1.OID, parent)
	}
	// State changed, so the digest must change with it
	if a1.Snapshot == a2.Snapshot {
		t.Fatal("the snapshot digest must change once the tip advances")
	}
}

func TestAnchorHeadReadsBackLatest(t *testing.T) {
	repo, _ := node(t, "alice")
	ctx := context.Background()
	mustSend(t, storeOf(t, repo, "alice"), "one")

	// Never anchored -> the zero value, not an error
	if a, err := AnchorHead(ctx, repo); err != nil || a.OID != "" {
		t.Fatalf("with no anchor it should return the zero value: %+v %v", a, err)
	}

	want, _ := AnchorNow(ctx, repo, nil, false)
	got, err := AnchorHead(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if got.OID != want.OID || got.Snapshot != want.Snapshot || got.Seq != 1 {
		t.Fatalf("the anchor read back does not match: %+v vs %+v", got, want)
	}
	if got.At.IsZero() {
		t.Error("the timestamp should parse back out of the trailer")
	}
}

// The snapshot content itself must land in the anchor commit, so anyone who
// obtains that object later can recompute the digest.
func TestAnchorCarriesSnapshotContent(t *testing.T) {
	repo, _ := node(t, "alice")
	ctx := context.Background()
	s := storeOf(t, repo, "alice")
	mustSend(t, s, "one")

	a, _ := AnchorNow(ctx, repo, nil, false)
	body := rawGit(t, repo.Dir, "log", "-1", "--format=%B", a.OID)

	if !strings.Contains(body, FeedRef(s.Pub())) {
		t.Fatalf("the anchor body should contain the feed list, actually: %q", body)
	}
	// Recomputing the digest must match the anchored one -- that is what
	// "recomputable" means
	snap, _ := Capture(ctx, repo)
	if snap.Digest != a.Snapshot {
		t.Fatalf("recomputed digest differs: %q vs %q", snap.Digest, a.Snapshot)
	}
}

// ── External anchoring ──────────────────────────────────────────

func TestAnchorPublishesExternally(t *testing.T) {
	repo, _ := node(t, "alice")
	ctx := context.Background()
	mustSend(t, storeOf(t, repo, "alice"), "one")

	pub := &recordingPublisher{body: "ots-receipt-xyz"}
	a, err := AnchorNow(ctx, repo, pub, false)
	if err != nil {
		t.Fatal(err)
	}

	if len(pub.got) != 1 {
		t.Fatalf("the external service should be called once, actually %d times", len(pub.got))
	}
	if pub.got[0] != a.Snapshot {
		t.Fatalf("what is handed out should be the snapshot digest: %q vs %q", pub.got[0], a.Snapshot)
	}
	if a.External != "ots-receipt-xyz" {
		t.Fatalf("the receipt should be recorded: %q", a.External)
	}

	// The receipt must land on the chain and stay auditable -- that is the point
	// of external anchoring
	vals, err := repo.TrailerValue(ctx, a.OID, "ImmuLog-External")
	if err != nil {
		t.Fatal(err)
	}
	if vals[0] != "ots-receipt-xyz" {
		t.Fatalf("the receipt did not land on the chain: %q", vals[0])
	}
}

// An unavailable external service must not block local anchoring: the local
// chain already has value.
func TestAnchorSurvivesPublisherFailure(t *testing.T) {
	repo, _ := node(t, "alice")
	ctx := context.Background()
	mustSend(t, storeOf(t, repo, "alice"), "one")

	pub := &recordingPublisher{err: errors.New("503 service unavailable")}
	a, err := AnchorNow(ctx, repo, pub, false)
	if err != nil {
		t.Fatalf("an external failure must not fail the anchoring: %v", err)
	}
	if a.External != "" {
		t.Fatalf("no receipt should be recorded on failure: %q", a.External)
	}
	if a.OID == "" || a.Snapshot == "" {
		t.Fatalf("the local anchor should still hold: %+v", a)
	}
}

// With no external service configured, anchoring still works -- it is just not
// external.
func TestAnchorWithoutPublisher(t *testing.T) {
	repo, _ := node(t, "alice")
	ctx := context.Background()
	mustSend(t, storeOf(t, repo, "alice"), "one")

	a, err := AnchorNow(ctx, repo, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if a.External != "" || a.OID == "" {
		t.Fatalf("with no external service there should still be a local anchor: %+v", a)
	}
}

// Control characters in a receipt must not break the trailer structure.
func TestAnchorSanitizesReceipt(t *testing.T) {
	repo, _ := node(t, "alice")
	ctx := context.Background()
	mustSend(t, storeOf(t, repo, "alice"), "one")

	pub := &recordingPublisher{body: "ok\x1f\x1e\nImmuLog-Snapshot: forged"}
	a, err := AnchorNow(ctx, repo, pub, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(a.External, "\x1f\x1e") {
		t.Fatalf("control characters in the receipt should be stripped: %q", a.External)
	}
	// The digest must not be swayed by a forged trailer inside the receipt
	got, _ := AnchorHead(ctx, repo)
	if got.Snapshot != a.Snapshot {
		t.Fatalf("the digest was affected by injection: %q vs %q", got.Snapshot, a.Snapshot)
	}
}
