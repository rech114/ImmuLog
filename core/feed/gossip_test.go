// SPDX-License-Identifier: Apache-2.0

package feed

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"immulog/core/gitx"
)

// ── Fixtures ────────────────────────────────────────────────────

// snapshotServer serves one node's view from the endpoint gossip reads. It
// answers on snapshotPath only, so a test fails loudly if the client ever asks
// for the wrong URL.
func snapshotServer(t *testing.T, snap Snapshot) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != snapshotPath {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"digest": snap.Digest,
			"refs":   snap.Refs,
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// closedServer returns a URL that is guaranteed to refuse connections, so
// "unreachable" is tested without depending on a magic port number.
func closedServer(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()
	return url
}

// viewOf builds a peer's snapshot from a subset of refs, with a syntactically
// valid digest -- the digest is only checked for shape, never trusted.
func viewOf(refs ...gitx.RefInfo) Snapshot {
	return Snapshot{Digest: strings.Repeat("a", 40), Refs: refs}
}

// ── Set-level comparison: the thing per-feed comparison cannot do ──

// A feed only one side knows about is ordinary, not a contradiction -- but it
// must show up, because `Sync` groups claims by ref and would never see it.
func TestCompareSeesAFeedOnlyOneSideKnows(t *testing.T) {
	repo, _ := node(t, "alice")
	ctx := context.Background()
	s := storeOf(t, repo, "alice")
	mustSend(t, s, "one")

	mine, err := Capture(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}

	aliceRef := FeedRef(s.Pub())
	// The peer serves alice and one more feed this node has never heard of.
	stranger := FeedRef(FeedID("bob"))
	theirs := viewOf(
		gitx.RefInfo{Name: aliceRef, OID: mine.tipOf(aliceRef)},
		gitx.RefInfo{Name: stranger, OID: strings.Repeat("b", 40)},
	)

	d, err := Compare(ctx, repo, mine, theirs)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.MissingHere) != 1 || d.MissingHere[0] != stranger {
		t.Fatalf("a feed only the peer knows must be reported, got %+v", d)
	}
	if len(d.Diverged) != 0 {
		t.Fatalf("a feed one side has never seen is not a contradiction, got %+v", d.Diverged)
	}
	if len(d.MissingThere) != 0 {
		t.Fatalf("nothing is missing on the peer's side here, got %+v", d.MissingThere)
	}
}

func TestCompareSeesAFeedThePeerOmits(t *testing.T) {
	repo, _ := node(t, "alice")
	ctx := context.Background()
	mustSend(t, storeOf(t, repo, "alice"), "one")
	mustSend(t, storeOf(t, repo, "bob"), "two")

	mine, _ := Capture(ctx, repo)

	// The peer knows only about a feed this node has never heard of.
	stranger := FeedRef(FeedID("carol"))
	theirs := viewOf(gitx.RefInfo{Name: stranger, OID: strings.Repeat("c", 40)})

	d, err := Compare(ctx, repo, mine, theirs)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.MissingHere) != 1 || d.MissingHere[0] != stranger {
		t.Fatalf("the stranger feed must be reported as unknown here, got %+v", d.MissingHere)
	}
	if len(d.MissingThere) != 2 {
		t.Fatalf("both local feeds are absent on the peer, got %+v", d.MissingThere)
	}
	if len(d.Diverged) != 0 {
		t.Fatalf("unknown objects must not be guessed at as divergence, got %+v", d.Diverged)
	}
}

// A missing object is not a verdict. Compare files it under Unverifiable rather
// than guessing; Diverged keeps refusing to answer at all.
func TestCompareFilesMissingObjectsAsUnverifiable(t *testing.T) {
	repo, _ := node(t, "alice")
	ctx := context.Background()
	s := storeOf(t, repo, "alice")
	mustSend(t, s, "one")
	mine, _ := Capture(ctx, repo)

	ref := FeedRef(s.Pub())
	theirs := viewOf(gitx.RefInfo{Name: ref, OID: strings.Repeat("d", 40)})

	d, err := Compare(ctx, repo, mine, theirs)
	if err != nil {
		t.Fatalf("Compare must not fail on an object it cannot check: %v", err)
	}
	if len(d.Unverifiable) != 1 || d.Unverifiable[0] != ref {
		t.Fatalf("expected the feed to be unverifiable, got %+v", d)
	}
	if len(d.Diverged) != 0 {
		t.Fatalf("an unchecked tip must never be reported as divergence, got %+v", d.Diverged)
	}
	// The strict wrapper keeps its old contract.
	if _, err := Diverged(ctx, repo, mine, theirs); err == nil {
		t.Fatal("Diverged must still refuse to answer when an object is missing")
	}
}

// ── Gossip rounds ───────────────────────────────────────────────

