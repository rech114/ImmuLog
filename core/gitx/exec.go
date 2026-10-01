// SPDX-License-Identifier: Apache-2.0

// Package gitx is the only boundary between ImmuLog and the git binary.
//
// This is the only package in the project allowed to import os/exec. Anywhere
// else, an exec.Command call is a design defect.
// The payoff: swapping languages, swapping in libgit2, or adding a cache layer
// touches exactly one package.
//
// Four rules (see docs/DESIGN.md §10):
//  1. Arguments are always []string, message bodies always go through stdin --
//     never build a shell string
//  2. Every call carries a context timeout -- a stuck child process must not
//     take down a handler
//  3. Never `git commit` (needs an index, needs a lock); only `commit-tree`
//  4. Never call git inside a loop -- one `git log` fetches one page
package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// ErrCASFailed means update-ref's <old> value did not match -- someone got
// there first, or the ref was rewritten. This is the signal source for tamper
// detection and must be surfaced verbatim, never swallowed.
var ErrCASFailed = errors.New("cas failed")

// Error is a non-zero exit from a git child process.
type Error struct {
	Args   []string
	Code   int
	Stderr string
}

func (e *Error) Error() string {
	s := strings.TrimSpace(e.Stderr)
	if s == "" {
		s = "exit " + strconv.Itoa(e.Code)
	}
	return "git " + strings.Join(e.Args, " ") + ": " + s
}

// Repo is a handle on a git repository. The zero value is not usable; use Open.
type Repo struct {
	Dir     string
	Timeout time.Duration
}

const defaultTimeout = 10 * time.Second

// Open binds an existing repository directory (bare or not).
func Open(dir string) *Repo { return &Repo{Dir: dir, Timeout: defaultTimeout} }

// Init creates a bare repository in dir. Bare is deliberate: no worktree, no
// index, and messages are committed via commit-tree so nothing lands on disk.
func Init(ctx context.Context, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	r := Open(dir)
	_, err := r.run(ctx, nil, "init", "--bare", "--quiet", "-b", "main")
	return err
}

// run is the single entry point for every git invocation. stdin is not piped
// when nil.
func (r *Repo) run(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = r.Dir
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb

	if err := cmd.Run(); err != nil {
		stderr := errb.String()
		if isCASRejection(stderr) {
			return nil, fmt.Errorf("%w: %s", ErrCASFailed, strings.TrimSpace(stderr))
		}
		var ee *exec.ExitError
		code := -1
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		}
		return nil, &Error{Args: args, Code: code, Stderr: stderr}
	}
	return out.Bytes(), nil
}

// runLines splits run's output into lines, dropping the trailing newline.
// For single-value output such as hash and ref lookups.
func (r *Repo) runLines(ctx context.Context, stdin []byte, args ...string) ([]string, error) {
	out, err := r.run(ctx, stdin, args...)
	if err != nil {
		return nil, err
	}
	text := strings.TrimRight(string(out), "\n")
	if text == "" {
		return nil, nil
	}
	return strings.Split(text, "\n"), nil
}

// isCASRejection normalises git's wording into ErrCASFailed.
func isCASRejection(stderr string) bool {
	return strings.Contains(stderr, "cannot lock ref") ||
		strings.Contains(stderr, "reference already exists") ||
		strings.Contains(stderr, "but expected")
}

// Config reads one git config value; a missing key yields an empty string
// rather than an error.
func (r *Repo) Config(ctx context.Context, key string) (string, error) {
	lines, err := r.runLines(ctx, nil, "config", "--get", key)
	if err != nil {
		var ge *Error
		if errors.As(err, &ge) && ge.Code == 1 {
			return "", nil // key does not exist
		}
		return "", err
	}
	if len(lines) == 0 {
		return "", nil
	}
	return strings.TrimSpace(lines[0]), nil
}

// Identity returns the repository's identity. ImmuLog's author identity comes
// only from here, never from a request body.
func (r *Repo) Identity(ctx context.Context) (name, email string, err error) {
	name, err = r.Config(ctx, "user.name")
	if err != nil {
		return "", "", err
	}
	email, err = r.Config(ctx, "user.email")
	if err != nil {
		return "", "", err
	}
	if name == "" || email == "" {
		return "", "", errors.New("git identity is not configured: set user.name and user.email")
	}
	return name, email, nil
}

// SigningKey returns the configured signing key; empty means commits are not
// signed.
func (r *Repo) SigningKey(ctx context.Context) (string, error) {
	return r.Config(ctx, "user.signingkey")
}
