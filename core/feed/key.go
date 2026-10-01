// SPDX-License-Identifier: Apache-2.0

package feed

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"immulog/core/gitx"
)

// ErrNoSigningKey means the repository has no signing key configured.
var ErrNoSigningKey = errors.New("user.signingkey is not configured")

// ErrSigningBroken means a key is configured but cannot sign -- we never
// degrade silently into unsigned commits.
var ErrSigningBroken = errors.New("signing key is unusable")

// Reason code for the key chain.
const ReasonKeyChanged = "keychain"

// ProbeSigning actually signs once, confirming the key works, and returns its
// fingerprint.
//
// A configured user.signingkey that cannot sign (missing key file, wrong
// permissions, gpg not installed) is the easiest accident to have: degrade
// silently and the user believes they are signing when they are not.
// **This fails loudly on purpose. It does not pretend to be safe.**
func ProbeSigning(ctx context.Context, repo *gitx.Repo) (string, error) {
	key, err := repo.SigningKey(ctx)
	if err != nil {
		return "", err
	}
	if key == "" {
		return "", ErrNoSigningKey
	}
	tree, err := repo.EmptyTree(ctx)
	if err != nil {
		return "", err
	}
	// This leaves a dangling object; acceptable, in exchange for certainty that
	// signing really works
	oid, err := repo.Commit(ctx, tree, "", "ImmuLog signing self-test\n", true)
	if err != nil {
		return "", errors.Join(ErrSigningBroken, err)
	}
	c, err := KeyAt(ctx, repo, oid)
	if err != nil {
		return "", err
	}
	if c.Key == "" {
		return "", ErrSigningBroken
	}
	return c.Key, nil
}

// KeyAt reads a commit's signing key fingerprint; empty when unsigned or the
// object is missing.
func KeyAt(ctx context.Context, repo *gitx.Repo, oid string) (gitx.RawCommit, error) {
	if oid == "" {
		return gitx.RawCommit{}, nil
	}
	raw, err := repo.Log(ctx, oid, 1)
	if err != nil || len(raw) == 0 {
		return gitx.RawCommit{}, err
	}
	return raw[0], nil
}

// CheckKeyChain verifies that the key chain across a run of new commits is
// continuous.
//
// There is exactly one rule:
//
//	a key may change -- but **only when the preceding commit is a rotation
//	notice that plainly declares the new key**
//
// That separates "changed keys" from "changed people": a legitimate rotation
// always leaves a trace, and a key change with no trace is an attack.
//
// prev is the commit just before this run (the local tip). A zero value means
// first contact, in which case the first key seen is the initial key (TOFU).
//
// ⚠️ One honest boundary: if `prev.Key` is empty (the history is entirely
// unsigned), the first signing key has nothing to vouch for it -- an unsigned
// prefix cannot be protected after the fact. That case is allowed, but callers
// should read it as "verifiable only from here on".
func CheckKeyChain(prev gitx.RawCommit, raw []gitx.RawCommit) (Verdict, error) {
	carry := prev
	for _, c := range raw {
		if c.Key != carry.Key {
			// An unsigned prefix cannot be protected: the first key seen can
			// only be accepted, and callers should label it "verifiable from
			// here on".
			unsignedPrefix := carry.Key == ""
			declared := carry.Declared != "" && carry.Declared == c.Key
			if !unsignedPrefix && !declared {
				return Verdict{
					Reason:  ReasonKeyChanged,
					Witness: carry.Key,
					Current: c.Key,
				}, nil
			}
		}
		carry = c
	}
	return Verdict{OK: true}, nil
}

// CurrentKey returns the signing key fingerprint at the local feed's tip;
// empty when unsigned.
func (s *Store) CurrentKey(ctx context.Context) (string, error) {
	tip, err := s.repo.Resolve(ctx, s.ref)
	if err != nil || tip == "" {
		return "", err
	}
	c, err := KeyAt(ctx, s.repo, tip)
	if err != nil {
		return "", err
	}
	return c.Key, nil
}

// DeclareKey appends a signing-key rotation notice.
//
// **It is signed with the current (old) key and declares the new key's
// fingerprint in a trailer.** Messages after it use the new key, so the chain
// stays continuous and auditable.
//
// Order matters: **announce first, then switch config**.
//
//	git config user.signingkey <new key>   <- do NOT do this first
//	ImmuLog calls DeclareKey(new fingerprint)
//	then point user.signingkey at it
//
// Why not let the caller pass the old key path and sign with it: for git's ssh
// signing, `-S<keyid>` resolves a key reference, not a fingerprint, and there
// is no portable way to cross-sign. So the ordering constraint is written down
// explicitly here instead of pretending it can be automated.
func (s *Store) DeclareKey(ctx context.Context, newKey string) (Message, error) {
	newKey = strings.TrimSpace(newKey)
	if newKey == "" {
		return Message{}, errors.New("new key must not be empty")
	}
	if !s.sign {
		return Message{}, ErrNoSigningKey
	}

	tip, err := s.repo.Resolve(ctx, s.ref)
	if err != nil {
		return Message{}, err
	}
	oldKey, err := s.CurrentKey(ctx)
	if err != nil {
		return Message{}, err
	}
	if oldKey == "" {
		return Message{}, ErrNoSigningKey
	}
	if oldKey == newKey {
		return Message{}, errors.New("old and new key are the same")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	seq, err := s.nextSeq(ctx, tip)
	if err != nil {
		return Message{}, err
	}
	tree, err := s.emptyTree(ctx)
	if err != nil {
		return Message{}, err
	}

	body := renderRotate(seq, newKey)
	oid, err := s.repo.Commit(ctx, tree, tip, body, true)
	if err != nil {
		return Message{}, err
	}
	if err := s.repo.UpdateRef(ctx, s.ref, oid, tip); err != nil {
		return Message{}, err
	}
	_ = s.advanceWitness(ctx, oid)

	return Message{
		OID: oid, Seq: seq, Author: s.name, Feed: s.ref,
		Kind: KindRotate, Key: ShortKey(newKey), At: time.Now().UTC(),
	}, nil
}

// renderRotate builds the commit message for a rotation notice.
func renderRotate(seq int, newKey string) string {
	var b strings.Builder
	b.WriteString("signing key rotation\n\n")
	b.WriteString(trailerKind + ": " + string(KindRotate) + "\n")
	b.WriteString(trailerSeq + ": " + strconv.Itoa(seq) + "\n")
	b.WriteString(trailerKey + ": " + sanitizeValue(newKey) + "\n")
	return b.String()
}
