// SPDX-License-Identifier: Apache-2.0

package feed

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"immulog/core/gitx"
)

// ── Fixtures: generate an SSH key on the spot, no reliance on the dev machine ──

// signRepo builds a repo configured for SSH signing and returns it with the key path.
func signRepo(t *testing.T) (*gitx.Repo, *Store, string) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")

	base := t.TempDir()
	key := filepath.Join(base, "id_ed25519")
	sshKeygen(t, key)

	// allowed_signers lets git decide "this signature is valid, and whose it is"
	allowed := filepath.Join(base, "allowed_signers")
	pub, err := os.ReadFile(key + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(allowed,
		[]byte("alice@example.com "+strings.TrimSpace(string(pub))+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	dir := filepath.Join(base, "repo.git")
	if err := gitx.Init(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	rawGit(t, dir, "config", "user.name", "alice")
	rawGit(t, dir, "config", "user.email", "alice@example.com")
	rawGit(t, dir, "config", "gpg.format", "ssh")
	rawGit(t, dir, "config", "user.signingkey", key+".pub")
	rawGit(t, dir, "config", "gpg.ssh.allowedSignersFile", allowed)

	repo := gitx.Open(dir)
	s, err := New(context.Background(), repo, FeedID("alice"))
	if err != nil {
		t.Fatal(err)
	}
	return repo, s, key
}

func sshKeygen(t *testing.T, path string) {
	t.Helper()
	cmd := exec.Command("ssh-keygen", "-t", "ed25519", "-N", "", "-C", "alice@example.com", "-f", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v\n%s", err, out)
	}
}

// ── Signing self-test ───────────────────────────────────────────

func TestProbeSigningReturnsFingerprint(t *testing.T) {
	repo, _, _ := signRepo(t)
	fp, err := ProbeSigning(context.Background(), repo)
	if err != nil {
		t.Fatalf("the self-test should pass: %v", err)
	}
	if !strings.HasPrefix(fp, "SHA256:") {
		t.Fatalf("it should return a key fingerprint, got %q", fp)
	}
}

func TestProbeSigningWithoutKey(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	dir := filepath.Join(t.TempDir(), "r.git")
	if err := gitx.Init(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	if _, err := ProbeSigning(context.Background(), gitx.Open(dir)); !errors.Is(err, ErrNoSigningKey) {
		t.Fatalf("with no key configured it should return ErrNoSigningKey, got %v", err)
	}
}

// A key is configured but cannot sign -- this must error, never silently degrade to plaintext.
func TestProbeSigningWithBrokenKeyFailsLoudly(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	dir := filepath.Join(t.TempDir(), "r.git")
	if err := gitx.Init(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	rawGit(t, dir, "config", "user.name", "alice")
	rawGit(t, dir, "config", "user.email", "alice@example.com")
	rawGit(t, dir, "config", "gpg.format", "ssh")
	rawGit(t, dir, "config", "user.signingkey", "/nonexistent/key.pub")

	if _, err := ProbeSigning(context.Background(), gitx.Open(dir)); !errors.Is(err, ErrSigningBroken) {
		t.Fatalf("an unusable key should return ErrSigningBroken, got %v", err)
	}
}

// ── Signatures really land on messages ──────────────────────────

func TestSignedMessagesCarryVerifiableIdentity(t *testing.T) {
	_, s, _ := signRepo(t)
	if !s.Signed() {
		t.Fatal("with a key configured Signed() should be true")
	}

	ctx := context.Background()
	if _, err := s.Send(ctx, "a signed message"); err != nil {
		t.Fatalf("Send: %v", err)
	}

	got, err := s.History(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1, got %d", len(got))
	}
	if got[0].Sig != "signature valid" {
		t.Fatalf("git should judge the signature valid, got %q", got[0].Sig)
	}
	if got[0].Key == "" {
		t.Fatal("it should carry the signing key fingerprint")
	}

	// Consistent with what git itself reads
	raw, _ := s.repo.Log(ctx, s.FeedRef(), 1)
	if raw[0].Sig != "good" {
		t.Fatalf("git's signature status should be good, got %q", raw[0].Sig)
	}
	if !strings.HasSuffix(raw[0].Key, strings.TrimPrefix(got[0].Key, "")) {
		t.Fatalf("fingerprints differ: %q vs %q", raw[0].Key, got[0].Key)
	}
}

// When unsigned it must not claim otherwise.
func TestUnsignedMessagesReportNoSignature(t *testing.T) {
	s, _ := newStore(t)
	mustSend(t, s, "an unsigned message")
	got, _ := s.History(context.Background(), 1)
	if got[0].Sig != "" || got[0].Key != "" {
		t.Fatalf("an unsigned message should carry no signature info: %+v", got[0])
	}
}

// ── The key chain ───────────────────────────────────────────────

// Pure function: construct RawCommit directly, no git needed.
func TestCheckKeyChainRejectsUnexplainedChange(t *testing.T) {
	prev := gitx.RawCommit{OID: "a", Key: "SHA256:old"}
	newer := []gitx.RawCommit{{OID: "b", Key: "SHA256:attacker"}}

	v, err := CheckKeyChain(prev, newer)
	if err != nil {
		t.Fatal(err)
	}
	if v.OK {
		t.Fatal("a key change with no rotation notice behind it must be judged illegitimate")
	}
	if v.Reason != ReasonKeyChanged {
		t.Fatalf("reason should be %q, got %q", ReasonKeyChanged, v.Reason)
	}
	if v.Witness != "SHA256:old" || v.Current != "SHA256:attacker" {
		t.Fatalf("the alarm should carry both keys: %+v", v)
	}
}

func TestCheckKeyChainAllowsDeclaredRotation(t *testing.T) {
	prev := gitx.RawCommit{OID: "a", Key: "SHA256:old"}
	newer := []gitx.RawCommit{
		// Notice: still signed by the old key (Key unchanged), but declaring the new one
		{OID: "b", Key: "SHA256:old", Declared: "SHA256:new"},
		{OID: "c", Key: "SHA256:new"},
	}
	v, err := CheckKeyChain(prev, newer)
	if err != nil {
		t.Fatal(err)
	}
	if !v.OK {
		t.Fatalf("a rotation notice signed by the old key should pass: %+v", v)
	}
}

// The notice declares one key and a third one is used -- equally illegitimate.
func TestCheckKeyChainRejectsMismatchedDeclaration(t *testing.T) {
	prev := gitx.RawCommit{OID: "a", Key: "SHA256:old"}
	newer := []gitx.RawCommit{
		{OID: "b", Key: "SHA256:old", Declared: "SHA256:new"},
		{OID: "c", Key: "SHA256:other"}, // a third key
	}
	v, _ := CheckKeyChain(prev, newer)
	if v.OK || v.Reason != ReasonKeyChanged {
		t.Fatalf("the declared key differs from the one used; should be illegitimate: %+v", v)
	}
}

// An unchanged key keeps the chain continuous by construction.
func TestCheckKeyChainAcceptsStableKey(t *testing.T) {
	prev := gitx.RawCommit{OID: "a", Key: "SHA256:k"}
	newer := []gitx.RawCommit{{OID: "b", Key: "SHA256:k"}, {OID: "c", Key: "SHA256:k"}}
	if v, _ := CheckKeyChain(prev, newer); !v.OK {
		t.Fatalf("an unchanged key should pass: %+v", v)
	}
}

// An unsigned history has nothing to vouch for it: the first key seen is accepted but must be labelled honestly.
func TestCheckKeyChainAcceptsFirstKeyOnUnsignedHistory(t *testing.T) {
	prev := gitx.RawCommit{OID: "a"} // unsigned
	newer := []gitx.RawCommit{{OID: "b", Key: "SHA256:first"}}
	if v, _ := CheckKeyChain(prev, newer); !v.OK {
		t.Fatalf("the first signature after an unsigned prefix should be accepted: %+v", v)
	}
}

// ── Rotation notices ────────────────────────────────────────────

func TestDeclareKeyChainsAndDeclares(t *testing.T) {
	_, s, _ := signRepo(t)
	ctx := context.Background()

	mustSend(t, s, "a message under the old key")
	before, _ := s.CurrentKey(ctx)
	if before == "" {
		t.Fatal("precondition: there should already be a signing key")
	}

	ann, err := s.DeclareKey(ctx, "SHA256:brand-new-key")
	if err != nil {
		t.Fatalf("DeclareKey: %v", err)
	}
	if ann.Kind != KindRotate {
		t.Fatalf("it should be a rotation notice: %+v", ann)
	}

	raw, _ := s.repo.Log(ctx, s.FeedRef(), 1)
	if raw[0].Declared != "SHA256:brand-new-key" {
		t.Fatalf("the notice should declare the new key, trailer = %q", raw[0].Declared)
	}
	// The notice itself is signed by the **old** key
	if raw[0].Key != before {
		t.Fatalf("the notice should be signed by the old key: %q vs %q", raw[0].Key, before)
	}

	// Chain check: notice plus a later commit under the new key -- legitimate
	newer := []gitx.RawCommit{
		raw[0],
		{OID: "next", Key: "SHA256:brand-new-key"},
	}
	if v, _ := CheckKeyChain(gitx.RawCommit{OID: "prev", Key: before}, newer); !v.OK {
		t.Fatalf("a legitimate rotation should pass the chain check: %+v", v)
	}
}

func TestDeclareKeyRequiresSigning(t *testing.T) {
	s, _ := newStore(t) // no key configured
	mustSend(t, s, "placeholder")
	if _, err := s.DeclareKey(context.Background(), "SHA256:x"); !errors.Is(err, ErrNoSigningKey) {
		t.Fatalf("rotation should be refused with no key configured, got %v", err)
	}
}

func TestDeclareKeyRejectsSameKey(t *testing.T) {
	_, s, _ := signRepo(t)
	ctx := context.Background()
	mustSend(t, s, "a start")
	cur, _ := s.CurrentKey(ctx)

	if _, err := s.DeclareKey(ctx, cur); err == nil {
		t.Fatal("the same old and new key should be refused")
	}
	if _, err := s.DeclareKey(ctx, "  "); err == nil {
		t.Fatal("an empty new key should be refused")
	}
}

// A rotation notice is a structural event and should not appear in the timeline.
func TestRotateIsNotATimelineEvent(t *testing.T) {
	_, s, _ := signRepo(t)
	ctx := context.Background()
	mustSend(t, s, "one message")
	if _, err := s.DeclareKey(ctx, "SHA256:next-key"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.History(ctx, 10)
	if len(got) != 2 {
		t.Fatalf("the chain should hold 2, got %d", len(got))
	}
	if got[0].Kind != KindRotate {
		t.Fatalf("the tip should be a rotation notice: %+v", got[0])
	}
}
