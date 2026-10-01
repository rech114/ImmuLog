// SPDX-License-Identifier: Apache-2.0

package web

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"immulog/core/feed"
	"immulog/core/gitx"
)

// ── Fixtures ────────────────────────────────────────────────────

func rawGit(t *testing.T, dir, stdin string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(cmd.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newServer starts a real HTTP service (not an in-memory handler) -- SSE needs to genuinely stream.
func newServer(t *testing.T) (*httptest.Server, *feed.Store, string) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")

	dir := filepath.Join(t.TempDir(), "repo.git")
	if err := gitx.Init(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	rawGit(t, dir, "", "config", "user.name", "alice")
	rawGit(t, dir, "", "config", "user.email", "alice@example.com")

	repo := gitx.Open(dir)
	store, err := feed.New(context.Background(), repo, feed.FeedID("alice"))
	if err != nil {
		t.Fatal(err)
	}

	files := fstest.MapFS{
		"index.html":  &fstest.MapFile{Data: []byte("<!doctype html><title>ImmuLog</title>")},
		"app/main.js": &fstest.MapFile{Data: []byte("export const x = 1;")},
	}
	_ = fs.FS(files)

	srv := httptest.NewServer(New(Config{
		Store: store, Hub: NewHub(), Repo: repo, Files: files,
	}).Handler())
	t.Cleanup(srv.Close)
	return srv, store, dir
}

func postCommit(t *testing.T, base string, body any) (*http.Response, map[string]any) {
	t.Helper()
	buf, _ := json.Marshal(body)
	resp, err := http.Post(base+"/api/commit", "application/json", bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("POST /api/commit: %v", err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

type sseEvent struct {
	ID    string
	Type  string
	Data  map[string]any
	Retry string
}

// readEvents reads the SSE stream until n frames carrying an event have arrived, or it times out.
func readEvents(t *testing.T, r io.ReadCloser, n int, timeout time.Duration) []sseEvent {
	t.Helper()
	defer r.Close()

	type result struct{ evs []sseEvent }
	done := make(chan result, 1)

	go func() {
		var evs []sseEvent
		var cur sseEvent
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if cur.Type != "" {
					evs = append(evs, cur)
					if len(evs) >= n {
						done <- result{evs}
						return
					}
				}
				cur = sseEvent{}
			case strings.HasPrefix(line, "id: "):
				cur.ID = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "event: "):
				cur.Type = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "retry: "):
				cur.Retry = strings.TrimPrefix(line, "retry: ")
			case strings.HasPrefix(line, "data: "):
				_ = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &cur.Data)
			}
		}
		done <- result{evs}
	}()

	select {
	case r := <-done:
		if len(r.evs) < n {
			t.Fatalf("expected %d events, only got %d: %+v", n, len(r.evs), r.evs)
		}
		return r.evs
	case <-time.After(timeout):
		t.Fatalf("timed out waiting for %d events", n)
		return nil
	}
}

func openStream(t *testing.T, base, query string) (*http.Response, []sseEvent, string) {
	t.Helper()
	resp, err := http.Get(base + "/api/stream" + query)
	if err != nil {
		t.Fatalf("GET /api/stream: %v", err)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type should be text/event-stream, got %q", ct)
	}
	return resp, nil, resp.Header.Get("X-Accel-Buffering")
}

// ── Write entry ─────────────────────────────────────────────────

func TestCommitReturnsOIDAndSeq(t *testing.T) {
	srv, _, _ := newServer(t)

	resp, body := postCommit(t, srv.URL, map[string]any{"kind": "msg", "body": "hello"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, expected 201 (%v)", resp.StatusCode, body)
	}
	oid, _ := body["oid"].(string)
	if len(oid) != 40 {
		t.Fatalf("malformed oid: %v", body["oid"])
	}
	if body["seq"].(float64) != 1 {
		t.Fatalf("seq = %v, expected 1", body["seq"])
	}
}

func TestCommitRejectsBadJSON(t *testing.T) {
	srv, _, _ := newServer(t)
	resp, err := http.Post(srv.URL+"/api/commit", "application/json", strings.NewReader("{not json"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, expected 400", resp.StatusCode)
	}
}

func TestCommitRejectsEmptyBody(t *testing.T) {
	srv, store, _ := newServer(t)
	resp, body := postCommit(t, srv.URL, map[string]any{"kind": "msg", "body": "   "})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, expected 400 (%v)", resp.StatusCode, body)
	}
	if body["error"] != "empty_body" {
		t.Fatalf("error code = %v", body["error"])
	}
	// It must genuinely not be stored -- the frontend blocks it too, but the server may not rely on the frontend
	if n, _ := store.History(context.Background(), 10); len(n) != 0 {
		t.Fatalf("an empty message must not be stored, yet the chain holds %d", len(n))
	}
}

func TestCommitRejectsTooLong(t *testing.T) {
	srv, _, _ := newServer(t)
	resp, body := postCommit(t, srv.URL, map[string]any{"kind": "msg", "body": strings.Repeat("x", feed.MaxBody+10)})
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, expected 413 (%v)", resp.StatusCode, body)
	}
	if body["error"] != "too_long" {
		t.Fatalf("error code = %v", body["error"])
	}
}

