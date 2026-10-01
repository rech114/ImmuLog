// SPDX-License-Identifier: Apache-2.0

// http.go -- routes and handlers. Protocol adaptation only: no domain logic, no
// transport details.
package web

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"immulog/core/feed"
	"immulog/core/gitx"
)

// How many entries to replay per page.
const (
	replayFirst   = 50  // first screen
	replayResume  = 200 // resume after disconnect
	maxUploadSize = feed.MaxBody * 2
	pushQueue     = 1 // coalesce push requests: rapid clicks push once
)

// Config holds the dependencies needed to assemble a Server.
type Config struct {
	Store     *feed.Store
	Hub       *Hub
	Repo      *gitx.Repo
	Files     fs.FS
	Remotes   []feed.Remote
	Peers     []feed.GossipPeer
	Publisher feed.Publisher
}

// Server wires the domain layer to the transport layer.
type Server struct {
	store     *feed.Store
	hub       *Hub
	repo      *gitx.Repo
	files     fs.FS
	remotes   []feed.Remote
	peers     []feed.GossipPeer
	publisher feed.Publisher
	state     *State
	log       *slog.Logger

	pushReq chan struct{}

	muGuard   sync.Mutex
	guardSeen map[string]bool
}

// New assembles a Server.
func New(cfg Config) *Server {
	return &Server{
		store:     cfg.Store,
		hub:       cfg.Hub,
		repo:      cfg.Repo,
		files:     cfg.Files,
		remotes:   cfg.Remotes,
		peers:     cfg.Peers,
		publisher: cfg.Publisher,
		state:     &State{},
		log:       slog.Default(),
		pushReq:   make(chan struct{}, pushQueue),
		guardSeen: map[string]bool{},
	}
}

// Handler returns the complete route table.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/stream", s.handleStream)
	mux.HandleFunc("POST /api/commit", s.handleCommit)
	mux.HandleFunc("POST /api/rotate", s.handleRotate)
	mux.HandleFunc("POST /api/epoch", s.handleNewEpoch)
	mux.HandleFunc("POST /api/shred", s.handleShred)
	mux.HandleFunc("GET /api/snapshot", s.handleSnapshot)
	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.Handle("GET /", http.FileServerFS(s.files)) // same-origin: zero CORS config
	return mux
}

// ── Write entry ─────────────────────────────────────────────────────

type commitReq struct {
	Kind     string `json:"kind"`
	Body     string `json:"body"`
	Retracts string `json:"retracts"`
	Reason   string `json:"reason"`
}

// handleCommit is the only write entry. Events are distinguished by kind rather
// than by separate endpoints.
func (s *Server) handleCommit(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)

	var req commitReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad_json"})
		return
	}

	var (
		msg feed.Message
		err error
	)
	if feed.Kind(req.Kind) == feed.KindRetract {
		msg, err = s.store.Retract(r.Context(), req.Retracts, req.Reason)
	} else {
		msg, err = s.store.Send(r.Context(), req.Body)
	}

	if code, _ := commitStatus(err); code != 0 {
		if code >= 500 {
			s.log.Error("commit failed", "err", err)
		}
		body := map[string]any{"error": errorCode(err)}
		if errors.Is(err, gitx.ErrCASFailed) {
			// The detail is the evidence of tampering and must be given verbatim
			body["detail"] = err.Error()
		}
		writeJSON(w, code, body)
		return
	}

	s.hub.Broadcast(feedEvent(msg)) // to everyone, sender included; the UI dedupes by OID
	s.kick()                        // coalesced background push; never blocks the request path
	s.refreshSnapshot(r.Context())
	writeJSON(w, http.StatusCreated, map[string]any{"oid": msg.OID, "seq": msg.Seq})
}

