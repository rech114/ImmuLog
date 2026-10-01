// SPDX-License-Identifier: Apache-2.0

package gitx

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"
)

// logFormat reads back one page of history in a single process call.
//
// `git log -z` separates records with NUL and \x1f separates fields, with %B
// placed **last**: that way a body containing \x1e or any control character
// cannot split a record in the wrong place.
//
// %G? is git's own signature status: N=unsigned G=good U=unknown key B=bad.
// %GK is the signer's key fingerprint -- the key chain is stitched together
// commit by commit from this value.
const logFormat = "%H%x1f%an%x1f%at%x1f%G?%x1f%GK%x1f" +
	"%(trailers:key=ImmuLog-Seq,valueonly)%x1f" +
	"%(trailers:key=ImmuLog-Retracts,valueonly)%x1f" +
	"%(trailers:key=ImmuLog-Reason,valueonly)%x1f" +
	"%B"

// RawCommit is one commit as parsed from log output.
// Trailers are parsed by git itself (no wheel reinvented here); Kind / Declared
// / Epoch / Enc are pulled from the trailing trailer paragraph using git's own
// trailer rules.
type RawCommit struct {
	OID      string
	Author   string
	At       time.Time
	Sig      string // "" | "good" | "untrusted" | "bad"
	Key      string // signing key fingerprint; empty when unsigned
	Seq      string
	Retracts string
	Reason   string
	Kind     string // `ImmuLog-Kind` trailer
	Declared string // `ImmuLog-Key` trailer (only present on rotation notices)
	Epoch    string // `ImmuLog-Epoch` trailer
	Enc      string // `ImmuLog-Enc` trailer -- the author's encryption public key
	Body     string
}

// sigStatus turns git's %G? character into something readable.
func sigStatus(c string) string {
	switch c {
	case "G":
		return "good"
	case "U":
		return "untrusted"
	case "B":
		return "bad"
	default:
		return ""
	}
}

