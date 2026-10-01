// SPDX-License-Identifier: Apache-2.0

package feed

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"immulog/core/gitx"
)

// Snapshot is a complete record of "where this node believes each feed's tip
// is" at one moment.
//
// Its purpose is **split-view detection**: two nodes each take a snapshot, and
// comparing them reveals whether someone is telling different people different
// stories.
type Snapshot struct {
	Digest string         `json:"digest"`
	Refs   []gitx.RefInfo `json:"refs"`
	At     time.Time      `json:"at"`
	Text   string         `json:"-"`
}

// Capture takes a snapshot of every feed this node currently knows about.
//
// The digest is the blob object name of a sorted canonical text. **Content
// addressing is already a Merkle leaf**, so there is no Merkle tree to build --
// that is "reuse the wheel" in practice.
//
// Why no RFC 6962 style inclusion / consistency proofs: that machinery exists
// for **light clients**, which do not hold the full ref list and therefore need
// O(log n) proofs. Our clients already hold every ref, so comparing them one by
// one is both simpler and **stronger** than proving inclusion. If light clients
// ever appear, that is the day to add proofs.
func Capture(ctx context.Context, repo *gitx.Repo) (Snapshot, error) {
	refs, err := repo.Refs(ctx, FeedPrefix)
	if err != nil {
		return Snapshot{}, err
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Name < refs[j].Name })

	var b strings.Builder
	for _, r := range refs {
		b.WriteString(r.Name)
		b.WriteByte(' ')
		b.WriteString(r.OID)
		b.WriteByte('\n')
	}
	text := b.String()

	digest, err := repo.HashBlob(ctx, []byte(text), false)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{Digest: digest, Refs: refs, Text: text, At: time.Now().UTC()}, nil
}

// tipOf returns one feed's tip from the snapshot.
func (s Snapshot) tipOf(ref string) string {
	for _, r := range s.Refs {
		if r.Name == ref {
			return r.OID
		}
	}
	return ""
}

// Diff is the outcome of comparing two snapshots, split into four buckets.
//
// The buckets exist because "different" is not the same as "contradictory"
// (§11): one side being behind is ordinary, and so is a feed only one side
// knows about. Only tips that are not ancestors of one another count as a
// contradiction.
type Diff struct {
	// Diverged holds feeds both sides name whose tips are not ancestors of one
	// another -- a split view.
	Diverged []string `json:"diverged,omitempty"`
	// MissingHere holds feeds the peer serves and this node does not know at
	// all. One peer reporting it is a new member; several reporting it while
	// the local source stays silent is the case gossip exists for.
	MissingHere []string `json:"missingHere,omitempty"`
	// MissingThere holds feeds this node serves and the peer does not.
	MissingThere []string `json:"missingThere,omitempty"`
	// Unverifiable holds feeds whose tips differ while the peer's object is not
	// present locally, so no verdict is possible. Never guessed at.
	Unverifiable []string `json:"unverifiable,omitempty"`
}

// Compare compares two snapshots as **sets**, not feed by feed.
//
// The bar for a contradiction stays deliberately strict: two tips for the same
// feed that are not ancestors of one another. One side simply being behind (an
// ancestor) means "not synced yet", not an attack.
//
// The set-level half is what a per-feed comparison cannot reach. `Sync` groups
// claims by ref, so a feed that only one side serves never enters its loop at
// all -- a source can omit a feed and stay invisible. Comparing the two ref
// sets is the only way to see it, and that is what a split view is before it
// gets narrowed to a single feed (docs/DESIGN.md §6.3, §11).
//
// Both snapshots' commits must be present locally for a verdict; anything else
// lands in Unverifiable rather than being guessed at.
func Compare(ctx context.Context, repo *gitx.Repo, mine, theirs Snapshot) (Diff, error) {
	names := make(map[string]bool, len(mine.Refs)+len(theirs.Refs))
	for _, r := range mine.Refs {
		names[r.Name] = true
	}
	for _, r := range theirs.Refs {
		names[r.Name] = true
	}
	ordered := make([]string, 0, len(names))
	for name := range names {
		ordered = append(ordered, name)
	}
	sort.Strings(ordered) // deterministic: the same pair of views yields the same verdict

	var d Diff
	for _, name := range ordered {
		m, t := mine.tipOf(name), theirs.tipOf(name)
		switch {
		case m == "":
			d.MissingHere = append(d.MissingHere, name)
		case t == "":
			d.MissingThere = append(d.MissingThere, name)
		case m == t:
			continue // identical tip: nothing to report
		default:
			fwd, err := repo.IsAncestor(ctx, m, t)
			if err != nil {
				d.Unverifiable = append(d.Unverifiable, name)
				continue
			}
			if fwd {
				continue // the peer is ahead: normal
			}
			back, err := repo.IsAncestor(ctx, t, m)
			if err != nil {
				d.Unverifiable = append(d.Unverifiable, name)
				continue
			}
			if !back {
				d.Diverged = append(d.Diverged, name) // neither is an ancestor => divergence
			}
		}
	}
	return d, nil
}

// Diverged returns only the contradictions, and refuses to answer when an
// object is missing locally -- it does not quietly say "consistent". It is
// Compare with the strict contract the first implementation had.
func Diverged(ctx context.Context, repo *gitx.Repo, mine, theirs Snapshot) ([]string, error) {
	d, err := Compare(ctx, repo, mine, theirs)
	if err != nil {
		return nil, err
	}
	if len(d.Unverifiable) > 0 {
		return nil, fmt.Errorf("cannot compare %s: the object is not present locally", d.Unverifiable[0])
	}
	return d.Diverged, nil
}
