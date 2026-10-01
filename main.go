// SPDX-License-Identifier: Apache-2.0

// Command immulog is a chat service that treats Git as its root of trust.
//
// This file only assembles and manages the lifecycle -- all real logic lives in
// the packages below. The layout is itself an architectural constraint:
// core/gitx is the only package that touches os/exec, and internal/web is the
// only one that touches net/http.
package main

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"immulog/core/feed"
	"immulog/core/gitx"
	"immulog/internal/web"
)

// The frontend is embedded in the binary -- deployment is one file, and nobody
// running a node needs Node.js.
//
// ⚠️ Before any release, vendor Beer CSS into web/ (see docs/DESIGN.md §8.8):
// a tamper-evidence product pulling a stylesheet from a third-party CDN at
// runtime is a supply-chain hole.
//
//go:embed web
var embedded embed.FS

func main() {
	if err := run(); err != nil {
		slog.Error("startup failed", "err", err)
		os.Exit(1)
	}
}

func run() error {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 1) Repository: bare, no worktree, no index -- committing a message writes
	//    no files at all
	dir := env("IMMULOG_REPO", "./repoDB")
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		if err := gitx.Init(ctx, dir); err != nil {
			return err
		}
		log.Info("initialised repository", "dir", dir)
	}
	repo := gitx.Open(dir)

	// 2) Identity comes only from git config, never from a request body
	name, email, err := repo.Identity(ctx)
	if err != nil {
		return err
	}
	key, _ := repo.SigningKey(ctx)

	// The feed identifier is derived only from "who you are", **not from the
	// key** -- otherwise rotating a key would mean a different feed, and the
	// rotation notice could not chain (see DESIGN.md §4.3).
	pub := feed.FeedID(name + "\x00" + email)

	// A configured key must actually work: refuse to start when signing is
	// broken, rather than silently degrading to plaintext
	fingerprint := ""
	if key != "" {
		fingerprint, err = feed.ProbeSigning(ctx, repo)
		if err != nil {
			return fmt.Errorf("user.signingkey is configured but cannot sign: %w\n"+
				"  fix it, or clear that config to run explicitly unsigned", err)
		}
	}

	store, err := feed.New(ctx, repo, pub)
	if err != nil {
		return err
	}

	// 3) End-to-end encryption (optional, but sticky once enabled)
	//
	// Two triggers: an explicit request, or an encryption identity that already
	// exists here. The latter makes "once on, always on" true -- otherwise a
	// forgotten environment variable would quietly fall back to plaintext, and
	// words already spoken cannot be taken back.
	encrypted := os.Getenv("IMMULOG_ENCRYPT") == "1" || feed.HasIdentity(dir)
	if encrypted {
		if err := store.SetupEncryption(ctx); err != nil {
			return fmt.Errorf("enabling encryption failed: %w", err)
		}
		log.Info("end-to-end encryption enabled", "keys", filepath.Join(dir, "immulog-keys"))
	}

	// 4) Frontend: embedded and same-origin -- zero CORS, zero build chain
	files, err := fs.Sub(embedded, "web")
	if err != nil {
		return err
	}

	// 5) Multi-source sync, snapshot gossip and external anchoring (all optional)
	remotes := parseRemotes(os.Getenv("IMMULOG_REMOTES"))
	peers := gossipPeers(parseRemotes(os.Getenv("IMMULOG_PEERS")))
	var publisher feed.Publisher
	if u := os.Getenv("IMMULOG_ANCHOR_URL"); u != "" {
		publisher = feed.HTTPPublisher{URL: u}
	}

	// 6) Assemble
	hub := web.NewHub()
	srv := web.New(web.Config{
		Store:     store,
		Hub:       hub,
		Repo:      repo,
		Files:     files,
		Remotes:   remotes,
		Peers:     peers,
		Publisher: publisher,
	})
	srv.Watch(ctx,
		duration("IMMULOG_SYNC_INTERVAL", web.SyncInterval),
		duration("IMMULOG_ANCHOR_INTERVAL", web.AnchorInterval),
		duration("IMMULOG_GOSSIP_INTERVAL", web.GossipInterval))

	// No WriteTimeout: SSE is a long-lived connection and it would cut it off
	s := &http.Server{
		Addr:              ":" + env("PORT", "8081"),
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.Shutdown(shut)
	}()

	log.Info("ImmuLog ready",
		"addr", s.Addr, "repo", dir, "feed", pub,
		"signed", store.Signed(), "remotes", len(remotes),
		"gossip", len(peers), "anchor", publisher != nil)
	if store.Signed() {
		log.Info("messages will be signed", "fingerprint", feed.ShortKey(fingerprint))
	} else {
		log.Warn("no user.signingkey: messages are unsigned, identity can be impersonated (see DESIGN.md §4)")
	}
	if len(remotes) == 0 {
		log.Warn("no IMMULOG_REMOTES: single-node mode, no peer sync")
	}
	if len(peers) == 0 {
		log.Warn("no IMMULOG_PEERS: no snapshot gossip, a split view stays local")
	} else if len(peers) < 3 {
		// Two views disagreeing say that something is wrong, never who is
		// wrong -- the third view is what localises it (core/feed/gossip.go).
		log.Warn("fewer than 3 gossip peers: a disagreement cannot be attributed to either side",
			"peers", len(peers))
	}
	if publisher == nil {
		log.Warn("no IMMULOG_ANCHOR_URL: anchors stay local, not truly external")
	}

	if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// parseRemotes parses IMMULOG_REMOTES.
//
// Two forms are accepted: `url` (auto-named) and `name=url`. The name is only
// used as a quarantine slot and is normalised into ref-safe form via FeedID.
//
// IMMULOG_PEERS uses the same syntax, so it is parsed by the same function --
// see gossipPeers for why the two lists stay different types.
func parseRemotes(spec string) []feed.Remote {
	var out []feed.Remote
	seen := map[string]bool{}
	for i, item := range strings.Split(spec, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		name, url := "", item
		if n, u, ok := strings.Cut(item, "="); ok {
			name, url = strings.TrimSpace(n), strings.TrimSpace(u)
		}
		if url == "" {
			continue
		}
		if name == "" {
			name = "r" + strconv.Itoa(i)
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, feed.Remote{Name: name, URL: url})
	}
	return out
}

// gossipPeers adapts the shared `name=url` parser's output for the gossip loop.
//
// A gossip peer is an HTTP endpoint, not a git remote, so the two stay
// different types even though the syntax is shared: the compiler should refuse
// to hand `file:///srv/chat.git` to an HTTP client.
func gossipPeers(rs []feed.Remote) []feed.GossipPeer {
	if len(rs) == 0 {
		return nil
	}
	out := make([]feed.GossipPeer, 0, len(rs))
	for _, r := range rs {
		out = append(out, feed.GossipPeer{Name: r.Name, URL: r.URL})
	}
	return out
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func duration(k string, def time.Duration) time.Duration {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return def
	}
	return d
}
