// SPDX-License-Identifier: Apache-2.0

package feed

import (
	"crypto/ecdh"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

func mustIdentity(t *testing.T) *EncIdentity {
	t.Helper()
	id, err := GenerateEncIdentity()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestWrapUnwrapRoundTrip(t *testing.T) {
	alice, bob := mustIdentity(t), mustIdentity(t)
	key := make([]byte, KeySize)
	for i := range key {
		key[i] = byte(i)
	}

	wrapped, err := WrapKey(mustPub(t, bob.Public()), key)
	if err != nil {
		t.Fatalf("WrapKey: %v", err)
	}
	got, err := UnwrapKey(bob.priv, wrapped)
	if err != nil {
		t.Fatalf("UnwrapKey: %v", err)
	}
	if string(got) != string(key) {
		t.Fatal("the unwrapped key differs from the original")
	}
	// It is not for alice, so alice cannot open it
	if _, err := UnwrapKey(alice.priv, wrapped); err == nil {
		t.Fatal("a non-recipient managed to open it")
	}
}

// Every wrap uses a fresh ephemeral pair, so two wraps of the same key must not
// be comparable -- otherwise an outsider can tell at a glance that the two are
// in the same room.
func TestWrapIsUniquePerCall(t *testing.T) {
	bob := mustIdentity(t)
	pub := mustPub(t, bob.Public())
	key := make([]byte, KeySize)

	a, _ := WrapKey(pub, key)
	b, _ := WrapKey(pub, key)
	if a == b {
		t.Fatal("two wraps produced the same bytes -- the ephemeral key is not randomised")
	}
	for _, w := range []string{a, b} {
		if got, err := UnwrapKey(bob.priv, w); err != nil || string(got) != string(key) {
			t.Fatalf("both wraps should open: %v", err)
		}
	}
}

func TestWrapRejectsWrongKeySize(t *testing.T) {
	bob := mustIdentity(t)
	if _, err := WrapKey(mustPub(t, bob.Public()), []byte("too short")); err == nil {
		t.Fatal("a wrong key length should error")
	}
}

func TestUnwrapRejectsGarbage(t *testing.T) {
	bob := mustIdentity(t)
	for _, bad := range []string{"", "!!!not-base64!!!", "AAAA", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"} {
		if _, err := UnwrapKey(bob.priv, bad); err == nil {
			t.Fatalf("garbage input %q must not be accepted", bad)
		}
	}
}

func TestSealOpenRoundTrip(t *testing.T) {
	key := make([]byte, KeySize)
	key[0] = 7

	for _, plain := range []string{"", "one line", "multi\nline\n\nwith a blank", strings.Repeat("x", 10000)} {
		ct, err := SealBody(key, plain)
		if err != nil {
			t.Fatalf("SealBody: %v", err)
		}
		if plain != "" && strings.Contains(ct, plain) {
			t.Fatal("the plaintext is visible in the ciphertext")
		}
		got, err := OpenBody(key, ct)
		if err != nil {
			t.Fatalf("OpenBody: %v", err)
		}
		if got != plain {
			t.Fatalf("round trip differs: %q vs %q", got, plain)
		}
	}
}

// This is the point of AEAD: flip one byte and it will not open, rather than
// yielding garbage.
func TestOpenRejectsTamperedCiphertext(t *testing.T) {
	key := make([]byte, KeySize)
	ct, _ := SealBody(key, "original text")

	// Flip a bit in the **decoded bytes**, not in the base64 string
	raw, err := base64.StdEncoding.DecodeString(ct)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-2] ^= 0x01
	tampered := base64.StdEncoding.EncodeToString(raw)

	if _, err := OpenBody(key, tampered); !errors.Is(err, ErrBadCiphertext) {
		t.Fatalf("a tampered ciphertext should return ErrBadCiphertext, got %v", err)
	}
}

// A truncated ciphertext must be refused too; half a message must not open.
func TestOpenRejectsTruncatedCiphertext(t *testing.T) {
	key := make([]byte, KeySize)
	ct, _ := SealBody(key, "a long enough message so there is something to truncate")
	raw, _ := base64.StdEncoding.DecodeString(ct)

	for _, cut := range []int{1, len(raw) / 2, len(raw) - 1} {
		short := base64.StdEncoding.EncodeToString(raw[:cut])
		if _, err := OpenBody(key, short); err == nil {
			t.Fatalf("truncating to %d bytes must not open", cut)
		}
	}
}

func TestOpenWithWrongKeyFails(t *testing.T) {
	k1, k2 := make([]byte, KeySize), make([]byte, KeySize)
	k2[0] = 1
	ct, _ := SealBody(k1, "original text")
	if _, err := OpenBody(k2, ct); err == nil {
		t.Fatal("a different key must not open it")
	}
}

// The body key and the wrapping key must derive from different info, and must
// not be usable interchangeably.
func TestBodyAndWrapKeysAreSeparated(t *testing.T) {
	key := make([]byte, KeySize)
	ct, _ := SealBody(key, "original text")

	// Treating a body ciphertext as a wrapped key must fail
	bob := mustIdentity(t)
	if _, err := UnwrapKey(bob.priv, ct); err == nil {
		t.Fatal("a body ciphertext must not unwrap as a wrapped key")
	}
}

func TestIdentitySeedRoundTrip(t *testing.T) {
	id := mustIdentity(t)
	again, err := ParseEncIdentity(id.Seed())
	if err != nil {
		t.Fatal(err)
	}
	if again.Public() != id.Public() {
		t.Fatal("the public key restored from the seed differs")
	}
	if len(id.Seed()) != KeySize {
		t.Fatalf("the seed should be %d bytes", KeySize)
	}
}

func mustPub(t *testing.T, b64 string) *ecdh.PublicKey {
	t.Helper()
	p, err := ParseEncPublic(b64)
	if err != nil {
		t.Fatalf("ParseEncPublic: %v", err)
	}
	return p
}