// commitStatus maps domain errors onto HTTP status codes. Returns 0 for nil.
//
// It exists separately to pin one security contract down:
// **a CAS failure (someone got there first, or history was rewritten) must be
// surfaced verbatim -- never reduced to a log line, as the prototype did.**
func commitStatus(err error) (int, string) {
	switch {
	case err == nil:
		return 0, ""
	case errors.Is(err, gitx.ErrCASFailed):
		return http.StatusConflict, "cas_failed"
	case errors.Is(err, feed.ErrTooLong):
		return http.StatusRequestEntityTooLarge, "too_long"
	case errors.Is(err, feed.ErrEmpty):
		return http.StatusBadRequest, "empty_body"
	case errors.Is(err, feed.ErrNoTarget):
		return http.StatusBadRequest, "missing_target"
	case errors.Is(err, feed.ErrBadTarget):
		return http.StatusBadRequest, "bad_target"
	case errors.Is(err, feed.ErrNotMine):
		return http.StatusForbidden, "not_your_message"
	default:
		return http.StatusInternalServerError, "commit_failed"
	}
}

func errorCode(err error) string {
	_, code := commitStatus(err)
	return code
}

// ── Encryption epochs ───────────────────────────────────────────────

type encPayload struct {
	Enabled bool `json:"enabled"`
	Epoch   int  `json:"epoch,omitempty"`
	Held    bool `json:"held"`
	Members int  `json:"members,omitempty"`
}

func (s *Server) encryption(ctx context.Context) encPayload {
	epochs, err := s.store.Epochs(ctx)
	if err != nil || len(epochs) == 0 {
		return encPayload{}
	}
	cur := epochs[len(epochs)-1]
	return encPayload{Enabled: true, Epoch: cur.N, Held: cur.Held, Members: cur.Members}
}

// handleNewEpoch rotates to a fresh encryption epoch.
//
// Recipients are the encryption public keys seen in recent history (plus this
// machine) -- a new member is picked up automatically once they have posted a
// message, and they cannot read any epoch created before they joined.
func (s *Server) handleNewEpoch(w http.ResponseWriter, r *http.Request) {
	recips, err := s.store.KnownRecipients(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "recipients_failed"})
		return
	}
	e, err := s.store.RotateEpoch(r.Context(), recips)
	if err != nil {
		s.log.Warn("epoch rotation failed", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "epoch_failed"})
		return
	}
	s.hub.Broadcast(Event{Type: "encryption", Data: s.encryption(r.Context())})
	writeJSON(w, http.StatusCreated, e)
}

type shredReq struct {
	Epoch int `json:"epoch"`
}

// handleShred discards this machine's plaintext key for one epoch.
//
// It **only discards this machine's copy**. The chain still holds the copy
// wrapped for us, so the discard is recorded as a persistent decision (see
// feed.ShredEpochKey) and leaves an auditable notice on the chain.
func (s *Server) handleShred(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 256)
	var req shredReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Epoch <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad_json"})
		return
	}
	if _, err := s.store.ShredEpoch(r.Context(), req.Epoch); err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "shred_failed", "detail": err.Error()})
		return
	}
	s.hub.Broadcast(Event{Type: "encryption", Data: s.encryption(r.Context())})
	writeJSON(w, http.StatusCreated, map[string]any{"epoch": req.Epoch, "shredded": true})
}

// ── Signing key rotation ────────────────────────────────────────────

type rotateReq struct {
	Key string `json:"key"`
}

// handleRotate appends a signing key rotation notice.
//
// **Signed by the current (old) key, declaring the new one** -- so the order is
// "call this first, then change the config". See feed.Store.DeclareKey.
func (s *Server) handleRotate(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 512)

	var req rotateReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad_json"})
		return
	}

	msg, err := s.store.DeclareKey(r.Context(), req.Key)
	switch {
	case err == nil:
		s.kick()
		writeJSON(w, http.StatusCreated, map[string]any{
			"oid": msg.OID, "seq": msg.Seq, "kind": "rotate",
		})
	case errors.Is(err, feed.ErrNoSigningKey):
		// Rotating on a feed that does not sign is meaningless -- refuse, rather
		// than quietly swapping the identity
		writeJSON(w, http.StatusConflict, map[string]any{"error": "signing_required"})
	default:
		s.log.Warn("rotation failed", "err", err)
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "rotate_failed"})
	}
}

