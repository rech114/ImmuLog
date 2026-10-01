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

	"immutalk/internal/feed"
	"immutalk/internal/gitx"
)

// ── 夹具 ──────────────────────────────────────────────────────────

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

// newServer 起一个真实 HTTP 服务（不是内存 handler）—— SSE 需要真的能流式写。
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
		"index.html":  &fstest.MapFile{Data: []byte("<!doctype html><title>Immutalk</title>")},
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

// readEvents 读 SSE 流直到集齐 n 条带 event 的帧，或超时。
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
			t.Fatalf("期望 %d 条事件，只拿到 %d：%+v", n, len(r.evs), r.evs)
		}
		return r.evs
	case <-time.After(timeout):
		t.Fatalf("等待 %d 条事件超时", n)
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
		t.Fatalf("Content-Type 应为 text/event-stream，得到 %q", ct)
	}
	return resp, nil, resp.Header.Get("X-Accel-Buffering")
}

// ── 写入口 ────────────────────────────────────────────────────────

func TestCommitReturnsOIDAndSeq(t *testing.T) {
	srv, _, _ := newServer(t)

	resp, body := postCommit(t, srv.URL, map[string]any{"kind": "msg", "body": "你好"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("状态码 = %d，期望 201（%v）", resp.StatusCode, body)
	}
	oid, _ := body["oid"].(string)
	if len(oid) != 40 {
		t.Fatalf("oid 形状不对：%v", body["oid"])
	}
	if body["seq"].(float64) != 1 {
		t.Fatalf("seq = %v，期望 1", body["seq"])
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
		t.Fatalf("状态码 = %d，期望 400", resp.StatusCode)
	}
}

func TestCommitRejectsEmptyBody(t *testing.T) {
	srv, store, _ := newServer(t)
	resp, body := postCommit(t, srv.URL, map[string]any{"kind": "msg", "body": "   "})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400（%v）", resp.StatusCode, body)
	}
	if body["error"] != "empty_body" {
		t.Fatalf("错误码 = %v", body["error"])
	}
	// 必须真的没落盘 —— 前端也会拦，但服务端不能依赖前端
	if n, _ := store.History(context.Background(), 10); len(n) != 0 {
		t.Fatalf("空消息不得落盘，链上却有 %d 条", len(n))
	}
}

func TestCommitRejectsTooLong(t *testing.T) {
	srv, _, _ := newServer(t)
	resp, body := postCommit(t, srv.URL, map[string]any{"kind": "msg", "body": strings.Repeat("x", feed.MaxBody+10)})
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("状态码 = %d，期望 413（%v）", resp.StatusCode, body)
	}
	if body["error"] != "too_long" {
		t.Fatalf("错误码 = %v", body["error"])
	}
}

func TestRetractViaHTTP(t *testing.T) {
	srv, store, _ := newServer(t)
	_, first := postCommit(t, srv.URL, map[string]any{"kind": "msg", "body": "说错了"})
	target := first["oid"].(string)

	resp, body := postCommit(t, srv.URL, map[string]any{
		"kind": "retract", "retracts": target, "reason": "发错频道",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("撤回应成功：%d %v", resp.StatusCode, body)
	}
	// 撤回是追加：链上应有 2 条，原消息仍在
	hist, _ := store.History(context.Background(), 10)
	if len(hist) != 2 {
		t.Fatalf("链应增长到 2 条，得到 %d", len(hist))
	}
}

func TestRetractWithoutTargetIsRejected(t *testing.T) {
	srv, _, _ := newServer(t)
	resp, body := postCommit(t, srv.URL, map[string]any{"kind": "retract", "reason": "x"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400（客户端错误不该是 500）", resp.StatusCode)
	}
	if body["error"] != "missing_target" {
		t.Fatalf("错误码 = %v", body["error"])
	}
}

// ── 安全契约：CAS 失败必须原样上报 ────────────────────────────────

func TestCommitStatusMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code int
		errc string
	}{
		{"成功", nil, 0, ""},
		{"CAS 冲突", fmt.Errorf("包装过的: %w", gitx.ErrCASFailed), http.StatusConflict, "cas_failed"},
		{"超长", feed.ErrTooLong, http.StatusRequestEntityTooLarge, "too_long"},
		{"空正文", feed.ErrEmpty, http.StatusBadRequest, "empty_body"},
		{"缺撤回目标", feed.ErrNoTarget, http.StatusBadRequest, "missing_target"},
		{"目标格式非法", feed.ErrBadTarget, http.StatusBadRequest, "bad_target"},
		{"不是自己的消息", feed.ErrNotMine, http.StatusForbidden, "not_your_message"},
		{"其它", errors.New("boom"), http.StatusInternalServerError, "commit_failed"},
	}
	for _, c := range cases {
		code, errc := commitStatus(c.err)
		if code != c.code || errc != c.errc {
			t.Errorf("%s：得到 (%d,%q)，期望 (%d,%q)", c.name, code, errc, c.code, c.errc)
		}
	}
	// 真实场景里 CAS 错误一定被包装过，errors.Is 必须能穿透
	if code, _ := commitStatus(&gitx.Error{Code: 128}); code != http.StatusInternalServerError {
		t.Error("普通 git 错误不应被当成 CAS 冲突")
	}
}

// ── 下行 ──────────────────────────────────────────────────────────

