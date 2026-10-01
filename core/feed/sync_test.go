// SPDX-License-Identifier: Apache-2.0

package feed

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"immulog/core/gitx"
)

// ── Fixtures ────────────────────────────────────────────────────

// node is an independent node: its own repository plus its own feed.
func node(t *testing.T, who string) (*gitx.Repo, string) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")

	dir := filepath.Join(t.TempDir(), who+".git")
	if err := gitx.Init(context.Background(), dir); err != nil {
		t.Fatalf("Init: %v", err)
	}
	rawGit(t, dir, "config", "user.name", who)
	rawGit(t, dir, "config", "user.email", who+"@example.com")
	return gitx.Open(dir), dir
}

// hub is a bare repository acting as the shared relay.
func hub(t *testing.T, name string) (string, Remote) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")

	dir := filepath.Join(t.TempDir(), "hub.git")
	if err := gitx.Init(context.Background(), dir); err != nil {
		t.Fatalf("Init hub: %v", err)
	}
	return dir, Remote{Name: name, URL: dir}
}

// publish fills the hub with src's feeds.
//
// This fetches rather than pushes: the local sandbox's receive-pack is unusable (see the gitx transport tests).
// The production push path is covered by the gitx package on CI; here we only care about sync and verification.
func publish(t *testing.T, hubDir, srcDir string) {
	t.Helper()
	rawGitIn(t, hubDir, "", "fetch", "--quiet", srcDir, "+refs/feeds/*:refs/feeds/*")
}

func storeOf(t *testing.T, repo *gitx.Repo, who string) *Store {
	t.Helper()
	s, err := New(context.Background(), repo, FeedID(who))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// ── Normal sync ─────────────────────────────────────────────────

func TestSyncAdvancesFromRemote(t *testing.T) {
	ctx := context.Background()
	aRepo, aDir := node(t, "alice")
	hubDir, remote := hub(t, "hub")
	bRepo, _ := node(t, "bob")

	a := storeOf(t, aRepo, "alice")
	mustSend(t, a, "the first line")
	mustSend(t, a, "the second line")
	publish(t, hubDir, aDir)

	res, err := Sync(ctx, bRepo, []Remote{remote}, 100)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(res.Advanced) != 2 {
		t.Fatalf("should sync 2 messages, got %d: %+v", len(res.Advanced), res.Advanced)
	}
	// Oldest first, matching chain order
	if res.Advanced[0].Body != "the first line" || res.Advanced[1].Body != "the second line" {
		t.Fatalf("wrong order: %q, %q", res.Advanced[0].Body, res.Advanced[1].Body)
	}
	if res.Advanced[0].Author != "alice" {
		t.Errorf("the author should come from the commit, got %q", res.Advanced[0].Author)
	}
	if len(res.Alarms) != 0 {
		t.Fatalf("a normal sync should raise no alarm: %+v", res.Alarms)
	}
	if res.Reached != 1 || len(res.Peers) != 1 || !res.Peers[0].OK {
		t.Fatalf("wrong peer state: %+v", res.Peers)
	}

	// A foreign feed also has to land in trusted state, with a witness anchor
	ref := FeedRef(FeedID("alice"))
	tip, _ := bRepo.Resolve(ctx, ref)
	if tip == "" {
		t.Fatal("the foreign feed should have landed in refs/feeds/*")
	}
	w, _ := WitnessOf(ctx, bRepo, ref)
	if w != tip {
		t.Fatalf("the witness anchor should advance to the foreign tip: %q vs %q", w, tip)
	}
}

func TestSyncIsIdempotent(t *testing.T) {
	ctx := context.Background()
	aRepo, aDir := node(t, "alice")
	hubDir, remote := hub(t, "hub")
	bRepo, _ := node(t, "bob")

	mustSend(t, storeOf(t, aRepo, "alice"), "just one")
	publish(t, hubDir, aDir)

	first, _ := Sync(ctx, bRepo, []Remote{remote}, 100)
	if len(first.Advanced) != 1 {
		t.Fatalf("the first sync should deliver 1, got %d", len(first.Advanced))
	}
	second, _ := Sync(ctx, bRepo, []Remote{remote}, 100)
	if len(second.Advanced) != 0 {
		t.Fatalf("a second sync must not redeliver, got %d", len(second.Advanced))
	}
}

func TestSyncReportsUnreachablePeer(t *testing.T) {
	bRepo, _ := node(t, "bob")
	bad := Remote{Name: "dead", URL: filepath.Join(t.TempDir(), "nope.git")}

	res, err := Sync(context.Background(), bRepo, []Remote{bad}, 100)
	if err != nil {
		t.Fatalf("one unreachable remote must not make Sync fail: %v", err)
	}
	if res.Reached != 0 {
		t.Fatalf("there should be no reachable remote, got %d", res.Reached)
	}
	if len(res.Peers) != 1 || res.Peers[0].OK || res.Peers[0].Note == "" {
		t.Fatalf("it should report unreachability honestly: %+v", res.Peers)
	}
}

// ── Security: foreign history may not be silently rewritten either ──

func TestSyncRejectsRewrittenForeignHistory(t *testing.T) {
	ctx := context.Background()
	aRepo, aDir := node(t, "alice")
	hubDir, remote := hub(t, "hub")
	bRepo, _ := node(t, "bob")
	aRef := FeedRef(FeedID("alice"))

	a := storeOf(t, aRepo, "alice")
	mustSend(t, a, "one")
	second := mustSend(t, a, "two")
	publish(t, hubDir, aDir)

	if res, _ := Sync(ctx, bRepo, []Remote{remote}, 100); len(res.Alarms) != 0 {
		t.Fatalf("the first sync should raise no alarm: %+v", res.Alarms)
	}
	trusted, _ := bRepo.Resolve(ctx, aRef)
	if trusted != second.OID {
		t.Fatalf("b's trusted tip should be %s, got %q", second.OID, trusted)
	}

	// alice's repository is rewritten by an external attacker: a parallel chain from the first message
	parent := rawGit(t, aDir, "rev-parse", second.OID+"^")
	tree := rawGit(t, aDir, "hash-object", "-w", "-t", "tree", "--stdin")
	forged := rawGitIn(t, aDir, "rewritten\n\nImmuLog-Kind: msg\nImmuLog-Seq: 2\n",
		"commit-tree", tree, "-p", parent)
	rawGit(t, aDir, "update-ref", aRef, forged)
	publish(t, hubDir, aDir)

	res, err := Sync(ctx, bRepo, []Remote{remote}, 100)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(res.Alarms) != 1 {
		t.Fatalf("there should be exactly one alarm, got %d: %+v", len(res.Alarms), res.Alarms)
	}
	if res.Alarms[0].Reason != ReasonRewrite {
		t.Fatalf("the reason should be rewrite, got %q", res.Alarms[0].Reason)
	}
	if res.Alarms[0].Peer != "hub" {
		t.Errorf("the alarm should name its source: %+v", res.Alarms[0])
	}
	if len(res.Advanced) != 0 {
		t.Fatalf("a rewritten commit must not enter the timeline: %+v", res.Advanced)
	}

	// The core assertion: local trusted state and the witness anchor do not budge
	if got, _ := bRepo.Resolve(ctx, aRef); got != trusted {
		t.Fatalf("the local copy must be kept as-is, yet became %q", got)
	}
	if w, _ := WitnessOf(ctx, bRepo, aRef); w != trusted {
		t.Fatalf("the witness anchor must not be dragged along, yet became %q", w)
	}
	if !res.Peers[0].OK == false && res.Peers[0].Note == "" {
		t.Errorf("the peer should be marked abnormal: %+v", res.Peers[0])
	}
}

// liar builds a repository holding a different parallel history.
//
// Clone it, then rewrite the tip -- exactly the equivalent of a force push.
// Note clone does **not** copy user.* config, so the identity has to be set explicitly.
func liar(t *testing.T, srcDir, ref, parent string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "liar.git")
	rawGitIn(t, srcDir, "", "clone", "--bare", "--quiet", srcDir, dir)
	rawGit(t, dir, "config", "user.name", "alice")
	rawGit(t, dir, "config", "user.email", "alice@example.com")

	tree := rawGit(t, dir, "hash-object", "-w", "-t", "tree", "--stdin")
	forged := rawGitIn(t, dir, "another history\n\nImmuLog-Kind: msg\nImmuLog-Seq: 2\n",
		"commit-tree", tree, "-p", parent)
	rawGit(t, dir, "update-ref", ref, forged)
	return dir
}