// kick requests one background push; an already-pending request absorbs it
// (coalesced pushing, see §10 rule 6).
func (s *Server) kick() {
	select {
	case s.pushReq <- struct{}{}:
	default:
	}
}

// pushLoop pushes the local feed to every remote. Failure is not fatal -- the
// local commit is the fact.
func (s *Server) pushLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.pushReq:
			if len(s.remotes) == 0 {
				continue
			}
			if tip, err := s.store.Tip(ctx); err != nil || tip == "" {
				continue
			}
			for name, msg := range feed.Publish(ctx, s.repo, s.remotes, s.store.FeedRef()) {
				s.log.Warn("push failed", "remote", name, "err", msg)
			}
		}
	}
}

// ── Downstream ──────────────────────────────────────────────────────

// handleStream uses one code path for both the first screen and
// resume-after-disconnect.
//
// The event id is the commit OID: the browser sends Last-Event-ID on reconnect,
// so resuming needs no application-level protocol at all.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	sess, err := NewSession(w)
	if err != nil {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	if err := sess.Open(); err != nil {
		return
	}

	ctx := r.Context()
	since := r.URL.Query().Get("since")
	if since == "" {
		since = r.Header.Get("Last-Event-ID")
	}

	limit := replayFirst
	if since != "" {
		limit = replayResume
	}

	// Verify before replaying: if the history has been touched, the user must be
	// told **before** seeing any of it
	if v, err := s.store.Verify(ctx); err == nil && !v.OK {
		if ev, ok := alarmEvent(v); ok {
			_ = sess.Send(ev)
		}
	}

	if err := s.replay(ctx, sess, since, limit); err != nil {
		s.log.Warn("replay interrupted", "err", err)
		return
	}
	if err := sess.Send(Event{Type: "hello", Data: s.hello(ctx)}); err != nil {
		return
	}

	ch, cancel := s.hub.Subscribe()
	defer cancel()

	beat := time.NewTicker(Heartbeat)
	defer beat.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if err := sess.Send(ev); err != nil {
				return
			}
		case <-beat.C:
			if err := sess.Ping(); err != nil {
				return
			}
		}
	}
}

// replay pushes history oldest first; with `since`, only what follows it.
func (s *Server) replay(ctx context.Context, sess *Session, since string, limit int) error {
	history, err := s.store.History(ctx, limit)
	if err != nil {
		return err
	}
	end := len(history) // history is newest first
	if since != "" {
		for i, m := range history {
			if m.OID == since {
				end = i // only push what is newer
				break
			}
		}
	}
	// Oldest first: collect what to send, then emit in reverse
	batch := make([]Event, 0, end)
	for i := 0; i < end; i++ {
		if ev, ok := feedEventOf(history[i]); ok {
			batch = append(batch, ev)
		}
	}
	for i := len(batch) - 1; i >= 0; i-- {
		if err := sess.Send(batch[i]); err != nil {
			return err
		}
	}
	return nil
}

// ── Observability ───────────────────────────────────────────────────

type identityPayload struct {
	Signed bool   `json:"signed"`
	Key    string `json:"key,omitempty"`
}

type helloPayload struct {
	Head       string          `json:"head,omitempty"`
	Signed     bool            `json:"signed"`
	Identity   identityPayload `json:"identity"`
	Encryption encPayload      `json:"encryption"`
	Snapshot   string          `json:"snapshot,omitempty"`
	AnchoredAt string          `json:"anchoredAt,omitempty"`
	External   bool            `json:"external"`
	Peers      []feed.PeerView `json:"peers,omitempty"`
	// Gossip is the peer *view* comparison. It is kept apart from Peers, which
	// is the git-sync picture: a peer can agree on every feed it carries and
	// still be carrying fewer feeds than somebody else.
	Gossip  []feed.PeerReport    `json:"gossip,omitempty"`
	Missing []feed.SeenElsewhere `json:"missingFeeds,omitempty"`
}

