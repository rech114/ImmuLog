// SPDX-License-Identifier: Apache-2.0

package feed

// gossip.go -- comparing this node's *whole view* against a peer's.
//
// Why this is not Sync a second time: Sync compares claims **within a ref**
// (`claims[ref][]claim`), so a feed that only one side serves never enters its
// loop and a source can omit a feed while staying invisible. Gossip compares
// the two ref **sets**, which is the only way to see that. That is a split view
// at its original breadth (docs/DESIGN.md §6.3, L4).
//
// Two properties it must keep:
//
//  1. **Read-only.** Gossip never calls update-ref, so it needs no quarantine:
//     it produces verdicts and nothing else. Promotion of a foreign feed stays
//     with Sync, which owns the fast-forward-only rule.
//  2. **Cheap.** A peer's view arrives from the endpoint the project already
//     exposes (`GET /api/snapshot`, §7.2) -- digest and ref list in one round
//     trip. Comparing views with twenty parties costs twenty small GETs;
//     fetching from twenty parties does not. Breadth is the security parameter
//     in §0's formula, so the mechanism that makes breadth cheap is the point.
//
// Its reach grows with the number of **independently operated** peers, and two
// are not enough: two views disagreeing tell you *that* something is wrong,
// never *who* is wrong. The third view is what localises it. A gossip set of
// one is a node talking to itself and is worth exactly nothing.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"immulog/core/gitx"
)

const (
	// snapshotPath is the endpoint peers are read from. It already existed: it
	// is the browser's own view endpoint (§7.2), so gossip adds no server
	// surface at all.
	snapshotPath = "/api/snapshot"

	// maxGossipBody bounds what a peer can make this node read. A peer is
	// untrusted input, and an unbounded read is a trivial denial of service.
	maxGossipBody = 1 << 20

	// gossipTimeout is per request. Peers are polled on a loop, so one hung
	// request must not hold the whole round open.
	gossipTimeout = 10 * time.Second
)

// GossipPeer is an HTTP endpoint serving /api/snapshot.
//
// Deliberately a different type from Remote: a Remote carries feeds over git's
// transports, a GossipPeer carries **views** over HTTP. Reusing one type for
// both would invite handing a git URL to an HTTP client.
type GossipPeer struct {
	Name string
	URL  string
}

// SeenElsewhere is a feed this node does not hold but at least one peer does.
//
// Peers is the part that matters. One peer reporting it is ordinary -- a member
// joined. Several reporting it while the local source never mentioned it is the
// case gossip exists for.
type SeenElsewhere struct {
	Feed  string `json:"feed"`
	Peers int    `json:"peers"`
}

// PeerReport is what one gossip round learned about one peer.
type PeerReport struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	OK     bool   `json:"ok"`
	Note   string `json:"note,omitempty"`
	Digest string `json:"digest,omitempty"`
	Feeds  int    `json:"feeds,omitempty"`
	Diff
}

// GossipResult is the product of one round.
type GossipResult struct {
	Peers   []PeerReport    // one entry per configured peer, in configuration order
	Alarms  []Verdict       // detected contradictions
	Missing []SeenElsewhere // feeds peers hold and this node does not, most-reported first
	Reached int             // how many peers answered
}