func TestStreamReplaysHistoryThenHello(t *testing.T) {
	srv, _, _ := newServer(t)
	for _, b := range []string{"一", "二", "三"} {
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
		t.Error("应下发 X-Accel-Buffering: no，否则反代会缓冲")
	}
	if resp.Header.Get("Last-Event-ID") != "" {
		t.Error("不应主动下发 Last-Event-ID")
	}

	evs := readEvents(t, resp.Body, 4, 5*time.Second)
	// 历史按从旧到新回放
	if evs[0].Type != "msg" || evs[0].Data["body"] != "一" {
		t.Fatalf("回放顺序错误：%+v", evs[0])
	}
	if evs[2].Data["body"] != "三" {
		t.Fatalf("回放顺序错误：%+v", evs[2])
	}
	// 事件 id 就是 commit OID —— 断线续传的游标
	if len(evs[0].ID) != 40 {
		t.Fatalf("事件 id 应是 40 位对象名，得到 %q", evs[0].ID)
	}
	// 回放完毕切实时
	if evs[3].Type != "hello" {
		t.Fatalf("最后应是 hello，得到 %q", evs[3].Type)
	}
	if evs[3].Data["signed"] != false {
		t.Error("未配置签名密钥时 signed 应为 false")
	}
}

func TestStreamDeliversLiveMessages(t *testing.T) {
	srv, _, _ := newServer(t)

	resp, err := http.Get(srv.URL + "/api/stream")
	if err != nil {
		t.Fatal(err)
	}
	// 空 feed：只有 hello
	readEvents(t, resp.Body, 1, 5*time.Second)

	// 另开一条流收实时事件
	resp2, err := http.Get(srv.URL + "/api/stream")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(150 * time.Millisecond)
		postCommit(t, srv.URL, map[string]any{"kind": "msg", "body": "实时消息"})
	}()
	evs := readEvents(t, resp2.Body, 2, 5*time.Second)
	if evs[1].Type != "msg" || evs[1].Data["body"] != "实时消息" {
		t.Fatalf("实时事件不正确：%+v", evs[1])
	}
}

// Last-Event-ID 是断线续传的全部机制 —— 必须精确只补缺口。
func TestStreamResumeFromLastEventID(t *testing.T) {
	srv, _, _ := newServer(t)
	var oids []string
	for _, b := range []string{"一", "二", "三"} {
		_, body := postCommit(t, srv.URL, map[string]any{"kind": "msg", "body": b})
		oids = append(oids, body["oid"].(string))
	}

	req, _ := http.NewRequest("GET", srv.URL+"/api/stream", nil)
	req.Header.Set("Last-Event-ID", oids[1]) // 客户端说：我已经收到第二条了
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	evs := readEvents(t, resp.Body, 2, 5*time.Second)
	if len(evs) != 2 {
		t.Fatalf("应只补 1 条消息 + hello，得到 %d 条", len(evs))
	}
	if evs[0].Data["body"] != "三" {
		t.Fatalf("应只续传「三」，得到 %+v", evs[0].Data)
	}
	if evs[1].Type != "hello" {
		t.Fatalf("第二条应是 hello，得到 %q", evs[1].Type)
	}
}

// 篡改后连上来的客户端，第一眼就该看到告警。
func TestStreamAlarmsOnTamperedHistory(t *testing.T) {
	srv, store, dir := newServer(t)
	postCommit(t, srv.URL, map[string]any{"kind": "msg", "body": "a"})
	_, second := postCommit(t, srv.URL, map[string]any{"kind": "msg", "body": "b"})
	head := second["oid"].(string)

	// 外部攻击者：从 a 另起一条平行链并强推
	parent := rawGit(t, dir, "", "rev-parse", head+"^")
	tree := rawGit(t, dir, "", "hash-object", "-w", "-t", "tree", "--stdin")
	forged := rawGit(t, dir, "伪造\n\nImmutalk-Kind: msg\nImmutalk-Seq: 2\n", "commit-tree", tree, "-p", parent)
	rawGit(t, dir, "", "update-ref", feed.FeedRef(store.Pub()), forged)

	resp, err := http.Get(srv.URL + "/api/stream")
	if err != nil {
		t.Fatal(err)
	}
	evs := readEvents(t, resp.Body, 1, 5*time.Second)
	if evs[0].Type != "alarm" {
		t.Fatalf("第一条应是 alarm，得到 %q", evs[0].Type)
	}
	if evs[0].Data["reason"] != feed.ReasonRewrite {
		t.Errorf("reason = %v，期望 rewrite", evs[0].Data["reason"])
	}
	if evs[0].Data["local"] == "" || evs[0].Data["remote"] == "" {
		t.Errorf("告警应带上本地/远端锚点：%+v", evs[0].Data)
	}
}

// ── 观测 ──────────────────────────────────────────────────────────

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
		t.Fatalf("快照应只含 feeds 命名空间：%+v", out.Refs)
	}
	if !out.OK || out.Witness == "" {
		t.Fatalf("正常状态下应判定一致：%+v", out)
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
		t.Errorf("feed = %v，期望 %v", out["feed"], store.Pub())
	}
	if out["ok"] != true {
		t.Error("health 应为 ok")
	}
}

// ── 静态资源（同源，零 CORS）─────────────────────────────────────

// 未配置签名密钥时，轮换必须被拒 —— 换身份的前提是本来就有可验证的身份。
func TestRotateRequiresSigningKey(t *testing.T) {
	srv, _, _ := newServer(t)
	body, _ := json.Marshal(map[string]string{"key": "SHA256:whatever"})
	resp, err := http.Post(srv.URL+"/api/rotate", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("状态码 = %d，期望 409", resp.StatusCode)
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
			t.Errorf("%s 状态码 = %d", path, resp.StatusCode)
		}
	}
}