func TestRetractViaHTTP(t *testing.T) {
	srv, store, _ := newServer(t)
	_, first := postCommit(t, srv.URL, map[string]any{"kind": "msg", "body": "got it wrong"})
	target := first["oid"].(string)

	resp, body := postCommit(t, srv.URL, map[string]any{
		"kind": "retract", "retracts": target, "reason": "wrong channel",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("the retraction should succeed: %d %v", resp.StatusCode, body)
	}
	// A retraction appends: the chain should hold 2 and the original is still there
	hist, _ := store.History(context.Background(), 10)
	if len(hist) != 2 {
		t.Fatalf("the chain should grow to 2, got %d", len(hist))
	}
}

func TestRetractWithoutTargetIsRejected(t *testing.T) {
	srv, _, _ := newServer(t)
	resp, body := postCommit(t, srv.URL, map[string]any{"kind": "retract", "reason": "x"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, expected 400 (a client error is not a 500)", resp.StatusCode)
	}
	if body["error"] != "missing_target" {
		t.Fatalf("error code = %v", body["error"])
	}
}

// ── Security contract: a CAS failure must be surfaced verbatim ──

func TestCommitStatusMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code int
		errc string
	}{
		{"success", nil, 0, ""},
		{"CAS conflict", fmt.Errorf("wrapped: %w", gitx.ErrCASFailed), http.StatusConflict, "cas_failed"},
		{"too long", feed.ErrTooLong, http.StatusRequestEntityTooLarge, "too_long"},
		{"empty body", feed.ErrEmpty, http.StatusBadRequest, "empty_body"},
		{"missing target", feed.ErrNoTarget, http.StatusBadRequest, "missing_target"},
		{"malformed target", feed.ErrBadTarget, http.StatusBadRequest, "bad_target"},
		{"not your message", feed.ErrNotMine, http.StatusForbidden, "not_your_message"},
		{"other", errors.New("boom"), http.StatusInternalServerError, "commit_failed"},
	}
	for _, c := range cases {
		code, errc := commitStatus(c.err)
		if code != c.code || errc != c.errc {
			t.Errorf("%s: got (%d,%q), expected (%d,%q)", c.name, code, errc, c.code, c.errc)
		}
	}
	// In reality a CAS error is always wrapped; errors.Is has to see through it
	if code, _ := commitStatus(&gitx.Error{Code: 128}); code != http.StatusInternalServerError {
		t.Error("an ordinary git error must not be mistaken for a CAS conflict")
	}
}

// ── Downstream ──────────────────────────────────────────────────

func TestStreamReplaysHistoryThenHello(t *testing.T) {
	srv, _, _ := newServer(t)
	for _, b := range []string{"one", "two", "three"} {
		postCommit(t, srv.URL, map[string]any{"kind": "msg", "body": b})
	}

	resp, err := http.Get(srv.URL + "/api/stream")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("Content-Type = %q", resp.Header.Get("Content-Type"))
	}
	if resp.Header.Get("X-Accel-Buffering") != "no" {
		t.Error("it should send X-Accel-Buffering: no, or a reverse proxy will buffer")
	}
	if resp.Header.Get("Last-Event-ID") != "" {
		t.Error("it should not proactively send Last-Event-ID")
	}

	evs := readEvents(t, resp.Body, 4, 5*time.Second)
	// History replays oldest first
	if evs[0].Type != "msg" || evs[0].Data["body"] != "one" {
		t.Fatalf("wrong replay order: %+v", evs[0])
	}
	if evs[2].Data["body"] != "three" {
		t.Fatalf("wrong replay order: %+v", evs[2])
	}
	// The event id is the commit OID -- the resume cursor
	if len(evs[0].ID) != 40 {
		t.Fatalf("the event id should be a 40-char object name, got %q", evs[0].ID)
	}
	// Replay finished, now live
	if evs[3].Type != "hello" {
		t.Fatalf("the last one should be hello, got %q", evs[3].Type)
	}
	if evs[3].Data["signed"] != false {
		t.Error("with no signing key signed should be false")
	}
}

func TestStreamDeliversLiveMessages(t *testing.T) {
	srv, _, _ := newServer(t)

	resp, err := http.Get(srv.URL + "/api/stream")
	if err != nil {
		t.Fatal(err)
	}
	// Empty feed: only hello
	readEvents(t, resp.Body, 1, 5*time.Second)

	// Open another stream to catch live events
	resp2, err := http.Get(srv.URL + "/api/stream")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(150 * time.Millisecond)
		postCommit(t, srv.URL, map[string]any{"kind": "msg", "body": "a live message"})
	}()
	evs := readEvents(t, resp2.Body, 2, 5*time.Second)
	if evs[1].Type != "msg" || evs[1].Data["body"] != "a live message" {
		t.Fatalf("the live event is wrong: %+v", evs[1])
	}
}

