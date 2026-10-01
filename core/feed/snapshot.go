// SPDX-License-Identifier: Apache-2.0

package feed

import (
	"context"
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

// Diverged compares two snapshots feed by feed and returns the refs that
// **contradict** each other.
//
// The bar is deliberately strict: a contradiction requires two snapshots to
// give tips for the same feed that are not ancestors of one another. One side
// simply being behind (an ancestor) means "not synced yet", not an attack.
//
// Both snapshots' commits must be present locally -- callers should fetch
// before comparing.
func Diverged(ctx context.Context, repo *gitx.Repo, mine, theirs Snapshot) ([]string, error) {
	var out []string
	for _, r := range mine.Refs {
		other := theirs.tipOf(r.Name)
		if other == "" || other == r.OID {
			continue
		}
		fwd, err := repo.IsAncestor(ctx, r.OID, other)
		if err != nil {
			return nil, err
		}
		if fwd {
			continue // the peer is ahead: normal
		}
		back, err := repo.IsAncestor(ctx, other, r.OID)
		if err != nil {
			return nil, err
		}
		if !back {
			out = append(out, r.Name) // neither is an ancestor => divergence
		}
	}
	sort.Strings(out)
	return out, nil
}
