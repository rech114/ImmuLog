// SPDX-License-Identifier: Apache-2.0

package feed

import (
	"context"
	"sort"
	"strings"

	"immulog/core/gitx"
)

// Remote is one sync source. The URL uses git's own transports
// (ssh / https / file / git).
type Remote struct {
	Name string
	URL  string
}

// PeerView is how one remote currently looks to this node.
type PeerView struct {
	Name string `json:"name"`
	URL  string `json:"url"`
	OK   bool   `json:"ok"`
	Tip  string `json:"tip,omitempty"`
	Note string `json:"note,omitempty"`
}

// SyncResult is the product of one sync.
type SyncResult struct {
	Advanced []Message  // messages that are genuinely new this round, in chain order
	Alarms   []Verdict  // detected inconsistencies
	Peers    []PeerView // the state of each remote
	Reached  int        // number of reachable remotes
}

// Sync pulls from every remote, verifies feed by feed, and advances local
// state with fast-forwards only.
//
// Remote Names must be pairwise distinct -- the name selects a quarantine slot,
// and duplicates would overwrite each other. (main.go's parseRemotes already
// dedupes; direct callers must guarantee it themselves.)
//
// Four rules (docs/DESIGN.md §8.10 / §6.3 / §4.3):
//
//  1. **Network input lands in the quarantine first** refs/quarantine/<slot>/ --
//     it never writes trusted state directly
//  2. **Every feed is compared against the local witness anchor** -- foreign
//     history may not be silently rewritten either
//  3. **Fast-forwards only** -- anything else leaves local state untouched and
//     raises an alarm
//  4. **The key chain must stay continuous** -- a key may only change through a
//     rotation notice signed by the old key
//
// Remotes that contradict each other (two sources giving tips for the same feed
// with no common descendant) are reported as a split view.
func Sync(ctx context.Context, repo *gitx.Repo, remotes []Remote, maxNew int) (SyncResult, error) {
	res := SyncResult{}

	// 1) Pull into the quarantine
	reachable := make(map[string]bool, len(remotes))
	for _, rm := range remotes {
		slot := FeedID(rm.Name)
		if err := repo.FetchInto(ctx, rm.URL, FeedPrefix+"*", gitx.Quarantine+slot); err != nil {
			reachable[rm.Name] = false
			continue
		}
		reachable[rm.Name] = true
		res.Reached++
	}

	// 2) Group each remote's claims by feed
	type claim struct{ peer, tip string }
	claims := map[string][]claim{}
	slots := map[string]string{} // slot -> remote name
	for _, rm := range remotes {
		slots[FeedID(rm.Name)] = rm.Name
	}

	qr, err := repo.Refs(ctx, gitx.Quarantine)
	if err != nil {
		return res, err
	}
	for _, r := range qr {
		slot, name, ok := strings.Cut(strings.TrimPrefix(r.Name, gitx.Quarantine), "/")
		if !ok || name == "" {
			continue
		}
		peer, ok := slots[slot]
		if !ok {
			continue
		}
		ref := FeedRef(name)
		claims[ref] = append(claims[ref], claim{peer: peer, tip: r.OID})
	}

	bad := map[string]string{} // remote name -> problem description

	// 3) Verify and advance, feed by feed
	feeds := make([]string, 0, len(claims))
	for ref := range claims {
		feeds = append(feeds, ref)
	}
	sort.Strings(feeds)

	for _, ref := range feeds {
		w, err := WitnessOf(ctx, repo, ref)
		if err != nil {
			return res, err
		}
		local, err := repo.Resolve(ctx, ref)
		if err != nil {
			return res, err
		}

		var best string
		for _, cl := range claims[ref] {
			if !reachable[cl.peer] {
				continue
			}
			// Compare against the **local witness anchor**: foreign history may
			// not be silently rewritten either
			if w != "" {
				fwd, err := repo.IsAncestor(ctx, w, cl.tip)
				if err != nil {
					return res, err
				}
				if !fwd {
					back, _ := repo.IsAncestor(ctx, cl.tip, w)
					reason := ReasonRewrite
					if back {
						reason = ReasonRollback
					}
					res.Alarms = append(res.Alarms, Verdict{
						Feed: ref, Peer: cl.peer, Reason: reason,
						Witness: w, Current: cl.tip,
					})
					bad[cl.peer] = "inconsistent with the local witness anchor"
					continue
				}
			}
			// Compare against **other remotes**: two sources contradicting each
			// other is a split view
			switch {
			case best == "" || best == cl.tip:
				best = cl.tip
			default:
				fwd, _ := repo.IsAncestor(ctx, best, cl.tip)
				back, _ := repo.IsAncestor(ctx, cl.tip, best)
				if fwd {
					best = cl.tip
				} else if !back {
					res.Alarms = append(res.Alarms, Verdict{
						Feed: ref, Peer: cl.peer, Reason: ReasonSplit,
						Witness: best, Current: cl.tip,
					})
					bad[cl.peer] = "contradicts another remote"
				}
			}
		}

		if best == "" || best == local {
			continue
		}
		// Fast-forwards only: anything else leaves local state untouched (the
		// alarm was already raised above)
		if local != "" {
			fwd, err := repo.IsAncestor(ctx, local, best)
			if err != nil || !fwd {
				continue
			}
		}

		// Key chain: a key may only change via a rotation notice signed by the
		// old key.
		// Note this uses best (an OID), not ref -- ref still points at local
		// right now, so the range would be empty.
		raw, err := repo.LogRange(ctx, best, local, maxNew)
		if err != nil {
			continue
		}
		prev, err := KeyAt(ctx, repo, local)
		if err != nil {
			continue
		}
		if kv, err := CheckKeyChain(prev, raw); err == nil && !kv.OK {
			kv.Feed = ref
			kv.Peer = claims[ref][0].peer
			res.Alarms = append(res.Alarms, kv)
			bad[kv.Peer] = "broken key chain"
			continue // refuse to advance
		}

		if err := repo.UpdateRef(ctx, ref, best, local); err != nil {
			continue
		}
		if err := AdvanceWitness(ctx, repo, ref, best); err != nil {
			continue
		}
		res.Advanced = append(res.Advanced, Decode(raw, ref, Opener(ctx, repo))...)
	}

	// 4) Summarise each remote's state
	for _, rm := range remotes {
		v := PeerView{Name: rm.Name, URL: rm.URL, OK: reachable[rm.Name]}
		switch {
		case !reachable[rm.Name]:
			v.Note = "unreachable"
		default:
			if note, ok := bad[rm.Name]; ok {
				v.OK, v.Note = false, note
			}
		}
		res.Peers = append(res.Peers, v)
	}
	return res, nil
}

// Publish pushes the local feed to every remote.
//
// Failure is **not fatal**: the local commit is the fact, a remote is only a
// courier. Returns each remote's error (empty string on success) so the UI can
// show reachability.
func Publish(ctx context.Context, repo *gitx.Repo, remotes []Remote, feedRef string) map[string]string {
	out := make(map[string]string, len(remotes))
	if feedRef == "" {
		return out
	}
	for _, rm := range remotes {
		if err := repo.PushRef(ctx, rm.URL, feedRef); err != nil {
			out[rm.Name] = err.Error()
		}
	}
	return out
}
