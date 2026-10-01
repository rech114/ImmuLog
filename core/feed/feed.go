// SPDX-License-Identifier: Apache-2.0

// Package feed is ImmuLog's domain layer.
//
// A feed is one user's append-only message chain, materialised as
// refs/feeds/<pub>. Every message is a commit object, and structural metadata
// lives in commit trailers (native git syntax: human-readable and
// machine-parseable). A retraction is an **appended event**; the original
// object is never deleted -- see docs/DESIGN.md §6.1.
//
// This package never touches os/exec (only gitx does) and never touches HTTP
// (only web calls in).
package feed

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	"immulog/core/gitx"
)

// Kind is the message type. Events are distinguished by kind rather than by
// separate endpoints (§7.2).
type Kind string

const (
	KindMsg     Kind = "msg"
	KindRetract Kind = "retract"
	KindRotate  Kind = "rotate" // signing key rotation notice
	KindEpoch   Kind = "epoch"  // encryption epoch record
	KindShred   Kind = "shred"  // discard one epoch's key
)

// Trailer keys. Changing these changes the protocol.
const (
	trailerKind     = "ImmuLog-Kind"
	trailerSeq      = "ImmuLog-Seq"
	trailerRetracts = "ImmuLog-Retracts"
	trailerReason   = "ImmuLog-Reason"
	trailerKey      = "ImmuLog-Key"
	trailerEpoch    = "ImmuLog-Epoch"
	trailerEnc      = "ImmuLog-Enc"
)

// MaxBody is the upper bound for one message body.
const MaxBody = 8192

// MaxReason is the upper bound for a retraction reason.
const MaxReason = 512

// ErrTooLong means the body is oversized.
var ErrTooLong = errors.New("message is too long")

// ErrNoTarget means a retraction carried no target object.
var ErrNoTarget = errors.New("retraction target is missing")

// ErrBadTarget means the retraction target is not a valid object name.
var ErrBadTarget = errors.New("retraction target is malformed")

// ErrNotMine means the retraction target is not in this machine's feed -- you
// may only retract what you yourself said.
var ErrNotMine = errors.New("you may only retract messages in your own feed")

// ErrEmpty means the body is empty.
var ErrEmpty = errors.New("body must not be empty")

// Message is a message as sent to the frontend.
type Message struct {
	OID      string    `json:"oid"`
	Seq      int       `json:"seq"`
	Author   string    `json:"author"`
	Feed     string    `json:"feed,omitempty"`
	Body     string    `json:"body"`
	Sig      string    `json:"sig,omitempty"`
	Key      string    `json:"key,omitempty"` // signing key fingerprint (short)
	Kind     Kind      `json:"kind"`
	Retracts string    `json:"retracts,omitempty"`
	Reason   string    `json:"reason,omitempty"`
	Epoch    int       `json:"epoch,omitempty"`  // encryption epoch; 0 means plaintext
	Locked   bool      `json:"locked,omitempty"` // has an epoch but cannot be opened here
	At       time.Time `json:"at"`
}

// FeedID derives a feed identifier from an identity.
//
// The output is always hexadecimal, and therefore inherently a safe ref name:
// no character in an identity can become a path traversal or a ref injection.
// A different identity means a different feed, which is exactly what we want.
func FeedID(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:8])
}

// Store binds one feed. All writes are serialised in-process -- there is only
// one local writer, so serialising removes self-contention; outward
// correctness still rests on CAS (see append).
type Store struct {
	repo *gitx.Repo
	pub  string
	ref  string
	name string
	sign bool

	mu   sync.Mutex
	tree string // the empty tree's object name, computed once and reused

	// Cache of the current encryption epoch. The write path needs it for every
	// message, so it cannot afford to walk refs/keys/* each time (that would be
	// N process calls).
	epoch    int
	epochKey []byte
}

// New validates the git identity and binds a feed. Identity comes only from git
// config, never from a request body (§4.2).
func New(ctx context.Context, repo *gitx.Repo, pub string) (*Store, error) {
	name, _, err := repo.Identity(ctx)
	if err != nil {
		return nil, err
	}
	key, err := repo.SigningKey(ctx)
	if err != nil {
		return nil, err
	}
	return &Store{
		repo: repo,
		pub:  pub,
		ref:  FeedRef(pub),
		name: name,
		sign: key != "",
	}, nil
}

