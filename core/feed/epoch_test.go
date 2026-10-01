// SPDX-License-Identifier: Apache-2.0

package feed

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"immulog/core/gitx"
)

// ── Fixtures ────────────────────────────────────────────────────

// encStore builds a Store with encryption enabled (its own identity plus epoch 1).
func encStore(t *testing.T) (*Store, *gitx.Repo, string) {
	t.Helper()
	repo, dir := node(t, "alice")
	s := storeOf(t, repo, "alice")
	if err := s.SetupEncryption(context.Background()); err != nil {
		t.Fatalf("SetupEncryption: %v", err)
	}
	return s, repo, dir
}

// actAs switches this machine's identity to someone else and deletes every local
// epoch key -- simulating "another machine just got this repository and must
// unwrap to read anything".
func actAs(t *testing.T, repoDir string, id *EncIdentity) {
	t.Helper()
	if err := SaveIdentity(repoDir, id); err != nil {
		t.Fatal(err)
	}
	matches, _ := filepath.Glob(filepath.Join(repoDir, keysDir, "epoch-*"))
	for _, m := range matches {
		if err := os.Remove(m); err != nil {
			t.Fatal(err)
		}
	}
}

// rawCommitBody reads the raw body straight out of the git object -- used to
// prove there is no plaintext in the ciphertext.
func rawCommitBody(t *testing.T, dir, oid string) string {
	t.Helper()
	return rawGit(t, dir, "cat-file", "commit", oid)
}

// ── Setup and idempotence ───────────────────────────────────────

func TestSetupEncryptionIsIdempotent(t *testing.T) {
	s, _, _ := encStore(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if err := s.SetupEncryption(ctx); err != nil {
			t.Fatal(err)
		}
	}
	epochs, err := s.Epochs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(epochs) != 1 {
		t.Fatalf("repeated Setup must not conjure more epochs, got %d", len(epochs))
	}
	if epochs[0].N != 1 || epochs[0].Members != 1 || !epochs[0].Held {
		t.Fatalf("epoch 1 has the wrong shape: %+v", epochs[0])
	}
}

func TestBeforeSetupThereIsNoEpoch(t *testing.T) {
	repo, _ := node(t, "alice")
	s := storeOf(t, repo, "alice")
	if _, err := s.CurrentEpoch(context.Background()); !errors.Is(err, ErrNoEpoch) {
		t.Fatalf("with encryption off it should return ErrNoEpoch, got %v", err)
	}
}

// ── Encryption at rest ──────────────────────────────────────────

func TestSendEncryptsBodyInGitObject(t *testing.T) {
	s, _, dir := encStore(t)
	ctx := context.Background()

	secret := "this sentence must not appear in a git object"
	m, err := s.Send(ctx, secret)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if m.Epoch != 1 {
		t.Fatalf("the message should carry an epoch number, got %d", m.Epoch)
	}

	// 1) no plaintext may be visible in the git object
	raw := rawCommitBody(t, dir, m.OID)
	if strings.Contains(raw, secret) {
		t.Fatal("plaintext appeared in the commit object -- encryption is not working")
	}
	// 2) but the metadata stays: position on the chain, author and time are all there
	for _, want := range []string{"ImmuLog-Kind: msg", "ImmuLog-Seq: 1", "ImmuLog-Epoch: 1"} {
		if !strings.Contains(raw, want) {
			t.Errorf("metadata %q must not be encrypted away (or the chain stops being verifiable)", want)
		}
	}
	// 3) this machine reads it back as plaintext
	got, _ := s.History(ctx, 1)
	if len(got) != 1 || got[0].Body != secret {
		t.Fatalf("this machine should decrypt it: %+v", got)
	}
	if got[0].Locked {
		t.Fatal("this machine holds the key, so it must not be Locked")
	}
}

func TestPlaintextHistoryStaysReadable(t *testing.T) {
	repo, _ := node(t, "alice")
	ctx := context.Background()
	s := storeOf(t, repo, "alice")

	mustSend(t, s, "said before encryption") // plaintext
	if err := s.SetupEncryption(ctx); err != nil {
		t.Fatal(err)
	}
	mustSend(t, s, "said after encryption")

	got, _ := s.History(ctx, 10)
	if len(got) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(got))
	}
	if got[1].Body != "said before encryption" || got[1].Epoch != 0 {
		t.Fatalf("plaintext history should read back unchanged: %+v", got[1])
	}
	if got[0].Body != "said after encryption" || got[0].Epoch != 1 {
		t.Fatalf("the encrypted message should be readable: %+v", got[0])
	}
}

// ── Core property 1: a late joiner cannot read epochs from before they joined ──

func TestLateJoinerCannotReadOldEpoch(t *testing.T) {
	s, _, dir := encStore(t)
	ctx := context.Background()

	// epoch 1: alice only
	mustSend(t, s, "a secret from before the join")
	before, _ := s.History(ctx, 1)
	if before[0].Body != "a secret from before the join" {
		t.Fatal("precondition: alice herself should be able to read it")
	}

	// bob joins with his public key; alice rotates and wraps epoch 2 for him
	bob, err := GenerateEncIdentity()
	if err != nil {
		t.Fatal(err)
	}
	e2, err := s.RotateEpoch(ctx, []string{bob.Public()})
	if err != nil {
		t.Fatalf("RotateEpoch: %v", err)
	}
	if e2.N != 2 || e2.Members != 2 {
		t.Fatalf("epoch 2 should have 2 recipients: %+v", e2)
	}
	mustSend(t, s, "a public message from after the join")

	// Switch to bob's machine: only his private key, and no epoch key locally
	actAs(t, dir, bob)
	bobStore := storeOf(t, s.repo, "alice")

	got, err := bobStore.History(ctx, 10)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(got))
	}

	// bob cannot read epoch 1
	if !got[1].Locked || got[1].Body != "" {
		t.Fatalf("bob must not read the pre-join message: %+v", got[1])
	}
	if got[1].OID == "" || got[1].Seq != 1 {
		t.Error("the body is unreadable, but the message existence and position must stay visible")
	}
	// bob can read epoch 2 -- the wrap was made for him
	if got[0].Locked || got[0].Body != "a public message from after the join" {
		t.Fatalf("bob should read epoch 2: %+v", got[0])
	}
}

