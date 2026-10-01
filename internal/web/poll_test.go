// SPDX-License-Identifier: Apache-2.0

package web

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"immulog/core/feed"
)

// ── Fixtures ────────────────────────────────────────────────────

// parseFrames reads a **complete** body, which is the whole point of poll mode:
// the server hangs up, so there is nothing to wait for.
func parseFrames(t *testing.T, raw string) []sseEvent {
	t.Helper()
	var evs []sseEvent
	var cur sseEvent
	for _, line := range strings.Split(raw, "\n") {
		switch {
		case line == "":
			if cur.Type != "" {
				evs = append(evs, cur)
			}
			cur = sseEvent{}
		case strings.HasPrefix(line, "id: "):
			cur.ID = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "event: "):
			cur.Type = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			_ = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &cur.Data)
		}
	}
	return evs
}

// pollOnce performs one poll cycle and returns the raw body plus its frames.
//
// The client has a timeout on purpose: if the server ever starts *holding* the
// connection, this fails loudly instead of hanging the suite.
func pollOnce(t *testing.T, base, lastEventID string) (string, []sseEvent) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, base+"/api/stream?poll=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("poll request: %v", err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q", ct)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("poll mode must hang up, not hold the connection: %v", err)
	}
	return string(body), parseFrames(t, string(body))
}

// ── The degrade path (§7.8) ─────────────────────────────────────

// The whole of poll mode: serve what is new, hang up. Everything else is the
// same code path as the stream.
func TestPollModeServesABatchThenHangsUp(t *testing.T) {
	srv, _, _ := newServer(t)
	for _, b := range []string{"one", "two", "three"} {
		postCommit(t, srv.URL, map[string]any{"kind": "msg", "body": b})
	}

	raw, evs := pollOnce(t, srv.URL, "")

	if len(evs) != 4 {
		t.Fatalf("expected three messages and a hello, got %d frames: %+v", len(evs), evs)
	}
	// Oldest first, exactly as the live stream replays them
	if evs[0].Data["body"] != "one" || evs[2].Data["body"] != "three" {
		t.Fatalf("wrong replay order: %+v", evs)
	}
	if len(evs[0].ID) != 40 {
		t.Fatalf("the event id must stay the commit OID, got %q", evs[0].ID)
	}
	if evs[3].Type != "hello" {
		t.Fatalf("the batch must end with hello, got %q", evs[3].Type)
	}
	// The browser reconnects on this value, so it *is* the poll interval.
	if !strings.Contains(raw, "retry: ") {
		t.Fatal("poll mode must announce its cadence with retry:")
	}
}

// A batch has to say that it is a batch: the UI tells the user why messages
// arrive in lumps instead of pretending the stream is fine.
func TestPollModeAnnouncesItself(t *testing.T) {
	srv, _, _ := newServer(t)
	postCommit(t, srv.URL, map[string]any{"kind": "msg", "body": "one"})

	_, evs := pollOnce(t, srv.URL, "")
	if hello := evs[len(evs)-1]; hello.Type == "hello" && hello.Data["poll"] != true {
		t.Fatalf("hello should carry poll:true, got %+v", hello.Data)
	}

	// And the streaming path must not claim to be polling.
	resp, err := http.Get(srv.URL + "/api/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	evs = readEvents(t, resp.Body, 2, 5*time.Second)
	if _, present := evs[1].Data["poll"]; present {
		t.Fatalf("streaming mode must not set poll: %+v", evs[1].Data)
	}
}

// Polling is only usable if a resumed batch is still a resume: the browser sends
// Last-Event-ID on every reconnect, and must not be re-sent the whole history.
func TestPollModeResumesFromLastEventID(t *testing.T) {
	srv, _, _ := newServer(t)
	_, first := postCommit(t, srv.URL, map[string]any{"kind": "msg", "body": "one"})
	oid := first["oid"].(string)

	_, evs := pollOnce(t, srv.URL, oid)
	if len(evs) != 1 || evs[0].Type != "hello" {
		t.Fatalf("nothing was new, so the batch should be hello alone: %+v", evs)
	}

	postCommit(t, srv.URL, map[string]any{"kind": "msg", "body": "two"})
	_, evs = pollOnce(t, srv.URL, oid)
	if len(evs) != 2 {
		t.Fatalf("expected only the new message and hello, got %+v", evs)
	}
	if evs[0].Data["body"] != "two" {
		t.Fatalf("the batch must start after the cursor, got %+v", evs[0].Data)
	}
}

// The pre-replay alarm belongs to the first screen. Re-sending it on every batch
// would stack a new card in the timeline every few seconds, and a warning that
// repeats is a warning nobody reads.
func TestPollModeSendsThePreReplayAlarmOncePerFirstScreen(t *testing.T) {
	srv, store, dir := newServer(t)
	postCommit(t, srv.URL, map[string]any{"kind": "msg", "body": "a"})
	_, second := postCommit(t, srv.URL, map[string]any{"kind": "msg", "body": "b"})
	head := second["oid"].(string)

	// External attacker: a parallel chain from a, force-pushed.
	parent := rawGit(t, dir, "", "rev-parse", head+"^")
	tree := rawGit(t, dir, "", "hash-object", "-w", "-t", "tree", "--stdin")
	forged := rawGit(t, dir, "forged\n\nImmuLog-Kind: msg\nImmuLog-Seq: 2\n", "commit-tree", tree, "-p", parent)
	rawGit(t, dir, "", "update-ref", feed.FeedRef(store.Pub()), forged)

	_, evs := pollOnce(t, srv.URL, "")
	if evs[0].Type != "alarm" {
		t.Fatalf("the first batch must lead with the alarm, got %+v", evs)
	}
	if evs[0].Data["reason"] != feed.ReasonRewrite {
		t.Fatalf("reason = %v", evs[0].Data["reason"])
	}

	// The next batch is a resume, so the notice is not repeated.
	_, evs = pollOnce(t, srv.URL, head)
	for _, ev := range evs {
		if ev.Type == "alarm" {
			t.Fatalf("the alarm was repeated on a later batch: %+v", ev)
		}
	}
}