// Pub returns this feed's owner identifier.
func (s *Store) Pub() string { return s.pub }

// FeedRef returns this feed's ref name.
func (s *Store) FeedRef() string { return s.ref }

// Signed reports whether this feed signs its commits.
func (s *Store) Signed() bool { return s.sign }

// Send appends an ordinary message.
func (s *Store) Send(ctx context.Context, body string) (Message, error) {
	if strings.TrimSpace(body) == "" {
		return Message{}, ErrEmpty
	}
	if len(body) > MaxBody {
		return Message{}, ErrTooLong
	}
	return s.append(ctx, KindMsg, body, "", "")
}

// Retract appends a retraction event. The original message object is never
// deleted.
//
// Only messages **in your own feed** may be retracted: the target must be an
// ancestor of this machine's tip. That rule makes retraction the author's
// right, rather than something anyone can do to anyone.
func (s *Store) Retract(ctx context.Context, oid, reason string) (Message, error) {
	if oid == "" {
		return Message{}, ErrNoTarget
	}
	if !isOID(oid) {
		return Message{}, ErrBadTarget
	}
	if len(reason) > MaxReason {
		reason = reason[:MaxReason]
	}

	s.mu.Lock()
	tip, err := s.repo.Resolve(ctx, s.ref)
	s.mu.Unlock()
	if err != nil {
		return Message{}, err
	}
	if tip == "" {
		return Message{}, ErrNotMine
	}
	// Fail closed: a missing object (IsAncestor reports 128) or a non-ancestor
	// both mean "not yours"
	exists, err := s.repo.Exists(ctx, oid)
	if err != nil {
		return Message{}, err
	}
	if !exists {
		return Message{}, ErrNotMine
	}
	ok, err := s.repo.IsAncestor(ctx, oid, tip)
	if err != nil {
		return Message{}, err
	}
	if !ok {
		return Message{}, ErrNotMine
	}

	return s.append(ctx, KindRetract, "", oid, reason)
}

// History reads back the most recent messages (a single process call).
func (s *Store) History(ctx context.Context, limit int) ([]Message, error) {
	raw, err := s.repo.Log(ctx, s.ref, limit)
	if err != nil {
		return nil, err
	}
	return Decode(raw, s.ref, s.OpenBody), nil
}

// Tip returns the current chain tip; empty string for an empty feed.
func (s *Store) Tip(ctx context.Context) (string, error) {
	return s.repo.Resolve(ctx, s.ref)
}

// ── internal ────────────────────────────────────────────────────────

func (s *Store) append(ctx context.Context, kind Kind, body, retracts, reason string) (Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tip, err := s.repo.Resolve(ctx, s.ref)
	if err != nil {
		return Message{}, err
	}
	seq, err := s.nextSeq(ctx, tip)
	if err != nil {
		return Message{}, err
	}
	tree, err := s.emptyTree(ctx)
	if err != nil {
		return Message{}, err
	}

	env := envelope{kind: kind, body: body, seq: seq, retracts: retracts, reason: reason}

	// Encrypt the body when an epoch exists. The ciphertext goes into a git
	// object (freely copyable) while the plaintext key stays on this machine --
	// that dividing line is the entire point of the encryption phase.
	if n, key := s.currentEpochLocked(ctx); n > 0 && key != nil {
		ct, err := SealBody(key, body)
		if err != nil {
			return Message{}, err
		}
		env.body, env.epoch = ct, n
	}
	// Every message carries its author's encryption public key, so elsewhere
	// knows who to wrap for
	if me, err := LoadIdentity(s.repo.Dir); err == nil {
		env.enc = me.Public()
	}

	oid, err := s.repo.Commit(ctx, tree, tip, render(env), s.sign)
	if err != nil {
		return Message{}, err
	}

	// CAS: abort when old does not match. This is the signal source for tamper
	// detection and must be surfaced verbatim.
	if err := s.repo.UpdateRef(ctx, s.ref, oid, tip); err != nil {
		return Message{}, err
	}
	// We just created this commit ourselves, so by construction it descends
	// from the old tip -- advance the witness anchor directly.
	_ = s.advanceWitness(ctx, oid)

	return Message{
		OID: oid, Seq: seq, Author: s.name, Body: body,
		Kind: kind, Retracts: retracts, Reason: reason, Epoch: env.epoch,
		At: time.Now().UTC(),
	}, nil
}

