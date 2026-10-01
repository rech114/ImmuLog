// SPDX-License-Identifier: Apache-2.0

package feed

// keyring.go -- this machine's key custody.
//
// This is the only place crypto-shredding actually lands: **plaintext epoch
// keys exist only on this machine**. They never enter the git object store,
// never enter a ref, and never take part in sync.
// "Discard the key" means overwriting and deleting this machine's copy.
//
// Honest ceiling (docs/DESIGN.md §6.4):
//   - others may still hold copies -- discarding is only complete when **every
//     holder does the same**
//   - overwriting on flash storage / journaling filesystems is not a physical
//     erase guarantee
//
// So the strength of this is custody discipline, not cryptography.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// keysDir is the key directory name: inside the repo directory, but outside any
// namespace git manages.
const keysDir = "immulog-keys"

// identityFile holds this machine's X25519 private key seed.
const identityFile = "identity"

// File permissions: readable and writable by this user only.
const keyPerm = 0o600

// ErrKeysMissing means this machine holds no key material.
var ErrKeysMissing = errors.New("no encryption identity on this machine")

// ErrShredded means this epoch's key was **deliberately discarded** here.
//
// It is not the same as ErrNoKey: ErrNoKey is "never had it", ErrShredded is
// "had it and threw it away". That distinction is the fulcrum of the whole
// crypto-shredding design -- see ShredEpochKey.
var ErrShredded = errors.New("this epoch's key was deliberately discarded")

// keyPath returns the key directory inside the repo, creating it on demand.
func keyPath(repoDir string) (string, error) {
	dir := filepath.Join(repoDir, keysDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

func epochFile(dir string, n int) string {
	return filepath.Join(dir, fmt.Sprintf("epoch-%d", n))
}

// shredMarker is the persistent record of "this epoch was discarded here".
//
// Why it is needed: the chain holds a copy of the key wrapped for us, so as
// long as our identity survives, **deleting the local file means the key can be
// unwrapped from the chain again at any time** -- which would make the deletion
// a no-op. Discarding must be a **decision**, not a file deletion.
func shredMarker(dir string, n int) string {
	return filepath.Join(dir, fmt.Sprintf("shredded-%d", n))
}

// IsShredded reports whether this machine has deliberately discarded an epoch.
func IsShredded(repoDir string, n int) bool {
	dir, err := keyPath(repoDir)
	if err != nil {
		return false
	}
	_, err = os.Stat(shredMarker(dir, n))
	return err == nil
}

// LoadIdentity reads this machine's encryption identity; ErrKeysMissing if absent.
func LoadIdentity(repoDir string) (*EncIdentity, error) {
	dir, err := keyPath(repoDir)
	if err != nil {
		return nil, err
	}
	seed, err := os.ReadFile(filepath.Join(dir, identityFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrKeysMissing
	}
	if err != nil {
		return nil, err
	}
	return ParseEncIdentity(seed)
}

// SaveIdentity persists this machine's encryption identity (overwriting).
func SaveIdentity(repoDir string, id *EncIdentity) error {
	dir, err := keyPath(repoDir)
	if err != nil {
		return err
	}
	return writeSecret(filepath.Join(dir, identityFile), id.Seed())
}

// LoadEpochKey reads an epoch's plaintext key.
// ErrShredded if it was discarded; ErrNoKey if it was never held.
func LoadEpochKey(repoDir string, n int) ([]byte, error) {
	if IsShredded(repoDir, n) {
		return nil, ErrShredded
	}
	dir, err := keyPath(repoDir)
	if err != nil {
		return nil, err
	}
	key, err := os.ReadFile(epochFile(dir, n))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoKey
	}
	if err != nil {
		return nil, err
	}
	if len(key) != KeySize {
		return nil, fmt.Errorf("epoch %d key has wrong length (%d bytes)", n, len(key))
	}
	return key, nil
}

// SaveEpochKey persists an epoch's plaintext key.
func SaveEpochKey(repoDir string, n int, key []byte) error {
	if len(key) != KeySize {
		return fmt.Errorf("key must be %d bytes", KeySize)
	}
	if IsShredded(repoDir, n) {
		// A discarded epoch may not be written back -- otherwise the discard
		// would be quietly undone
		return ErrShredded
	}
	dir, err := keyPath(repoDir)
	if err != nil {
		return err
	}
	return writeSecret(epochFile(dir, n), key)
}

// ShredEpochKey overwrites and deletes an epoch's plaintext key, then leaves a
// persistent marker.
//
// All three steps are required:
//  1. overwrite -- against "deleted but not really" recovery paths
//  2. delete
//  3. **leave a marker** -- otherwise the wrapped copy on the chain lets the key
//     be unwrapped again and the deletion becomes a no-op
//
// It is **not** a physical erase guarantee on journaling filesystems or flash
// wear levelling -- see the file header.
func ShredEpochKey(repoDir string, n int) error {
	dir, err := keyPath(repoDir)
	if err != nil {
		return err
	}
	path := epochFile(dir, n)

	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		// No key file: either already discarded (marker present) or never held
		if IsShredded(repoDir, n) {
			return nil
		}
		return ErrNoKey
	}
	if err := writeSecret(path, make([]byte, KeySize)); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	return writeSecret(shredMarker(dir, n), []byte("1"))
}

// HasEpochKey reports whether this machine still holds an epoch's plaintext key.
func HasEpochKey(repoDir string, n int) bool {
	_, err := LoadEpochKey(repoDir, n)
	return err == nil
}

// HasIdentity reports whether this machine already has an encryption identity.
func HasIdentity(repoDir string) bool {
	_, err := LoadIdentity(repoDir)
	return err == nil
}

// writeSecret writes atomically with 0600 permissions.
func writeSecret(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, keyPerm); err != nil {
		return err
	}
	if err := os.Chmod(tmp, keyPerm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