// Last-Event-ID is the entire resume mechanism -- it must fill exactly the gap.
func TestStreamResumeFromLastEventID(t *testing.T) {
	srv, _, _ := newServer(t)
	var oids []string
	for _, b := range []string{"one", "two", "three"} {
		_, body := postCommit(t, srv.URL, map[string]any{"kind": "msg", "body": b})
		oids = append(oids, body["oid"].(string))
	}

	req, _ := http.NewRequest("GET", srv.URL+"/api/stream", nil)
	req.Header.Set("Last-Event-ID", oids[1]) // the client says: I already have the second one
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	evs := readEvents(t, resp.Body, 2, 5*time.Second)
	if len(evs) != 2 {
		t.Fatalf("it should send only 1 message + hello, got %d", len(evs))
	}
	if evs[0].Data["body"] != "three" {
		t.Fatalf("it should only resume 'three', got %+v", evs[0].Data)
	}
	if evs[1].Type != "hello" {
		t.Fatalf("the second should be hello, got %q", evs[1].Type)
	}
}

// A client connecting after tampering should see the alarm first thing.
func TestStreamAlarmsOnTamperedHistory(t *testing.T) {
	srv, store, dir := newServer(t)
	postCommit(t, srv.URL, map[string]any{"kind": "msg", "body": "a"})
	_, second := postCommit(t, srv.URL, map[string]any{"kind": "msg", "body": "b"})
	head := second["oid"].(string)

	// External attacker: a parallel chain from a, force-pushed
	parent := rawGit(t, dir, "", "rev-parse", head+"^")
	tree := rawGit(t, dir, "", "hash-object", "-w", "-t", "tree", "--stdin")
	forged := rawGit(t, dir, "forged\n\nImmuLog-Kind: msg\nImmuLog-Seq: 2\n", "commit-tree", tree, "-p", parent)
	rawGit(t, dir, "", "update-ref", feed.FeedRef(store.Pub()), forged)

	resp, err := http.Get(srv.URL + "/api/stream")
	if err != nil {
		t.Fatal(err)
	}
	evs := readEvents(t, resp.Body, 1, 5*time.Second)
	if evs[0].Type != "alarm" {
		t.Fatalf("the first one should be alarm, got %q", evs[0].Type)
	}
	if evs[0].Data["reason"] != feed.ReasonRewrite {
		t.Errorf("reason = %v, expected rewrite", evs[0].Data["reason"])
	}
	if evs[0].Data["local"] == "" || evs[0].Data["remote"] == "" {
		t.Errorf("the alarm should carry local/remote anchors: %+v", evs[0].Data)
	}
}

// ── Observability ───────────────────────────────────────────────

func TestSnapshotListsFeedRefsButNotWitness(t *testing.T) {
	srv, store, _ := newServer(t)
	postCommit(t, srv.URL, map[string]any{"kind": "msg", "body": "a"})

	resp, err := http.Get(srv.URL + "/api/snapshot")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Refs    []gitx.RefInfo `json:"refs"`
		Witness string         `json:"witness"`
		OK      bool           `json:"ok"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Refs) != 1 || out.Refs[0].Name != feed.FeedRef(store.Pub()) {
		t.Fatalf("the snapshot should contain only the feeds namespace: %+v", out.Refs)
	}
	if !out.OK || out.Witness == "" {
		t.Fatalf("in a normal state it should verify as consistent: %+v", out)
	}
}

func TestHealth(t *testing.T) {
	srv, store, _ := newServer(t)
	postCommit(t, srv.URL, map[string]any{"kind": "msg", "body": "a"})

	resp, err := http.Get(srv.URL + "/api/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out["feed"] != store.Pub() {
		t.Errorf("feed = %v, expected %v", out["feed"], store.Pub())
	}
	if out["ok"] != true {
		t.Error("health should be ok")
	}
}

// ── Static assets (same-origin, zero CORS) ──────────────────────

// With no signing key, rotation must be refused -- swapping identity presumes a verifiable identity exists first.
func TestRotateRequiresSigningKey(t *testing.T) {
	srv, _, _ := newServer(t)
	body, _ := json.Marshal(map[string]string{"key": "SHA256:whatever"})
	resp, err := http.Post(srv.URL+"/api/rotate", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, expected 409", resp.StatusCode)
	}
}

func TestServesEmbeddedAssets(t *testing.T) {
	srv, _, _ := newServer(t)
	for _, path := range []string{"/", "/index.html", "/app/main.js"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s status = %d", path, resp.StatusCode)
		}
	}
}