// currentEpochLocked returns the current epoch and its plaintext key
// (0, nil means encryption is off). The caller must hold s.mu.
func (s *Store) currentEpochLocked(ctx context.Context) (int, []byte) {
	if s.epoch > 0 {
		return s.epoch, s.epochKey
	}
	n, err := s.CurrentEpoch(ctx)
	if err != nil {
		return 0, nil
	}
	key, err := s.OpenEpoch(ctx, n)
	if err != nil {
		return 0, nil
	}
	s.epoch, s.epochKey = n, key
	return n, key
}

func (s *Store) nextSeq(ctx context.Context, tip string) (int, error) {
	if tip == "" {
		return 1, nil
	}
	if raw, err := s.repo.Log(ctx, s.ref, 1); err != nil {
		return 0, err
	} else if len(raw) > 0 {
		if n, err := strconv.Atoi(raw[0].Seq); err == nil && n > 0 {
			return n + 1, nil
		}
	}
	// A foreign commit without a Seq trailer: fall back to chain length
	n, err := s.repo.Count(ctx, s.ref)
	return n + 1, err
}

func (s *Store) emptyTree(ctx context.Context) (string, error) {
	if s.tree != "" {
		return s.tree, nil
	}
	t, err := s.repo.EmptyTree(ctx)
	if err != nil {
		return "", err
	}
	s.tree = t
	return t, nil
}

// envelope is one message about to be encoded. A struct rather than six
// positional arguments -- after epoch and enc arrived, the argument list became
// impossible to read.
type envelope struct {
	kind     Kind
	body     string // plaintext, or already-sealed ciphertext
	seq      int
	retracts string
	reason   string
	epoch    int    // 0 = plaintext
	enc      string // this machine's encryption public key (non-empty when enabled)
}

// render encodes one message into a commit message.
//
// The body and the trailer block are always separated by one blank line, and
// the body's trailing whitespace is stripped -- so decoding can treat
// "everything before the last blank line" as the body, with a deterministic
// boundary. git's trailer parser takes the **last** occurrence, so a forged
// trailer inside the body has no effect.
//
// The first paragraph must never be empty: otherwise the commit message starts
// with a blank line and git's trailer parsing breaks.
//
// ⚠️ Trailer values must go through sanitizeValue: a single newline inside a
// value fabricates a line like `ImmuLog-Seq: 999`, and since git takes the last
// occurrence -- **the forgery wins**.
func render(env envelope) string {
	var head string
	switch env.kind {
	case KindRetract:
		head = sanitizeValue(env.reason)
		if head == "" {
			head = "retract " + short(oidOr(env.retracts))
		}
	default:
		head = sanitizeText(env.body) // bodies keep newlines; multi-line is legal
	}
	if head == "" {
		head = "(empty message)"
	}

	var b strings.Builder
	b.WriteString(head)
	b.WriteString("\n\n")
	b.WriteString(trailerKind + ": " + string(env.kind) + "\n")
	b.WriteString(trailerSeq + ": " + strconv.Itoa(env.seq) + "\n")
	if env.retracts != "" {
		b.WriteString(trailerRetracts + ": " + sanitizeValue(env.retracts) + "\n")
	}
	if env.reason != "" {
		b.WriteString(trailerReason + ": " + sanitizeValue(env.reason) + "\n")
	}
	if env.epoch > 0 {
		b.WriteString(trailerEpoch + ": " + strconv.Itoa(env.epoch) + "\n")
	}
	if env.enc != "" {
		b.WriteString(trailerEnc + ": " + sanitizeValue(env.enc) + "\n")
	}
	return b.String()
}

// BodyOpener decrypts one epoch's ciphertext. nil means plaintext only.
type BodyOpener func(epoch int, ciphertext string) (string, error)