// Gossip polls every peer and compares each view with the local one.
//
// It writes nothing. The refs it reports are evidence for the UI and the alarm
// stream, never state: what a peer claims must not move a local ref, and the
// only path that does is Sync's quarantine plus fast-forward.
func Gossip(ctx context.Context, repo *gitx.Repo, peers []GossipPeer, client *http.Client) (GossipResult, error) {
	res := GossipResult{}
	mine, err := Capture(ctx, repo)
	if err != nil {
		return res, err
	}

	holders := map[string]int{}
	for _, p := range peers {
		report := PeerReport{Name: p.Name, URL: p.URL}

		theirs, err := FetchSnapshot(ctx, client, p.URL)
		if err != nil {
			// A peer being down is not a security event: the local copy is the
			// fact, a peer is only a second opinion.
			report.Note = "unreachable"
			res.Peers = append(res.Peers, report)
			continue
		}
		res.Reached++
		report.Digest, report.Feeds = theirs.Digest, len(theirs.Refs)

		diff, err := Compare(ctx, repo, mine, theirs)
		if err != nil {
			return res, err
		}
		report.Diff = diff
		report.OK = len(diff.Diverged) == 0 && len(diff.Unverifiable) == 0
		report.Note = noteFor(diff)

		for _, name := range diff.MissingHere {
			holders[name]++
		}
		// Only a genuine contradiction is an alarm. A feed one side knows and
		// the other does not is ordinary, so it is reported and never alarmed
		// on -- crying wolf is how a tamper warning gets ignored.
		for _, name := range diff.Diverged {
			res.Alarms = append(res.Alarms, Verdict{
				Reason:  ReasonSplit,
				Feed:    name,
				Peer:    p.Name,
				Witness: mine.tipOf(name),
				Current: theirs.tipOf(name),
			})
		}
		res.Peers = append(res.Peers, report)
	}

	res.Missing = make([]SeenElsewhere, 0, len(holders))
	for name, n := range holders {
		res.Missing = append(res.Missing, SeenElsewhere{Feed: name, Peers: n})
	}
	// Most-reported first: a feed two peers agree exists is a stronger signal
	// than one peer's claim, and that ordering is the ranking.
	sort.Slice(res.Missing, func(i, j int) bool {
		if res.Missing[i].Peers != res.Missing[j].Peers {
			return res.Missing[i].Peers > res.Missing[j].Peers
		}
		return res.Missing[i].Feed < res.Missing[j].Feed
	})
	return res, nil
}

// noteFor turns a diff into one short line for the peers panel.
func noteFor(d Diff) string {
	switch {
	case len(d.Diverged) > 0:
		return fmt.Sprintf("disagrees about %d feed(s)", len(d.Diverged))
	case len(d.Unverifiable) > 0:
		return "sent tips this node cannot check"
	case len(d.MissingHere) > 0:
		return "knows feeds this node does not"
	case len(d.MissingThere) > 0:
		return "does not carry every local feed"
	default:
		return "agrees"
	}
}

// snapshotResponse mirrors the two fields of GET /api/snapshot that gossip
// needs. The endpoint's other fields (anchor, peers, witness) are for the
// browser and are deliberately not parsed here.
type snapshotResponse struct {
	Digest string         `json:"digest"`
	Refs   []gitx.RefInfo `json:"refs"`
}

// FetchSnapshot reads one peer's view.
//
// It forms no verdict of its own: the caller compares the result with the local
// snapshot, which is where any conclusion comes from. A peer's reply is
// untrusted input, so a malformed body is an error rather than data, and the
// read is bounded.
func FetchSnapshot(ctx context.Context, client *http.Client, base string) (Snapshot, error) {
	if client == nil {
		client = &http.Client{Timeout: gossipTimeout}
	}
	endpoint, err := gossipEndpoint(base)
	if err != nil {
		return Snapshot{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Snapshot{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return Snapshot{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return Snapshot{}, fmt.Errorf("peer returned %d", resp.StatusCode)
	}

	var body snapshotResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxGossipBody)).Decode(&body); err != nil {
		return Snapshot{}, fmt.Errorf("peer sent an unreadable snapshot: %w", err)
	}
	if !isOID(body.Digest) {
		return Snapshot{}, errors.New("peer sent a malformed digest")
	}
	return Snapshot{Digest: body.Digest, Refs: body.Refs}, nil
}

// gossipEndpoint accepts "host:port", "http://host:port" and a full endpoint
// URL, and always ends up at /api/snapshot.
//
// Operators write a peer list in whatever shape their notes are in, and a
// missing scheme is the easy mistake -- http.NewRequest would reject it with a
// message that says nothing about the peer list.
func gossipEndpoint(base string) (string, error) {
	base = strings.TrimSpace(base)
	if base == "" {
		return "", errors.New("peer URL is empty")
	}
	if !strings.Contains(base, "://") {
		base = "http://" + base
	}
	u, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("peer URL %q is not usable: %w", base, err)
	}
	if u.Host == "" {
		return "", fmt.Errorf("peer URL %q has no host", base)
	}
	// Trailing slashes are trimmed on the parsed path, never on the raw string:
	// trimming "http://" as text leaves "http:/", which then looks scheme-less
	// and gets prefixed a second time.
	if u.Path = strings.TrimSuffix(u.Path, "/"); u.Path == "" {
		u.Path = snapshotPath
	}
	return u.String(), nil
}
