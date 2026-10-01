// SPDX-License-Identifier: Apache-2.0

package feed

// epoch.go -- on-chain records and the lifecycle of encryption epochs.
//
// An epoch is a span of messages covered by one symmetric key. The key is
// **not** here -- what lives here is the result of wrapping it once per
// recipient's public key, stored in refs/keys/<n>. The plaintext key exists
// only on this machine (see keyring.go).
//
// The consequence: the same ciphertext can replicate freely with the
// repository, only members can open it, and **a new member cannot obtain epochs
// created before they joined** -- and that last property needs nobody's
// cooperation.

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"immulog/core/gitx"
)

// KeysPrefix is the namespace for epoch key records.
const KeysPrefix = "refs/keys/"

// KeyRef returns the key record ref for an epoch.
func KeyRef(n int) string { return KeysPrefix + strconv.Itoa(n) }

// maxMemberScan bounds how far back recipient discovery walks.
const maxMemberScan = 500

// Epoch is one encryption generation.
type Epoch struct {
	N       int       `json:"n"`
	Members int       `json:"members"`
	OID     string    `json:"oid,omitempty"`
	At      time.Time `json:"at"`
	// Held reports whether this machine still holds the epoch's plaintext key.
	Held bool `json:"held"`
}

// ErrNoEpoch means the chain has no epoch yet.
var ErrNoEpoch = errors.New("no encryption epoch has been established")

// ── Establishing ────────────────────────────────────────────────────

// SetupEncryption makes sure this machine has an encryption identity and at
// least one epoch.
//
// Idempotent: does nothing when both already exist.
func (s *Store) SetupEncryption(ctx context.Context) error {
	if _, err := LoadIdentity(s.repo.Dir); errors.Is(err, ErrKeysMissing) {
		id, err := GenerateEncIdentity()
		if err != nil {
			return err
		}
		if err := SaveIdentity(s.repo.Dir, id); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}

	if _, err := s.CurrentEpoch(ctx); errors.Is(err, ErrNoEpoch) {
		_, err = s.RotateEpoch(ctx, nil)
		return err
	} else if err != nil {
		return err
	}
	return nil
}

// CurrentEpoch returns the newest epoch number on the chain; ErrNoEpoch when
// encryption has never been enabled.
func (s *Store) CurrentEpoch(ctx context.Context) (int, error) {
	epochs, err := s.Epochs(ctx)
	if err != nil {
		return 0, err
	}
	if len(epochs) == 0 {
		return 0, ErrNoEpoch
	}
	return epochs[len(epochs)-1].N, nil
}

// Epochs lists every epoch record on the chain, ascending by number.
func (s *Store) Epochs(ctx context.Context) ([]Epoch, error) {
	refs, err := s.repo.Refs(ctx, KeysPrefix)
	if err != nil {
		return nil, err
	}
	var out []Epoch
	for _, r := range refs {
		n, err := strconv.Atoi(strings.TrimPrefix(r.Name, KeysPrefix))
		if err != nil {
			continue
		}
		e := Epoch{N: n, OID: r.OID, Held: HasEpochKey(s.repo.Dir, n)}
		if raw, err := s.repo.Log(ctx, r.Name, 1); err == nil && len(raw) > 0 {
			e.At = raw[0].At
			e.Members = countRecipients(raw[0].Body)
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].N < out[j].N })
	return out, nil
}

// ── Rotation ────────────────────────────────────────────────────────