// Two remotes giving tips for the same feed that are not ancestors of one another => a split view.
func TestSyncDetectsSplitView(t *testing.T) {
	ctx := context.Background()
	aRepo, aDir := node(t, "alice")
	h1Dir, r1 := hub(t, "h1")
	h2Dir, r2 := hub(t, "h2")
	bRepo, _ := node(t, "bob")
	aRef := FeedRef(FeedID("alice"))

	a := storeOf(t, aRepo, "alice")
	first := mustSend(t, a, "a shared starting point")
	honest := mustSend(t, a, "the honest history")
	liarDir := liar(t, aDir, aRef, first.OID)

	publish(t, h1Dir, aDir)    // h1 says: the tip is honest
	publish(t, h2Dir, liarDir) // h2 says: the tip is forged

	res, err := Sync(ctx, bRepo, []Remote{r1, r2}, 100)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(res.Alarms) != 1 || res.Alarms[0].Reason != ReasonSplit {
		t.Fatalf("a split view should be reported, got %+v", res.Alarms)
	}
	if res.Reached != 2 {
		t.Errorf("both remotes should be reachable: %+v", res.Peers)
	}
	_ = honest
}

// The alarm must carry two distinct anchors, or the user cannot gather evidence.
func TestSplitAlarmCarriesBothAnchors(t *testing.T) {
	ctx := context.Background()
	aRepo, aDir := node(t, "alice")
	h1Dir, r1 := hub(t, "h1")
	h2Dir, r2 := hub(t, "h2")
	bRepo, _ := node(t, "bob")
	aRef := FeedRef(FeedID("alice"))

	a := storeOf(t, aRepo, "alice")
	first := mustSend(t, a, "starting point")
	mustSend(t, a, "honest")
	liarDir := liar(t, aDir, aRef, first.OID)

	publish(t, h1Dir, aDir)
	publish(t, h2Dir, liarDir)

	res, _ := Sync(ctx, bRepo, []Remote{r1, r2}, 100)
	if len(res.Alarms) != 1 {
		t.Fatalf("there should be one alarm: %+v", res.Alarms)
	}
	v := res.Alarms[0]
	if v.Witness == "" || v.Current == "" || v.Witness == v.Current {
		t.Fatalf("the alarm should carry two different anchors: %+v", v)
	}
	if !strings.HasPrefix(v.Feed, FeedPrefix) {
		t.Errorf("the alarm should name which feed it concerns: %+v", v)
	}
}