// Someone who is not a recipient cannot unwrap what is in the epoch record.
func TestNonMemberCannotUnwrap(t *testing.T) {
	s, repo, dir := encStore(t)
	ctx := context.Background()
	mustSend(t, s, "members only")

	carol, _ := GenerateEncIdentity()
	actAs(t, dir, carol)

	if _, err := openEpochKey(ctx, repo, 1); !errors.Is(err, ErrNotRecipient) {
		t.Fatalf("a non-member should get ErrNotRecipient, got %v", err)
	}
	got, _ := storeOf(t, repo, "alice").History(ctx, 1)
	if !got[0].Locked {
		t.Fatal("a non-member must not read the body")
	}
}

// ── Core property 2: discarding a key leaves the ciphertext, unreadable ──

func TestShredMakesBodyUnreadableButKeepsTheRecord(t *testing.T) {
	s, repo, dir := encStore(t)
	ctx := context.Background()

	m, err := s.Send(ctx, "say it and forget it")
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := s.History(ctx, 1); got[0].Body != "say it and forget it" {
		t.Fatal("precondition: readable before the discard")
	}

	if _, err := s.ShredEpoch(ctx, 1); err != nil {
		t.Fatalf("ShredEpoch: %v", err)
	}

	// 1) this machine can no longer open it
	got, _ := s.History(ctx, 10)
	if !got[0].Locked || got[0].Body != "" {
		t.Fatalf("it must not stay readable after the key is discarded: %+v", got[0])
	}
	// 2) but the ciphertext is still in the git object -- not deleted, unreadable
	raw := rawCommitBody(t, dir, m.OID)
	if len(raw) < 100 {
		t.Fatal("the commit object must not vanish")
	}
	// 3) the chain itself is intact: the witness anchor still points at the tip
	if v, _ := s.Verify(ctx); !v.OK {
		t.Fatalf("discarding a key must not break integrity: %+v", v)
	}
	if _, err := openEpochKey(ctx, repo, 1); err == nil {
		t.Fatal("this machine must not still hold the epoch 1 key")
	}
}

// The discard itself must be traceable and auditable -- that is "provable
// forgetting".
func TestShredIsAuditableOnChain(t *testing.T) {
	s, _, dir := encStore(t)
	ctx := context.Background()
	mustSend(t, s, "words to be forgotten")

	ann, err := s.ShredEpoch(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if ann.Kind != KindShred || ann.Epoch != 1 {
		t.Fatalf("the discard notice has the wrong shape: %+v", ann)
	}

	raw := rawGit(t, dir, "log", "-1", "--format=%B", ann.OID)
	for _, want := range []string{"ImmuLog-Kind: shred", "ImmuLog-Epoch: 1"} {
		if !strings.Contains(raw, want) {
			t.Errorf("the notice should contain %q, actually: %q", want, raw)
		}
	}
}

func TestShredUnknownEpochFails(t *testing.T) {
	s, _, _ := encStore(t)
	if _, err := s.ShredEpoch(context.Background(), 99); !errors.Is(err, ErrNoKey) {
		t.Fatalf("discarding a non-existent epoch should return ErrNoKey, got %v", err)
	}
}

// ── The rotation chain ──────────────────────────────────────────

func TestRotateChainsEpochRecords(t *testing.T) {
	s, repo, _ := encStore(t)
	ctx := context.Background()
	mustSend(t, s, "one")

	if _, err := s.RotateEpoch(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RotateEpoch(ctx, nil); err != nil {
		t.Fatal(err)
	}

	epochs, _ := s.Epochs(ctx)
	if len(epochs) != 3 {
		t.Fatalf("expected 3 epochs, got %d", len(epochs))
	}
	for i := 1; i < len(epochs); i++ {
		parent := rawGit(t, repo.Dir, "rev-parse", epochs[i].OID+"^")
		if parent != epochs[i-1].OID {
			t.Fatalf("epoch records must chain: epoch %d predecessor is not epoch %d", epochs[i].N, epochs[i-1].N)
		}
	}

	// A new message uses the newest epoch
	m := mustSend(t, s, "two")
	if m.Epoch != 3 {
		t.Fatalf("a new message should use the newest epoch, got %d", m.Epoch)
	}
	// This machine still holds the the keys of older epochs (they were not discarded)
	if !HasEpochKey(repo.Dir, 1) {
		t.Error("rotation must not throw away old keys -- that would make all history unreadable")
	}
	got, _ := s.History(ctx, 10)
	for _, msg := range got {
		if msg.Locked {
			t.Fatalf("history should stay readable after rotation: %+v", msg)
		}
	}
}

func TestKnownRecipientsIncludesSelfAndSeenPeers(t *testing.T) {
	s, _, _ := encStore(t)
	ctx := context.Background()
	mustSend(t, s, "something I said")

	me, err := LoadIdentity(s.repo.Dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.KnownRecipients(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != me.Public() {
		t.Fatalf("it should recognise only this machine, got %v", got)
	}
}