// RotateEpoch establishes a new epoch, wrapping its key for every recipient
// (including this machine).
//
// recipients are base64 X25519 public keys. An empty list wraps only for this
// machine -- an epoch only you can read.
func (s *Store) RotateEpoch(ctx context.Context, recipients []string) (Epoch, error) {
	me, err := LoadIdentity(s.repo.Dir)
	if err != nil {
		return Epoch{}, err
	}

	// Deduplicate, and always include ourselves -- otherwise we could not read
	// what we just wrote
	set := map[string]bool{me.Public(): true}
	for _, r := range recipients {
		r = strings.TrimSpace(r)
		if r != "" {
			set[r] = true
		}
	}
	pubs := make([]string, 0, len(set))
	for p := range set {
		pubs = append(pubs, p)
	}
	sort.Strings(pubs) // deterministic: the same member set yields the same record

	key := make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		return Epoch{}, err
	}

	var body strings.Builder
	for _, pub := range pubs {
		recipient, err := ParseEncPublic(pub)
		if err != nil {
			return Epoch{}, fmt.Errorf("invalid recipient key (%s...): %w", short(pub), err)
		}
		wrapped, err := WrapKey(recipient, key)
		if err != nil {
			return Epoch{}, err
		}
		body.WriteString(pub + " " + wrapped + "\n")
	}

	n := 1
	if cur, err := s.CurrentEpoch(ctx); err == nil {
		n = cur + 1
	} else if !errors.Is(err, ErrNoEpoch) {
		return Epoch{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Epoch records form a chain: the parent is the **previous** epoch's record
	// (only n=1 is a root)
	prev := ""
	if n > 1 {
		prev, err = s.repo.Resolve(ctx, KeyRef(n-1))
		if err != nil {
			return Epoch{}, err
		}
	}
	tree, err := s.emptyTree(ctx)
	if err != nil {
		return Epoch{}, err
	}
	text := body.String() + "\n" + trailerKind + ": " + string(KindEpoch) + "\n" +
		trailerEpoch + ": " + strconv.Itoa(n) + "\n"

	oid, err := s.repo.Commit(ctx, tree, prev, text, s.sign)
	if err != nil {
		return Epoch{}, err
	}
	// refs/keys/<n> is a **new** ref: use EnsureRef (old is all zeros) rather
	// than plain CAS, so it refuses when the ref already exists instead of
	// overwriting it.
	if err := s.repo.EnsureRef(ctx, KeyRef(n), oid); err != nil {
		return Epoch{}, err
	}
	// The plaintext key lands on this machine only -- this step is the dividing
	// line between "ciphertext replicates widely" and "keys do not"
	if err := SaveEpochKey(s.repo.Dir, n, key); err != nil {
		return Epoch{}, err
	}
	// Refresh the write-path cache. **Assign directly**: this function already
	// holds s.mu, and locking again would self-deadlock.
	s.epoch, s.epochKey = n, key

	return Epoch{N: n, Members: len(pubs), OID: oid, At: time.Now().UTC(), Held: true}, nil
}

// ── Discarding ──────────────────────────────────────────────────────

// ShredEpoch discards this machine's plaintext key for an epoch and leaves an
// announcement on the chain.
//
// **What it cannot do must be stated plainly**: it only discards this machine's
// copy. Copies held elsewhere do not vanish -- unless every holder does the
// same. The point of the announcement is to make the act **auditable**: anyone
// can see who discarded which generation, and when.
func (s *Store) ShredEpoch(ctx context.Context, n int) (Message, error) {
	if err := ShredEpochKey(s.repo.Dir, n); err != nil {
		return Message{}, err
	}
	// If the discarded epoch is the current one, the cache must be invalidated
	// or later messages would still be sealed with the old key
	s.mu.Lock()
	if s.epoch == n {
		s.epoch, s.epochKey = 0, nil
	}
	s.mu.Unlock()

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

	var b strings.Builder
	b.WriteString("discarded the key for epoch " + strconv.Itoa(n) + "\n\n")
	b.WriteString(trailerKind + ": " + string(KindShred) + "\n")
	b.WriteString(trailerSeq + ": " + strconv.Itoa(seq) + "\n")
	b.WriteString(trailerEpoch + ": " + strconv.Itoa(n) + "\n")

	oid, err := s.repo.Commit(ctx, tree, tip, b.String(), s.sign)
	if err != nil {
		return Message{}, err
	}
	if err := s.repo.UpdateRef(ctx, s.ref, oid, tip); err != nil {
		return Message{}, err
	}
	_ = s.advanceWitness(ctx, oid)

	return Message{
		OID: oid, Seq: seq, Author: s.name, Feed: s.ref,
		Kind: KindShred, Epoch: n, At: time.Now().UTC(),
	}, nil
}

// ── Recipients and decryption ───────────────────────────────────────

// KnownRecipients collects encryption public keys from recent history
// (including this machine).
//
// Only the last maxMemberScan commits are examined: a member who never speaks
// would be missed. That trade-off is deliberate -- otherwise every rotation
// would walk the entire chain.
func (s *Store) KnownRecipients(ctx context.Context) ([]string, error) {
	me, err := LoadIdentity(s.repo.Dir)
	if err != nil {
		return nil, err
	}
	set := map[string]bool{me.Public(): true}

	raw, err := s.repo.Log(ctx, s.ref, maxMemberScan)
	if err != nil {
		return nil, err
	}
	for _, r := range raw {
		if p := strings.TrimSpace(r.Enc); p != "" {
			set[p] = true
		}
	}

	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}

// OpenEpoch returns the plaintext key for an epoch.
func (s *Store) OpenEpoch(ctx context.Context, n int) ([]byte, error) {
	return openEpochKey(ctx, s.repo, n)
}

// openEpochKey returns an epoch's plaintext key: first this machine's copy,
// then -- if absent -- the copy wrapped for us on the chain.
//
// Once unwrapped it is stored locally, so only the first message of an epoch
// pays the X25519 cost.
func openEpochKey(ctx context.Context, repo *gitx.Repo, n int) ([]byte, error) {
	// Discarding is a **decision**: the chain still holds a copy wrapped for us,
	// so this check must come first, or deleting the local file would only be
	// undone by unwrapping it again.
	if IsShredded(repo.Dir, n) {
		return nil, ErrShredded
	}
	if key, err := LoadEpochKey(repo.Dir, n); err == nil {
		return key, nil
	}
	me, err := LoadIdentity(repo.Dir)
	if err != nil {
		return nil, ErrNoKey
	}
	raw, err := repo.Log(ctx, KeyRef(n), 1)
	if err != nil || len(raw) == 0 {
		return nil, ErrNoKey
	}
	mine := me.Public()
	for _, line := range strings.Split(bodyOf(raw[0].Body), "\n") {
		pub, wrapped, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok || pub != mine {
			continue
		}
		key, err := UnwrapKey(me.priv, wrapped)
		if err != nil {
			return nil, err
		}
		_ = SaveEpochKey(repo.Dir, n, key)
		return key, nil
	}
	return nil, ErrNotRecipient
}

// Opener builds a decryptor from a repository, for Sync to use when decoding
// foreign messages.
func Opener(ctx context.Context, repo *gitx.Repo) BodyOpener {
	return func(epoch int, ciphertext string) (string, error) {
		key, err := openEpochKey(ctx, repo, epoch)
		if err != nil {
			return "", err
		}
		return OpenBody(key, ciphertext)
	}
}

// countRecipients counts how many wrapped copies a key record holds.
func countRecipients(body string) int {
	n := 0
	for _, line := range strings.Split(bodyOf(body), "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}