func TestGossipAgreesWithAnIdenticalView(t *testing.T) {
	repo, _ := node(t, "alice")
	ctx := context.Background()
	mustSend(t, storeOf(t, repo, "alice"), "one")

	mine, _ := Capture(ctx, repo)
	peer := snapshotServer(t, mine)

	res, err := Gossip(ctx, repo, []GossipPeer{{Name: "bob", URL: peer.URL}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Reached != 1 || len(res.Peers) != 1 {
		t.Fatalf("expected one reachable peer, got %+v", res)
	}
	p := res.Peers[0]
	if !p.OK || p.Note != "agrees" {
		t.Fatalf("an identical view must be agreement, got %+v", p)
	}
	if p.Digest != mine.Digest || p.Feeds != len(mine.Refs) {
		t.Fatalf("the report should carry the peer's digest and feed count, got %+v", p)
	}
	if len(res.Alarms) != 0 || len(res.Missing) != 0 {
		t.Fatalf("agreement must raise nothing, got %+v", res)
	}
}

// The reason gossip exists: several independent peers report a feed this node
// does not hold, while the local source never mentioned it.
func TestGossipCountsHowManyPeersReportTheSameMissingFeed(t *testing.T) {
	repo, _ := node(t, "alice")
	ctx := context.Background()
	mustSend(t, storeOf(t, repo, "alice"), "one")

	stranger := FeedRef(FeedID("dave"))
	theirs := viewOf(gitx.RefInfo{Name: stranger, OID: strings.Repeat("e", 40)})
	one, two := snapshotServer(t, theirs), snapshotServer(t, theirs)

	res, err := Gossip(ctx, repo, []GossipPeer{
		{Name: "bob", URL: one.URL},
		{Name: "carol", URL: two.URL},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Missing) != 1 {
		t.Fatalf("expected exactly one unheld feed, got %+v", res.Missing)
	}
	if res.Missing[0].Feed != stranger || res.Missing[0].Peers != 2 {
		t.Fatalf("the peer count is the whole point: got %+v", res.Missing[0])
	}
	if len(res.Alarms) != 0 {
		t.Fatalf("a feed only peers know is not tampering, got %+v", res.Alarms)
	}
}

// Two views disagreeing about the same feed is a split view, and that is the
// only thing gossip alarms on.
func TestGossipAlarmsOnASplitView(t *testing.T) {
	repo, dir := node(t, "alice")
	ctx := context.Background()
	s := storeOf(t, repo, "alice")
	ref := FeedRef(s.Pub())

	first := mustSend(t, s, "starting point")
	mustSend(t, s, "the honest one")
	mine, _ := Capture(ctx, repo)

	// The peer serves a parallel chain. Its object must be present locally:
	// a contradiction can only be judged against objects this node holds.
	liarDir := liar(t, dir, ref, first.OID)
	rawGit(t, dir, "fetch", "--quiet", liarDir, "+refs/feeds/*:refs/quarantine/x/*")
	forged := rawGit(t, dir, "rev-parse", "refs/quarantine/x/"+s.Pub())

	peer := snapshotServer(t, viewOf(gitx.RefInfo{Name: ref, OID: forged}))
	res, err := Gossip(ctx, repo, []GossipPeer{{Name: "boris", URL: peer.URL}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Alarms) != 1 {
		t.Fatalf("a parallel chain must raise exactly one alarm, got %+v", res.Alarms)
	}
	a := res.Alarms[0]
	if a.Reason != ReasonSplit || a.Feed != ref || a.Peer != "boris" {
		t.Fatalf("the alarm must name the feed and the peer, got %+v", a)
	}
	if a.Witness != mine.tipOf(ref) || a.Current != forged {
		t.Fatalf("the alarm must carry both tips so the user can check them, got %+v", a)
	}
	if res.Peers[0].OK {
		t.Fatal("a peer that disagrees must not be marked OK")
	}
}

// Gossip is read-only. A peer that advertises unknown feeds and a divergent tip
// must not be able to move a single local ref -- that path belongs to Sync,
// which owns the quarantine and the fast-forward-only rule.
func TestGossipNeverWritesLocalRefs(t *testing.T) {
	repo, dir := node(t, "alice")
	ctx := context.Background()
	s := storeOf(t, repo, "alice")
	ref := FeedRef(s.Pub())

	first := mustSend(t, s, "starting point")
	mustSend(t, s, "the honest one")

	liarDir := liar(t, dir, ref, first.OID)
	rawGit(t, dir, "fetch", "--quiet", liarDir, "+refs/feeds/*:refs/quarantine/x/*")
	forged := rawGit(t, dir, "rev-parse", "refs/quarantine/x/"+s.Pub())

	theirs := viewOf(
		gitx.RefInfo{Name: ref, OID: forged},
		gitx.RefInfo{Name: FeedRef(FeedID("stranger")), OID: strings.Repeat("f", 40)},
	)
	peer := snapshotServer(t, theirs)

	before := rawGit(t, dir, "for-each-ref", "--format=%(refname) %(objectname)")
	if _, err := Gossip(ctx, repo, []GossipPeer{{Name: "boris", URL: peer.URL}}, nil); err != nil {
		t.Fatal(err)
	}
	after := rawGit(t, dir, "for-each-ref", "--format=%(refname) %(objectname)")

	if before != after {
		t.Fatalf("gossip moved a ref:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestGossipSurvivesAnUnreachablePeer(t *testing.T) {
	repo, _ := node(t, "alice")
	ctx := context.Background()
	mustSend(t, storeOf(t, repo, "alice"), "one")
	mine, _ := Capture(ctx, repo)
	good := snapshotServer(t, mine)

	res, err := Gossip(ctx, repo, []GossipPeer{
		{Name: "down", URL: closedServer(t)},
		{Name: "up", URL: good.URL},
	}, nil)
	if err != nil {
		t.Fatalf("one unreachable peer must not fail the round: %v", err)
	}
	if len(res.Peers) != 2 {
		t.Fatalf("every configured peer should be reported, got %+v", res.Peers)
	}
	if res.Reached != 1 {
		t.Fatalf("only one peer answered, got %d", res.Reached)
	}
	if res.Peers[0].Name != "down" || res.Peers[0].OK || res.Peers[0].Note != "unreachable" {
		t.Fatalf("a peer that is down is not a security event, got %+v", res.Peers[0])
	}
	if !res.Peers[1].OK {
		t.Fatalf("the reachable peer must still be compared, got %+v", res.Peers[1])
	}
}

// A peer's reply is untrusted input: a malformed body is an error, never data.
func TestFetchSnapshotRejectsMalformedReplies(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		status  int
		wantErr bool
	}{
		{"a good body", `{"digest":"` + strings.Repeat("a", 40) + `","refs":[]}`, 200, false},
		{"not json", `{`, 200, true},
		{"empty body", ``, 200, true},
		{"digest is not an object name", `{"digest":"nope","refs":[]}`, 200, true},
		{"digest is missing", `{"refs":[]}`, 200, true},
		{"the peer errored", `{}`, 500, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(c.status)
				_, _ = w.Write([]byte(c.body))
			}))
			defer srv.Close()

			_, err := FetchSnapshot(context.Background(), nil, srv.URL)
			if c.wantErr != (err != nil) {
				t.Fatalf("wantErr=%v, got %v", c.wantErr, err)
			}
		})
	}
}

// Operators write peer lists in whatever shape their notes are in; a missing
// scheme is the easy mistake, and it must not become a confusing HTTP error.
func TestGossipEndpointAcceptsWhatOperatorsWrite(t *testing.T) {
	cases := []struct {
		in    string
		want  string
		valid bool
	}{
		{"10.0.0.7:8082", "http://10.0.0.7:8082" + snapshotPath, true},
		{"http://10.0.0.7:8082", "http://10.0.0.7:8082" + snapshotPath, true},
		{"http://10.0.0.7:8082/", "http://10.0.0.7:8082" + snapshotPath, true},
		{"http://10.0.0.7:8082/api/snapshot", "http://10.0.0.7:8082" + snapshotPath, true},
		{"", "", false},
		{"   ", "", false},
		{"http://", "", false},
	}
	for _, c := range cases {
		got, err := gossipEndpoint(c.in)
		if !c.valid {
			if err == nil {
				t.Fatalf("%q should be rejected, got %q", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%q: %v", c.in, err)
		}
		if got != c.want {
			t.Fatalf("%q: want %q, got %q", c.in, c.want, got)
		}
	}
}

// Determinism matters: the same pair of views must always yield the same
// verdict, or the UI and the dedup keys drift.
func TestGossipOutputIsOrdered(t *testing.T) {
	repo, _ := node(t, "alice")
	ctx := context.Background()
	mustSend(t, storeOf(t, repo, "alice"), "one")

	// Peer A reports one unknown feed; peer B reports that same feed plus a
	// second one. The feed both peers confirm must rank first -- a claim two
	// independent sources make is worth more than one source's claim, and that
	// ordering is the whole ranking.
	seenTwice, seenOnce := FeedRef(FeedID("dave")), FeedRef(FeedID("xavier"))
	a := snapshotServer(t, viewOf(gitx.RefInfo{Name: seenTwice, OID: strings.Repeat("1", 40)}))
	b := snapshotServer(t, viewOf(
		gitx.RefInfo{Name: seenTwice, OID: strings.Repeat("1", 40)},
		gitx.RefInfo{Name: seenOnce, OID: strings.Repeat("2", 40)},
	))

	res, err := Gossip(ctx, repo, []GossipPeer{{Name: "a", URL: a.URL}, {Name: "b", URL: b.URL}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Missing) != 2 {
		t.Fatalf("expected two unheld feeds, got %+v", res.Missing)
	}
	if res.Missing[0].Feed != seenTwice || res.Missing[0].Peers != 2 {
		t.Fatalf("the feed both peers confirm must come first, got %+v", res.Missing)
	}
	if res.Missing[1].Feed != seenOnce || res.Missing[1].Peers != 1 {
		t.Fatalf("the feed one peer claims must come last, got %+v", res.Missing)
	}
}
