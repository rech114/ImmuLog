// SPDX-License-Identifier: Apache-2.0

package feed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"immulog/core/gitx"
)

// anchorRef is the tip of the anchor chain. Anchors are append-only, never
// rewritten.
const anchorRef = "refs/anchors/latest"

// anchorPrefix lets external tools walk the anchor chain.
const anchorPrefix = "refs/anchors/"

// maxReceipt is the truncation length for external receipts.
const maxReceipt = 512

// Publisher hands a snapshot digest to a destination **outside this machine**
// and returns a receipt.
//
// This is the entire "external anchoring" interface. The real **externality**
// comes from that one hop: once a receipt lives on someone else's turf, nobody
// can harmonise the story afterwards.
type Publisher interface {
	Publish(ctx context.Context, digest string) (receipt string, err error)
}

// Anchor is one anchoring event.
type Anchor struct {
	OID      string    `json:"oid"`
	Seq      int       `json:"seq"`
	Snapshot string    `json:"snapshot"`
	Prev     string    `json:"prev,omitempty"`
	At       time.Time `json:"at"`
	External string    `json:"external,omitempty"`
}

// AnchorNow appends the current snapshot to the anchor chain.
//
// Each anchor's parent is the previous anchor -- **the chain is guarded by
// git's hash chain**: rewriting an anchor in the middle requires rewriting
// every anchor after it, and clients may still hold the old ones. Same
// philosophy as retraction.
func AnchorNow(ctx context.Context, repo *gitx.Repo, pub Publisher, sign bool) (Anchor, error) {
	snap, err := Capture(ctx, repo)
	if err != nil {
		return Anchor{}, err
	}
	prev, err := repo.Resolve(ctx, anchorRef)
	if err != nil {
		return Anchor{}, err
	}
	now := time.Now().UTC()

	// An external failure must not block local anchoring: the local chain has
	// value on its own
	receipt := ""
	if pub != nil {
		if r, err := pub.Publish(ctx, snap.Digest); err == nil {
			// The receipt is a foreign string: collapse it to one line, or a
			// single newline could forge a trailer
			receipt = truncate(sanitizeValue(r), maxReceipt)
		}
	}

	// Canonical text first, trailer block last, one blank line between -- the
	// same convention as message encoding
	var b strings.Builder
	b.WriteString(snap.Text)
	b.WriteByte('\n')
	b.WriteString("ImmuLog-Snapshot: " + snap.Digest + "\n")
	b.WriteString("ImmuLog-At: " + now.Format(time.RFC3339) + "\n")
	if receipt != "" {
		b.WriteString("ImmuLog-External: " + receipt + "\n")
	}

	tree, err := repo.EmptyTree(ctx)
	if err != nil {
		return Anchor{}, err
	}
	oid, err := repo.Commit(ctx, tree, prev, b.String(), sign)
	if err != nil {
		return Anchor{}, err
	}
	// CAS: concurrent anchoring does not overwrite; just retry
	if err := repo.UpdateRef(ctx, anchorRef, oid, prev); err != nil {
		return Anchor{}, err
	}

	seq := 0
	if n, err := repo.Count(ctx, anchorRef); err == nil {
		seq = n
	}
	return Anchor{OID: oid, Seq: seq, Snapshot: snap.Digest, Prev: prev, At: now, External: receipt}, nil
}

// AnchorHead reads the latest anchor; zero value if never anchored.
func AnchorHead(ctx context.Context, repo *gitx.Repo) (Anchor, error) {
	head, err := repo.Resolve(ctx, anchorRef)
	if err != nil || head == "" {
		return Anchor{}, err
	}
	vals, err := repo.TrailerValue(ctx, head,
		"ImmuLog-Snapshot", "ImmuLog-At", "ImmuLog-External")
	if err != nil {
		return Anchor{}, err
	}
	a := Anchor{OID: head, Snapshot: vals[0], External: vals[2]}
	if t, perr := time.Parse(time.RFC3339, vals[1]); perr == nil {
		a.At = t.UTC()
	}
	if n, cerr := repo.Count(ctx, anchorRef); cerr == nil {
		a.Seq = n
	}
	return a, nil
}

// HTTPPublisher POSTs the digest to an external service and treats the response
// body as the receipt.
//
// An OpenTimestamps gateway, an RFC3161 gateway, or your own notary all work --
// swapping services means swapping a URL. This is also the only part of
// "external" that needs code: **the trust boundary is the URL**.
type HTTPPublisher struct {
	URL    string
	Client *http.Client
}

// Publish implements Publisher.
func (p HTTPPublisher) Publish(ctx context.Context, digest string) (string, error) {
	client := p.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	body, err := json.Marshal(map[string]string{
		"snapshot": digest,
		"at":       time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.URL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("anchor service returned %d", resp.StatusCode)
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, maxReceipt))
	return strings.TrimSpace(string(b)), nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
