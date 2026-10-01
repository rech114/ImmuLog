// http.go —— 路由与处理器。只做协议适配，不含领域逻辑，不含传输细节。
package web

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"time"

	"immutalk/internal/feed"
	"immutalk/internal/gitx"
)

// 每页回放的条数。
const (
	replayFirst   = 50  // 首屏
	replayResume  = 200 // 断线续传
	maxUploadSize = feed.MaxBody * 2
)

// Server 把领域层与传输层接起来。
type Server struct {
	store *feed.Store
	hub   *Hub
	repo  *gitx.Repo
	files fs.FS
	log   *slog.Logger
}

// New 组装一个 Server。
func New(store *feed.Store, hub *Hub, repo *gitx.Repo, files fs.FS) *Server {
	return &Server{store: store, hub: hub, repo: repo, files: files, log: slog.Default()}
}

// Handler 返回完整的路由表。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/stream", s.handleStream)
	mux.HandleFunc("POST /api/commit", s.handleCommit)
	mux.HandleFunc("GET /api/snapshot", s.handleSnapshot)
	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.Handle("GET /", http.FileServerFS(s.files)) // 同源：零 CORS 配置
	return mux
}

// ── 写入口 ────────────────────────────────────────────────────────

type commitReq struct {
	Kind     string `json:"kind"`
	Body     string `json:"body"`
	Retracts string `json:"retracts"`
	Reason   string `json:"reason"`
}

// handleCommit 是唯一的写入口。用 kind 区分事件类型，而不是开多个端点。
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
			s.log.Error("提交失败", "err", err)
		}
		body := map[string]any{"error": errorCode(err)}
		if errors.Is(err, gitx.ErrCASFailed) {
			// 详情是篡改检测的证据，必须原样给出
			body["detail"] = err.Error()
		}
		writeJSON(w, code, body)
		return
	}

	s.hub.Broadcast(feedEvent(msg)) // 广播给所有人（含发送者，前端按 OID 去重）
	writeJSON(w, http.StatusCreated, map[string]any{"oid": msg.OID, "seq": msg.Seq})
}

// commitStatus 把领域错误映射成 HTTP 状态码。err 为 nil 时返回 0。
//
// 单独提出来是为了把一条安全契约钉死：
// **CAS 失败（有人抢先或历史被改写）必须原样上报，绝不能像原型那样只 log 一行。**
func commitStatus(err error) (int, string) {
	switch {
	case err == nil:
		return 0, ""
	case errors.Is(err, gitx.ErrCASFailed):
		return http.StatusConflict, "cas_failed"
	case errors.Is(err, feed.ErrTooLong):
		return http.StatusRequestEntityTooLarge, "too_long"
	case errors.Is(err, feed.ErrNoTarget):
		return http.StatusBadRequest, "missing_target"
	case errors.Is(err, feed.ErrEmpty):
		return http.StatusBadRequest, "empty_body"
	default:
		return http.StatusInternalServerError, "commit_failed"
	}
}

func errorCode(err error) string {
	_, code := commitStatus(err)
	return code
}

// ── 下行 ──────────────────────────────────────────────────────────

// handleStream 首屏历史与断线续传共用同一条代码路径。
//
// 事件 id 就是 commit OID：浏览器重连时自动带 Last-Event-ID，
// 于是「续传」不需要任何应用层协议。
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

	// 先判定再回放：历史若被动过，用户必须在看到任何内容**之前**被告知
	if v, err := s.store.Verify(ctx); err == nil && !v.OK {
		if ev, ok := alarmEvent(v); ok {
			_ = sess.Send(ev)
		}
	}

	if err := s.replay(ctx, sess, since, limit); err != nil {
		s.log.Warn("回放中断", "err", err)
		return
	}

	head, _ := s.store.Tip(ctx)
	if err := sess.Send(Event{Type: "hello", Data: helloPayload{Head: head, Signed: s.store.Signed()}}); err != nil {
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

// replay 把历史按从旧到新推送；带 since 时只推它之后的部分。
func (s *Server) replay(ctx context.Context, sess *Session, since string, limit int) error {
	history, err := s.store.History(ctx, limit)
	if err != nil {
		return err
	}
	end := len(history) // history 是新的在前
	if since != "" {
		for i, m := range history {
			if m.OID == since {
				end = i // 只推比它更新的
				break
			}
		}
	}
	for i := end - 1; i >= 0; i-- {
		ev, ok := feedEventOf(history[i])
		if !ok {
			continue
		}
		if err := sess.Send(ev); err != nil {
			return err
		}
	}
	return nil
}

// ── 观测 ──────────────────────────────────────────────────────────

type helloPayload struct {
	Head   string `json:"head,omitempty"`
	Signed bool   `json:"signed"`
	Peers  any    `json:"peers,omitempty"`
}

// handleSnapshot 返回各 feed 的当前锚点 —— gossip 快照的数据源。
func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	refs, err := s.repo.Refs(r.Context(), "refs/feeds/")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "refs_failed"})
		return
	}
	verdict, _ := s.store.Verify(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{
		"refs":    refs,
		"witness": verdict.Witness,
		"tip":     verdict.Current,
		"ok":      verdict.OK,
		"reason":  verdict.Reason,
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	head, _ := s.store.Tip(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"head":    head,
		"feed":    s.store.Pub(),
		"signed":  s.store.Signed(),
		"clients": s.hub.ClientCount(),
	})
}

// ── 线协议适配 ────────────────────────────────────────────────────

// feedEventOf 把领域消息翻译成线上事件。撤回不产生新气泡，只更新目标。
func feedEventOf(m feed.Message) (Event, bool) {
	if m.Kind == feed.KindRetract {
		if m.Retracts == "" {
			return Event{}, false
		}
		return Event{ID: m.OID, Type: "retract", Data: map[string]any{
			"oid":      m.OID,
			"retracts": m.Retracts,
			"reason":   m.Reason,
		}}, true
	}
	return Event{ID: m.OID, Type: "msg", Data: m}, true
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
