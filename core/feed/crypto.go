// SPDX-License-Identifier: Apache-2.0

package feed

// crypto.go -- epoch keys and per-recipient wrapping.
//
// Everything comes from the standard library: crypto/ecdh (X25519),
// crypto/hkdf (RFC 5869), crypto/aes (GCM) and crypto/rand. No primitives are
// hand-rolled, and there are no third-party dependencies.
//
// Why per-recipient wrapping:
// integrity and privacy make **opposite** demands here -- integrity wants copies
// to spread as widely as possible, privacy wants key distribution far narrower
// than the ciphertext. Wrapping the epoch key once per member's public key
// satisfies both: the ciphertext replicates freely with the repository, and
// **only members can open it**.

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
)

// KeySize is the symmetric key length.
const KeySize = 32

// encInfo binds the HKDF-derived key to the "wrapping" use case.
var encInfo = []byte("immulog/epoch-wrap/v1")

// bodyInfo keeps body-encryption keys separate from wrapping keys.
var bodyInfo = []byte("immulog/body/v1")

var (
	// ErrNoKey means this machine does not have that epoch's key.
	ErrNoKey = errors.New("no key for that epoch on this machine")
	// ErrNotRecipient means the wrapped copy is not ours -- plainly, "you are
	// not a member".
	ErrNotRecipient = errors.New("this key was not wrapped for you")
	// ErrBadCiphertext means the ciphertext was altered or is malformed (AEAD
	// authentication failure lands here).
	ErrBadCiphertext = errors.New("ciphertext failed authentication")
)

// ── Encryption identity ─────────────────────────────────────────────

// EncIdentity is an X25519 key pair, used to **receive** epoch keys wrapped for
// us.
//
// It is unrelated to the signing key: signing uses git's ssh signing, while
// encryption uses this X25519 pair. Keeping them apart is deliberate -- signing
// keys usually live in ssh-agent, where the raw private bytes are unavailable.
type EncIdentity struct {
	priv *ecdh.PrivateKey
}

// GenerateEncIdentity creates a fresh encryption identity.
func GenerateEncIdentity() (*EncIdentity, error) {
	p, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return &EncIdentity{priv: p}, nil
}

// ParseEncIdentity restores an identity from a 32-byte seed (for persisting the
// private key).
func ParseEncIdentity(seed []byte) (*EncIdentity, error) {
	if len(seed) != KeySize {
		return nil, fmt.Errorf("encryption seed must be %d bytes, got %d", KeySize, len(seed))
	}
	p, err := ecdh.X25519().NewPrivateKey(seed)
	if err != nil {
		return nil, err
	}
	return &EncIdentity{priv: p}, nil
}

// Seed exports the 32-byte private key seed.
func (e *EncIdentity) Seed() []byte { return e.priv.Bytes() }

// Public returns the base64 public key, ready to be written into a trailer.
func (e *EncIdentity) Public() string {
	return base64.StdEncoding.EncodeToString(e.priv.PublicKey().Bytes())
}

// ParseEncPublic parses a base64 public key.
func ParseEncPublic(s string) (*ecdh.PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("public key is not valid base64: %w", err)
	}
	return ecdh.X25519().NewPublicKey(raw)
}

// ── Per-recipient wrapping ──────────────────────────────────────────

// WrapKey wraps an epoch key for one recipient public key.
//
// The construction is the standard ECIES shape:
//
//	ephemeral <- a fresh X25519 key pair
//	shared    <- ECDH(ephemeral, recipient)
//	wrapKey   <- HKDF-SHA256(shared, info="immulog/epoch-wrap/v1")
//	output    <- base64(ephemeralPub ‖ nonce ‖ AES-GCM(wrapKey, key))
//
// Every wrap uses a fresh ephemeral pair, so two wraps of the same epoch key
// for different members are not comparable -- an outsider cannot tell that the
// two are in the same room.
func WrapKey(recipient *ecdh.PublicKey, key []byte) (string, error) {
	if len(key) != KeySize {
		return "", fmt.Errorf("key must be %d bytes", KeySize)
	}
	eph, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return "", err
	}
	shared, err := eph.ECDH(recipient)
	if err != nil {
		return "", err
	}
	wk, err := hkdf.Key(sha256.New, shared, nil, string(encInfo), KeySize)
	if err != nil {
		return "", err
	}
	sealed, err := seal(wk, key)
	if err != nil {
		return "", err
	}
	out := append(append([]byte{}, eph.PublicKey().Bytes()...), sealed...)
	return base64.StdEncoding.EncodeToString(out), nil
}

// UnwrapKey recovers the epoch key with a private key. A wrap intended for
// someone else returns ErrNotRecipient.
func UnwrapKey(priv *ecdh.PrivateKey, wrapped string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(wrapped)
	if err != nil {
		return nil, fmt.Errorf("wrapped key is not valid base64: %w", err)
	}
	if len(raw) <= 32 {
		return nil, ErrNotRecipient
	}
	ephPub, err := ecdh.X25519().NewPublicKey(raw[:32])
	if err != nil {
		return nil, ErrNotRecipient
	}
	shared, err := priv.ECDH(ephPub)
	if err != nil {
		// X25519 rejects low-order points -- in other words, "this is not for you"
		return nil, ErrNotRecipient
	}
	wk, err := hkdf.Key(sha256.New, shared, nil, string(encInfo), KeySize)
	if err != nil {
		return nil, err
	}
	return open(wk, raw[32:])
}

// ── Body sealing ────────────────────────────────────────────────────

// SealBody encrypts a body with an epoch key and returns base64(nonce‖ciphertext).
//
// The body key is HKDF-derived from the epoch key and is **different** from the
// wrapping key -- so even if a wrapping output were misused somewhere, it still
// would not open a body.
func SealBody(key []byte, plaintext string) (string, error) {
	bk, err := hkdf.Key(sha256.New, key, nil, string(bodyInfo), KeySize)
	if err != nil {
		return "", err
	}
	sealed, err := seal(bk, []byte(plaintext))
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// OpenBody decrypts a body. A tampered ciphertext returns ErrBadCiphertext.
func OpenBody(key []byte, b64 string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return "", fmt.Errorf("ciphertext is not valid base64: %w", err)
	}
	bk, err := hkdf.Key(sha256.New, key, nil, string(bodyInfo), KeySize)
	if err != nil {
		return "", err
	}
	plain, err := open(bk, raw)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

// ── AEAD ────────────────────────────────────────────────────────────

func seal(key, plaintext []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

func open(key, blob []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	n := gcm.NonceSize()
	if len(blob) < n {
		return nil, ErrBadCiphertext
	}
	out, err := gcm.Open(nil, blob[:n], blob[n:], nil)
	if err != nil {
		return nil, ErrBadCiphertext
	}
	return out, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
