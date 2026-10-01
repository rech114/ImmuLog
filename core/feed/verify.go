// SPDX-License-Identifier: Apache-2.0

package feed

import (
	"context"
	"strings"

	"immulog/core/gitx"
)

// Namespaces for feeds and witness anchors.
const (
	FeedPrefix    = "refs/feeds/"
	witnessPrefix = "refs/witness/"
)

// FeedRef builds a ref name from a feed name.
func FeedRef(name string) string { return FeedPrefix + name }

// FeedName extracts the feed name from a ref name.
func FeedName(ref string) string { return strings.TrimPrefix(ref, FeedPrefix) }

// WitnessRef derives the witness ref from a feed ref.
//
// The witness anchor is advanced only by this machine and is **never pushed and
// never fetched** -- an attacker who takes over a remote still cannot reach it.
// This is layer L1 from docs/DESIGN.md §6.3.
func WitnessRef(feedRef string) string {
	return witnessPrefix + FeedName(feedRef)
}

// Reason codes for a verdict.
const (
	ReasonRewrite  = "rewrite"  // tip is not a descendant of the witness (a parallel chain: force push)
	ReasonRollback = "rollback" // tip is an ancestor of the witness (the chain was pointed backwards)
	ReasonSplit    = "split"    // two remotes contradict each other (split view)
)

// Verdict is the result of one integrity check.
type Verdict struct {
	OK      bool   `json:"ok"`
	Reason  string `json:"reason,omitempty"`
	Feed    string `json:"feed,omitempty"`
	Peer    string `json:"peer,omitempty"`
	Witness string `json:"witness,omitempty"`
	Current string `json:"current,omitempty"`
}

// WitnessOf reads a feed's witness anchor; empty when none has been established.
func WitnessOf(ctx context.Context, repo *gitx.Repo, feedRef string) (string, error) {
	return repo.Resolve(ctx, WitnessRef(feedRef))
}

// VerifyRef decides whether feedRef's tip still has its witness anchor as an
// ancestor.
//
// For a linear feed a single git primitive is complete:
//
//	rollback -- the tip became an **ancestor** of the witness
//	rewrite  -- the two became chains with no common descendant
//
// Both make IsAncestor(witness, tip) false; one reverse lookup distinguishes
// them, so the warning text can be accurate. Only the abnormal path pays for
// the extra call.
//
// The design position in one line: **a reference rewrite needs no prevention,
// only detection.**
func VerifyRef(ctx context.Context, repo *gitx.Repo, feedRef string) (Verdict, error) {
	w, err := WitnessOf(ctx, repo, feedRef)
	if err != nil {
		return Verdict{}, err
	}
	cur, err := repo.Resolve(ctx, feedRef)
	if err != nil {
		return Verdict{}, err
	}

	v := Verdict{Feed: feedRef, Witness: w, Current: cur}
	if w == "" || cur == "" || w == cur {
		v.OK = true
		return v, nil
	}

	fwd, err := repo.IsAncestor(ctx, w, cur)
	if err != nil {
		return v, err
	}
	if fwd {
		v.OK = true
		return v, nil
	}

	if back, err := repo.IsAncestor(ctx, cur, w); err == nil && back {
		v.Reason = ReasonRollback
	} else {
		v.Reason = ReasonRewrite
	}
	return v, nil
}

// AdvanceWitness moves the witness anchor to oid. **Call only after
// verification has passed.**
func AdvanceWitness(ctx context.Context, repo *gitx.Repo, feedRef, oid string) error {
	return repo.UpdateRef(ctx, WitnessRef(feedRef), oid, "")
}

// ── Convenience methods on Store (the local feed) ───────────────────

// Witness returns the local feed's witness anchor.
func (s *Store) Witness(ctx context.Context) (string, error) {
	return WitnessOf(ctx, s.repo, s.ref)
}

// Verify decides whether the local feed is still self-consistent.
func (s *Store) Verify(ctx context.Context) (Verdict, error) {
	return VerifyRef(ctx, s.repo, s.ref)
}

func (s *Store) advanceWitness(ctx context.Context, oid string) error {
	return AdvanceWitness(ctx, s.repo, s.ref, oid)
}
