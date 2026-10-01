// SPDX-License-Identifier: Apache-2.0

// guard.go -- background loops: integrity, multi-source sync, external
// anchoring.
//
// Three jobs, three loops, three intervals. The alarms they raise are not
// toasts -- they are **a notice inserted into the timeline** (docs/DESIGN.md
// §8.5): the client renders them as cards that never auto-dismiss, and an
// attacker cannot delete them.
package web

import (
	"context"
	"sync"
	"time"

	"immulog/core/feed"
)

// Default intervals for the background loops. Overridable by environment
// variable (both tests and operations rely on that).
const (
	IntegrityInterval = 5 * time.Second
	SyncInterval      = 5 * time.Second
	AnchorInterval    = 60 * time.Second
	// GossipInterval is how often peer views are compared. It is the cheapest
	// loop in the process -- one GET per peer -- so it can afford to be as
	// frequent as sync.
	GossipInterval = 5 * time.Second
)

// syncBatch is how many new messages per feed one sync pulls at most.
const syncBatch = 200

// State is this node's outward-facing condition: maintained by the background
// loops, read by handlers.
type State struct {
	mu      sync.RWMutex
	snap    feed.Snapshot
	anchor  feed.Anchor
	peers   []feed.PeerView
	gossip  []feed.PeerReport
	missing []feed.SeenElsewhere
}

// Snapshot returns a copy of the current condition.
func (s *State) Snapshot() (feed.Snapshot, feed.Anchor, []feed.PeerView) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snap, s.anchor, s.peers
}

// Gossip returns the latest peer views and the feeds peers hold that this node
// does not. It is a separate accessor so the git-sync picture and the
// view-comparison picture stay distinguishable.
func (s *State) Gossip() ([]feed.PeerReport, []feed.SeenElsewhere) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.gossip, s.missing
}

// swapSnapshot stores a new snapshot and reports whether its digest changed.
//
// The `snapshot` event fires on change only: a digest that repeats every few
// seconds is noise, not news, and a stream of identical frames would push the
// real events out of the browser's buffer.
func (s *State) swapSnapshot(snap feed.Snapshot) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := s.snap.Digest != snap.Digest
	s.snap = snap
	return changed
}

func (s *State) setGossip(reports []feed.PeerReport, missing []feed.SeenElsewhere) {
	s.mu.Lock()
	s.gossip, s.missing = reports, missing
	s.mu.Unlock()
}

func (s *State) setAnchor(a feed.Anchor) {
	s.mu.Lock()
	s.anchor = a
	s.mu.Unlock()
}

func (s *State) setPeers(p []feed.PeerView) {
	s.mu.Lock()
	s.peers = p
	s.mu.Unlock()
}

// Watch starts the background loops and returns immediately. Cancelling ctx
// stops all of them.
func (s *Server) Watch(ctx context.Context, syncEvery, anchorEvery, gossipEvery time.Duration) {
	if syncEvery <= 0 {
		syncEvery = SyncInterval
	}
	if anchorEvery <= 0 {
		anchorEvery = AnchorInterval
	}
	if gossipEvery <= 0 {
		gossipEvery = GossipInterval
	}
	go s.loop(ctx, syncEvery, s.syncOnce)
	go s.loop(ctx, anchorEvery, s.anchorOnce)
	go s.loop(ctx, gossipEvery, s.gossipOnce)
	go s.loop(ctx, IntegrityInterval, s.verifyOnce)
	go s.pushLoop(ctx)
}

func (s *Server) loop(ctx context.Context, every time.Duration, fn func(context.Context)) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			fn(ctx)
		}
	}
}

// syncOnce pulls from every remote, verifies, advances by fast-forward only,
// then broadcasts the delta.
func (s *Server) syncOnce(ctx context.Context) {
	res, err := feed.Sync(ctx, s.repo, s.remotes, syncBatch)
	if err != nil {
		s.log.Warn("sync failed", "err", err)
		return
	}
	if len(s.remotes) > 0 {
		s.state.setPeers(res.Peers)
	}
	for _, m := range res.Advanced {
		if ev, ok := feedEventOf(m); ok {
			s.hub.Broadcast(ev)
		}
	}
	for _, v := range res.Alarms {
		s.log.Warn("inconsistency detected",
			"feed", v.Feed, "peer", v.Peer, "reason", v.Reason,
			"witness", v.Witness, "current", v.Current)
		s.announce(v)
	}
	s.refreshSnapshot(ctx)
}