// Decode translates raw gitx commits into domain messages.
//
// feedRef determines ownership -- retractions only apply to messages **in the
// same feed** (see docs/DESIGN.md §6.1). When open is nil or fails, the message
// is marked Locked: **a lost key does not mean the message never existed**. Its
// position, author and timestamp are all still there; only the body is no
// longer readable.
func Decode(raw []gitx.RawCommit, feedRef string, open BodyOpener) []Message {
	out := make([]Message, 0, len(raw))
	for _, r := range raw {
		seq, _ := strconv.Atoi(r.Seq)
		kind := Kind(r.Kind)
		if kind == "" {
			kind = KindMsg
			if r.Retracts != "" {
				kind = KindRetract
			}
		}
		epoch, _ := strconv.Atoi(r.Epoch)

		m := Message{
			OID:      r.OID,
			Seq:      seq,
			Author:   r.Author,
			Feed:     feedRef,
			Body:     bodyOf(r.Body),
			Sig:      sigLabel(r.Sig),
			Key:      ShortKey(r.Key),
			Kind:     kind,
			Retracts: r.Retracts,
			Reason:   r.Reason,
			Epoch:    epoch,
			At:       r.At,
		}
		if epoch > 0 {
			plain, err := "", ErrNoKey
			if open != nil {
				plain, err = open(epoch, m.Body)
			}
			if err != nil {
				m.Locked, m.Body = true, ""
			} else {
				m.Body = plain
			}
		}
		out = append(out, m)
	}
	return out
}

// OpenBody implements BodyOpener using the epoch keys held on this machine.
func (s *Store) OpenBody(epoch int, ciphertext string) (string, error) {
	key, err := s.OpenEpoch(context.Background(), epoch)
	if err != nil {
		return "", err
	}
	return OpenBody(key, ciphertext)
}

// bodyOf strips the trailing trailer block and returns the clean body.
func bodyOf(raw string) string {
	s := strings.TrimRight(strings.ReplaceAll(raw, "\r\n", "\n"), " \t\n")
	if s == "" {
		return ""
	}
	if i := strings.LastIndex(s, "\n\n"); i >= 0 {
		tail := s[i+2:]
		if looksLikeTrailers(tail) {
			return s[:i]
		}
	}
	return s
}

func looksLikeTrailers(block string) bool {
	for _, line := range strings.Split(block, "\n") {
		if line == "" {
			continue
		}
		k, _, ok := strings.Cut(line, ":")
		if !ok || !strings.HasPrefix(k, "ImmuLog-") {
			return false
		}
	}
	return true
}

// sanitizeText removes control characters that would break record separation
// and strips trailing whitespace. **Newlines are preserved** -- multi-line
// bodies are legal.
func sanitizeText(s string) string {
	s = strings.ReplaceAll(s, "\x00", "")
	s = strings.ReplaceAll(s, "\x1f", "")
	s = strings.ReplaceAll(s, "\x1e", "")
	return strings.TrimRight(s, " \t\r\n")
}

// sanitizeValue collapses content to a single line, for trailer values.
//
// This is a security boundary: a newline inside a trailer value is enough to
// fabricate a line like `ImmuLog-Seq: 999`, and git's trailer parser takes the
// **last** occurrence -- so the forgery would override the real one. Collapsing
// to one line removes the class entirely.
func sanitizeValue(s string) string {
	s = sanitizeText(s)
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.TrimSpace(s)
}

// isOID reports whether s is a 40-character hex object name.
// Retraction targets come from a request body and must be validated strictly --
// otherwise it is another injection surface.
func isOID(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}

// sigLabel turns git's signature status into something a person reads. Empty
// means unsigned.
func sigLabel(status string) string {
	switch status {
	case "good":
		return "signature valid"
	case "untrusted":
		return "signature valid (unknown key)"
	case "bad":
		return "signature broken"
	default:
		return ""
	}
}

// ShortKey keeps only the tail of a fingerprint -- enough for a human to tell
// keys apart.
func ShortKey(k string) string {
	if len(k) > 16 {
		return k[len(k)-16:]
	}
	return k
}

func oidOr(s string) string {
	if s == "" {
		return "?"
	}
	return s
}

func short(oid string) string {
	if len(oid) > 6 {
		return oid[:6]
	}
	return oid
}
