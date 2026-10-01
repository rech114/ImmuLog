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
)

// syncBatch is how many new messages per feed one sync pulls at most.
const syncBatch = 200

// State is this node's outward-facing condition: maintained by the background
// loops, read by handlers.
type State struct {
	mu     sync.RWMutex
	snap   feed.Snapshot
	anchor feed.Anchor
	peers  []feed.PeerView
}

// Snapshot returns a copy of the current condition.
func (s *State) Snapshot() (feed.Snapshot, feed.Anchor, []feed.PeerView) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snap, s.anchor, s.peers
}

func (s *State) setSnapshot(snap feed.Snapshot) {
	s.mu.Lock()
	s.snap = snap
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

// Watch starts the three background loops and returns immediately. Cancelling
// ctx stops all of them.
func (s *Server) Watch(ctx context.Context, syncEvery, anchorEvery time.Duration) {
	if syncEvery <= 0 {
		syncEvery = SyncInterval
	}
	if anchorEvery <= 0 {
		anchorEvery = AnchorInterval
	}
	go s.loop(ctx, syncEvery, s.syncOnce)
	go s.loop(ctx, anchorEvery, s.anchorOnce)
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

func (s *Server) refreshSnapshot(ctx context.Context) {
	if snap, err := feed.Capture(ctx, s.repo); err == nil {
		s.state.setSnapshot(snap)
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
		detail = "Two remotes gave tips for the same feed that are not ancestors of one another: someone is telling you and someone else different stories."
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