// EmptyTree writes and returns the empty tree's object name.
// Messages produce no files; every commit reuses this one tree.
func (r *Repo) EmptyTree(ctx context.Context) (string, error) {
	out, err := r.run(context.WithoutCancel(ctx), []byte{}, "hash-object", "-w", "-t", "tree", "--stdin")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// Commit creates a commit object. An empty parent means the start of the chain.
//
// `git commit` is deliberately avoided: commit-tree needs no index, no
// worktree, and produces no .lock file, so it works in a bare repository and
// concurrent writes to different refs are inherently safe. The message arrives
// on stdin, which removes argument injection by construction.
func (r *Repo) Commit(ctx context.Context, tree, parent, message string, sign bool) (string, error) {
	args := []string{"commit-tree", tree}
	if parent != "" {
		args = append(args, "-p", parent)
	}
	if sign {
		args = append(args, "-S")
	}
	return r.commitTree(ctx, args, message)
}

// CommitAs signs a commit with a **specific key**.
//
// A key-rotation notice must be signed by the old key, but by then the user has
// already pointed user.signingkey at the new one, so the key has to be named
// explicitly. `-S<keyid>` is git's own syntax for that.
func (r *Repo) CommitAs(ctx context.Context, tree, parent, message, key string) (string, error) {
	args := []string{"commit-tree", tree}
	if parent != "" {
		args = append(args, "-p", parent)
	}
	if key != "" {
		args = append(args, "-S"+key)
	}
	return r.commitTree(ctx, args, message)
}

func (r *Repo) commitTree(ctx context.Context, args []string, message string) (string, error) {
	out, err := r.run(ctx, []byte(message), args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// Exists reports whether an object is present in the local object store.
func (r *Repo) Exists(ctx context.Context, oid string) (bool, error) {
	if oid == "" {
		return false, nil
	}
	_, err := r.run(ctx, nil, "cat-file", "-e", oid+"^{commit}")
	if err == nil {
		return true, nil
	}
	var ge *Error
	if errors.As(err, &ge) && (ge.Code == 1 || ge.Code == 128) {
		return false, nil
	}
	return false, err
}

// Log reads back one page of history, newest first.
func (r *Repo) Log(ctx context.Context, ref string, limit int) ([]RawCommit, error) {
	if ref == "" {
		return nil, nil
	}
	args := []string{"log", "-z", "--format=" + logFormat}
	if limit > 0 {
		args = append(args, "-n", strconv.Itoa(limit))
	}
	args = append(args, ref)

	out, err := r.run(ctx, nil, args...)
	if err != nil {
		var ge *Error
		// Empty repository / unknown ref: semantically "no history", not an error
		if errors.As(err, &ge) && (ge.Code == 128 || ge.Code == 1) {
			return nil, nil
		}
		return nil, err
	}
	return parseLog(string(out)), nil
}

func parseLog(raw string) []RawCommit {
	var commits []RawCommit
	for _, rec := range strings.Split(raw, "\x00") {
		rec = strings.Trim(rec, "\n")
		if rec == "" {
			continue
		}
		// Split only the first 8 separators; the body (field 9) is kept verbatim
		// even if it contains \x1f
		f := strings.SplitN(rec, "\x1f", 9)
		if len(f) != 9 {
			continue
		}
		c := RawCommit{
			OID:      strings.TrimSpace(f[0]),
			Author:   strings.TrimSpace(f[1]),
			Sig:      sigStatus(strings.TrimSpace(f[3])),
			Key:      strings.TrimSpace(f[4]),
			Seq:      strings.TrimSpace(f[5]),
			Retracts: strings.TrimSpace(f[6]),
			Reason:   strings.TrimSpace(f[7]),
			Body:     f[8],
		}
		c.Declared = trailerIn(f[8], "ImmuLog-Key")
		c.Kind = trailerIn(f[8], "ImmuLog-Kind")
		c.Epoch = trailerIn(f[8], "ImmuLog-Epoch")
		c.Enc = trailerIn(f[8], "ImmuLog-Enc")
		if secs, err := strconv.ParseInt(strings.TrimSpace(f[2]), 10, 64); err == nil {
			c.At = time.Unix(secs, 0).UTC()
		}
		if c.OID != "" {
			commits = append(commits, c)
		}
	}
	return commits
}

// trailerIn looks up key in the body's final paragraph. Empty when absent.
// It applies git's own trailer rule: trailers must live in the last paragraph.
func trailerIn(body, key string) string {
	s := strings.TrimRight(strings.ReplaceAll(body, "\r\n", "\n"), " \t\n")
	i := strings.LastIndex(s, "\n\n")
	if i < 0 {
		return ""
	}
	for _, line := range strings.Split(s[i+2:], "\n") {
		k, v, ok := strings.Cut(line, ":")
		if ok && strings.TrimSpace(k) == key {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// LogRange reads only the commits after `since` (exclusive) up to ref, oldest
// first. Sync uses it to compute "which commits are actually new", in one call.
func (r *Repo) LogRange(ctx context.Context, ref, since string, limit int) ([]RawCommit, error) {
	if ref == "" {
		return nil, nil
	}
	spec := ref
	if since != "" {
		spec = since + ".." + ref
	}
	args := []string{"log", "-z", "--reverse", "--format=" + logFormat}
	if limit > 0 {
		args = append(args, "-n", strconv.Itoa(limit))
	}
	args = append(args, spec)

	out, err := r.run(ctx, nil, args...)
	if err != nil {
		var ge *Error
		if errors.As(err, &ge) && (ge.Code == 128 || ge.Code == 1) {
			return nil, nil
		}
		return nil, err
	}
	return parseLog(string(out)), nil
}

// TrailerValue reads several trailer values from one commit (a single call).
// The order matches keys; a missing key yields an empty string.
func (r *Repo) TrailerValue(ctx context.Context, oid string, keys ...string) ([]string, error) {
	if oid == "" || len(keys) == 0 {
		return nil, nil
	}
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = "%(trailers:key=" + k + ",valueonly)"
	}
	out, err := r.run(ctx, nil, "log", "-1", "--format="+strings.Join(parts, "%x1f"), oid)
	if err != nil {
		return nil, err
	}
	fields := strings.Split(strings.TrimRight(string(out), "\n"), "\x1f")
	vals := make([]string, len(keys))
	for i := range keys {
		if i < len(fields) {
			vals[i] = strings.TrimSpace(fields[i])
		}
	}
	return vals, nil
}

// HashBlob computes a blob's object name: SHA-1 over `<type> <len>\0<content>`,
// git's own definition. With write=false it computes without storing, which is
// what periodic digesting wants.
func (r *Repo) HashBlob(ctx context.Context, data []byte, write bool) (string, error) {
	args := []string{"hash-object", "--stdin"}
	if write {
		args = append(args, "-w")
	}
	out, err := r.run(ctx, data, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