func (s *Server) hello(ctx context.Context) helloPayload {
	head, _ := s.store.Tip(ctx)
	snap, anchor, peers := s.state.Snapshot()
	gossip, missing := s.state.Gossip()

	p := helloPayload{
		Head:       head,
		Signed:     s.store.Signed(),
		Snapshot:   snap.Digest,
		Peers:      peers,
		Gossip:     gossip,
		Missing:    missing,
		Identity:   identityPayload{Signed: s.store.Signed()},
		Encryption: s.encryption(ctx),
	}
	if p.Identity.Signed {
		if k, err := s.store.CurrentKey(ctx); err == nil {
			p.Identity.Key = feed.ShortKey(k)
		}
	}
	if anchor.OID != "" {
		p.AnchoredAt = anchor.At.Local().Format("2006-01-02 15:04")
		p.External = anchor.External != ""
	}
	if p.Snapshot == "" {
		if fresh, err := feed.Capture(ctx, s.repo); err == nil {
			p.Snapshot = fresh.Digest
		}
	}
	return p
}

// handleSnapshot returns this node's view of every feed -- the gossip unit.
//
// It is the same endpoint a peer reads (core/feed's FetchSnapshot), so the
// browser and the gossip loop are looking at one shape of truth rather than
// two. `refs` is what makes set-level comparison possible: a digest alone would
// say "different" without saying what.
func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	snap, anchor, peers := s.state.Snapshot()
	gossip, missing := s.state.Gossip()
	if snap.Digest == "" {
		if fresh, err := feed.Capture(r.Context(), s.repo); err == nil {
			snap = fresh
		}
	}
	verdict, _ := s.store.Verify(r.Context())
	identity := identityPayload{Signed: s.store.Signed()}
	if identity.Signed {
		if k, err := s.store.CurrentKey(r.Context()); err == nil {
			identity.Key = feed.ShortKey(k)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"digest":       snap.Digest,
		"refs":         snap.Refs,
		"at":           snap.At,
		"anchor":       anchor,
		"peers":        peers,
		"gossip":       gossip,
		"missingFeeds": missing,
		"identity":     identity,
		"witness":      verdict.Witness,
		"tip":          verdict.Current,
		"ok":           verdict.OK,
		"reason":       verdict.Reason,
		"feed":         s.store.FeedRef(),
		"remotes":      s.remotes,
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	head, _ := s.store.Tip(r.Context())
	_, anchor, peers := s.state.Snapshot()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"head":     head,
		"feed":     s.store.Pub(),
		"signed":   s.store.Signed(),
		"clients":  s.hub.ClientCount(),
		"remotes":  len(s.remotes),
		"peers":    peers,
		"gossip":   len(s.peers),
		"anchored": anchor.OID != "",
	})
}

// ── Wire-protocol adaptation ────────────────────────────────────────

// feedEventOf turns a domain message into a wire event. A retraction produces
// no new bubble; it only updates its target.
//
// The event carries `feed`: clients use it to guarantee that **a retraction only
// applies to messages in the same feed**, which makes retraction the author's
// right rather than something anyone can do to anyone.
//
// A key rotation notice is a **structural event** and never enters the timeline
// (its effect shows up in the identity panel and the chain check), so it
// returns false.
func feedEventOf(m feed.Message) (Event, bool) {
	switch m.Kind {
	case feed.KindRotate, feed.KindEpoch, feed.KindShred:
		return Event{}, false
	case feed.KindRetract:
		if m.Retracts == "" {
			return Event{}, false
		}
		return Event{ID: m.OID, Type: "retract", Data: map[string]any{
			"oid":      m.OID,
			"feed":     m.Feed,
			"retracts": m.Retracts,
			"reason":   m.Reason,
		}}, true
	default:
		return Event{ID: m.OID, Type: "msg", Data: m}, true
	}
}

func feedEvent(m feed.Message) Event {
	ev, _ := feedEventOf(m)
	return ev
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
