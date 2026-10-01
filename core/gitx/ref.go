// SPDX-License-Identifier: Apache-2.0

package gitx

import (
	"context"
	"errors"
	"strconv"
	"strings"
)

// RefInfo is one entry in a ref snapshot.
type RefInfo struct {
	Name string `json:"name"`
	OID  string `json:"oid"`
}

// Count returns the length of a ref's chain (number of commits). Used as a
// fallback when a foreign commit carries no Seq trailer.
func (r *Repo) Count(ctx context.Context, ref string) (int, error) {
	out, err := r.run(ctx, nil, "rev-list", "--count", ref)
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	return n, err
}

// UpdateRef updates a ref with compare-and-swap semantics.
//
// This is the cornerstone of the whole tamper-evidence design: when old does
// not match, git refuses and returns ErrCASFailed. The local witness anchor and
// "reject silent rewrites" need no extra code -- it is all this one parameter.
func (r *Repo) UpdateRef(ctx context.Context, ref, next, old string) error {
	args := []string{"update-ref", "--no-deref", ref, next}
	if old != "" {
		args = append(args, old)
	}
	_, err := r.run(ctx, nil, args...)
	return err
}

// EnsureRef creates a ref only if it does not already exist (old is all zeros).
func (r *Repo) EnsureRef(ctx context.Context, ref, next string) error {
	return r.UpdateRef(ctx, ref, next, strings.Repeat("0", 40))
}

// DeleteRef deletes a ref; a non-empty old also gets CAS treatment.
func (r *Repo) DeleteRef(ctx context.Context, ref, old string) error {
	args := []string{"update-ref", "--no-deref", "-d", ref}
	if old != "" {
		args = append(args, old)
	}
	_, err := r.run(ctx, nil, args...)
	return err
}

// Resolve returns a ref's current value; a missing ref yields an empty string
// rather than an error.
func (r *Repo) Resolve(ctx context.Context, ref string) (string, error) {
	out, err := r.run(ctx, nil, "rev-parse", "--verify", "--quiet", ref)
	if err != nil {
		var ge *Error
		if errors.As(err, &ge) && (ge.Code == 1 || ge.Code == 128) {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// Refs reads every ref under a namespace in a single process call.
// This is the data source for gossip snapshots (the `{feed -> tip}` list):
// one invocation gets the whole set.
func (r *Repo) Refs(ctx context.Context, prefix string) ([]RefInfo, error) {
	args := []string{"for-each-ref", "--format=%(refname)%1f%(objectname)"}
	if prefix != "" {
		args = append(args, prefix)
	}
	out, err := r.run(ctx, nil, args...)
	if err != nil {
		return nil, err
	}
	var refs []RefInfo
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\x1f", 2)
		if len(parts) != 2 {
			continue
		}
		refs = append(refs, RefInfo{Name: parts[0], OID: strings.TrimSpace(parts[1])})
	}
	return refs, nil
}

// IsAncestor reports whether a is an ancestor of b.
//
// The whole "reference rewrite" verdict rests on this one call: if the witness
// anchor is no longer an ancestor of the current tip, history has been touched.
// That covers three of the four moves described in docs/DESIGN.md §2.
func (r *Repo) IsAncestor(ctx context.Context, a, b string) (bool, error) {
	if a == "" || b == "" {
		return false, nil
	}
	_, err := r.run(ctx, nil, "merge-base", "--is-ancestor", a, b)
	if err == nil {
		return true, nil
	}
	var ge *Error
	if errors.As(err, &ge) && ge.Code == 1 {
		return false, nil // 1 means "not an ancestor" -- a normal answer, not a failure
	}
	return false, err
}