// gossipOnce compares this node's whole view against every peer's.
//
// **Read-only by construction**: it never calls update-ref, so a peer cannot
// use gossip to move a local ref. Whatever it learns is reported and nothing
// else -- promotion of a foreign feed stays with syncOnce, which owns the
// quarantine and the fast-forward-only rule.
//
// It is also the one check that reaches peers this node does not *fetch* from:
// a view costs one GET, so the set of parties who can contradict you is no
// longer limited to the git remotes in the configuration.
func (s *Server) gossipOnce(ctx context.Context) {
	if len(s.peers) == 0 {
		return
	}
	res, err := feed.Gossip(ctx, s.repo, s.peers, nil)
	if err != nil {
		s.log.Warn("gossip failed", "err", err)
		return
	}
	s.state.setGossip(res.Peers, res.Missing)
	for _, v := range res.Alarms {
		s.log.Warn("split view detected",
			"feed", v.Feed, "peer", v.Peer,
			"witness", v.Witness, "current", v.Current)
		s.announce(v)
	}
}

// verifyOnce inspects the local feed. This machine is the only writer, so in
// normal operation this stays silent forever.
func (s *Server) verifyOnce(ctx context.Context) {
	v, err := s.store.Verify(ctx)
	if err != nil {
		s.log.Warn("integrity check failed", "err", err)
		return
	}
	if !v.OK {
		s.announce(v)
		return
	}
	s.refreshSnapshot(ctx)
}

// anchorOnce appends the current snapshot to the anchor chain and, best-effort,
// hands it to an external service.
func (s *Server) anchorOnce(ctx context.Context) {
	a, err := feed.AnchorNow(ctx, s.repo, s.publisher, s.store.Signed())
	if err != nil {
		s.log.Warn("anchoring failed", "err", err)
		return
	}
	s.state.setAnchor(a)
	s.log.Info("anchored snapshot",
		"seq", a.Seq, "snapshot", short(a.Snapshot), "external", a.External != "")
}

// refreshSnapshot re-reads the local view and tells the stream when it changed.
//
// This is where the `snapshot` event declared in §7.2 finally comes from: the
// digest is the unit peers compare, so a client should see it move.
func (s *Server) refreshSnapshot(ctx context.Context) {
	snap, err := feed.Capture(ctx, s.repo)
	if err != nil {
		return
	}
	if s.state.swapSnapshot(snap) {
		s.hub.Broadcast(Event{ID: snap.Digest, Type: "snapshot", Data: map[string]any{
			"digest": snap.Digest,
			"feeds":  len(snap.Refs),
			"at":     snap.At,
		}})
	}
}

// announce broadcasts an alarm after deduplication: one occurrence per problem,
// so the timeline does not flood.
func (s *Server) announce(v feed.Verdict) {
	key := v.Feed + "|" + v.Reason + "|" + v.Peer + "|" + v.Current
	s.muGuard.Lock()
	seen := s.guardSeen[key]
	if s.guardSeen == nil {
		s.guardSeen = map[string]bool{}
	}
	s.guardSeen[key] = true
	s.muGuard.Unlock()
	if seen {
		return
	}
	if ev, ok := alarmEvent(v); ok {
		s.hub.Broadcast(ev)
	}
}

// alarmEvent turns a verdict into an alarm event. Returns false when the check
// passed.
func alarmEvent(v feed.Verdict) (Event, bool) {
	if v.OK || v.Reason == "" {
		return Event{}, false
	}

	title, detail := "History rewrite detected",
		"The local witness anchor is no longer an ancestor of the current tip: someone rewrote this history without leaving a rewrite notice. The local copy has been kept and will not be overwritten."
	switch v.Reason {
	case feed.ReasonRollback:
		title = "History rollback detected"
		detail = "Message sequence numbers went backwards: the tip was pointed at an earlier commit."
	case feed.ReasonSplit:
		title = "Split view detected"
		detail = "Two sources gave tips for the same feed that are not ancestors of one another: someone is telling you and someone else different stories."
	case feed.ReasonKeyChanged:
		title = "Signing key swapped"
		detail = "A key change appeared on the chain with no rotation notice behind it: the new key is neither the previous one nor declared by it. The message that carries it was not written by its author."
	}
	if v.Peer != "" {
		detail += " (source: " + v.Peer + ")"
	}

	return Event{
		ID:   v.Current + "|" + v.Reason,
		Type: "alarm",
		Data: map[string]any{
			"oid":    v.Current,
			"title":  title,
			"detail": detail,
			"local":  short(v.Witness),
			"remote": short(v.Current),
			"reason": v.Reason,
			"feed":   feed.FeedName(v.Feed),
			"peer":   v.Peer,
		},
	}, true
}

func short(oid string) string {
	if len(oid) > 6 {
		return oid[:6]
	}
	return oid
}
