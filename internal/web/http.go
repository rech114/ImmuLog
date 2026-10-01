// http.go —— 路由与处理器。只做协议适配，不含领域逻辑，不含传输细节。
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

	"immutalk/internal/feed"
	"immutalk/internal/gitx"
)

// 每页回放的条数。
const (
	replayFirst   = 50  // 首屏
	replayResume  = 200 // 断线续传
	maxUploadSize = feed.MaxBody * 2
	pushQueue     = 1 // 推送请求合并：连点只推一次
)

// Config 是组装 Server 需要的外部依赖。
type Config struct {
	Store     *feed.Store
	Hub       *Hub
	Repo      *gitx.Repo
	Files     fs.FS
	Remotes   []feed.Remote
	Publisher feed.Publisher
}

// Server 把领域层与传输层接起来。
type Server struct {
	store     *feed.Store
	hub       *Hub
	repo      *gitx.Repo
	files     fs.FS
	remotes   []feed.Remote
	publisher feed.Publisher
	state     *State
	log       *slog.Logger

	pushReq chan struct{}

	muGuard   sync.Mutex
	guardSeen map[string]bool
}

// New 组装一个 Server。
func New(cfg Config) *Server {
	return &Server{
		store:     cfg.Store,
		hub:       cfg.Hub,
		repo:      cfg.Repo,
		files:     cfg.Files,
		remotes:   cfg.Remotes,
		publisher: cfg.Publisher,
		state:     &State{},
		log:       slog.Default(),
		pushReq:   make(chan struct{}, pushQueue),
		guardSeen: map[string]bool{},
	}
}

// Handler 返回完整的路由表。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/stream", s.handleStream)
	mux.HandleFunc("POST /api/commit", s.handleCommit)
	mux.HandleFunc("POST /api/rotate", s.handleRotate)
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
	s.kick()                        // 后台合并推送，不阻塞请求路径
	s.refreshSnapshot(r.Context())
	writeJSON(w, http.StatusCreated, map[string]any{"oid": msg.OID, "seq": msg.Seq})
}

// commitStatus 把领域错误映射成 HTTP 状态码。err 为 nil 时返回 0。
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

// ── 密钥轮换 ──────────────────────────────────────────────────────

type rotateReq struct {
	Key string `json:"key"`
}

// handleRotate 追加一条密钥轮换公告。
//
// **由当前（旧）密钥签名，声明新密钥** —— 所以顺序是「先调这个，再改配置」。
// 详见 feed.Store.DeclareKey 的文档。
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
		// 没在签名的 feed 上谈轮换毫无意义 —— 拒绝，而不是悄悄换掉身份
		writeJSON(w, http.StatusConflict, map[string]any{"error": "signing_required"})
	default:
		s.log.Warn("轮换失败", "err", err)
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "rotate_failed"})
	}
}

// kick 请求一次后台推送；已有待处理的请求就合并掉（聚合推送，见 §10 纪律 6）。
func (s *Server) kick() {
	select {
	case s.pushReq <- struct{}{}:
	default:
	}
}

// pushLoop 把本机 feed 推给所有远端。失败不致命 —— 本地提交才是事实。
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
				s.log.Warn("推送失败", "remote", name, "err", msg)
			}
		}
	}
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
	// 从旧到新：先收集要发的，再逆序发出
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

// ── 观测 ──────────────────────────────────────────────────────────

type identityPayload struct {
	Signed bool   `json:"signed"`
	Key    string `json:"key,omitempty"`
}

type helloPayload struct {
	Head       string          `json:"head,omitempty"`
	Signed     bool            `json:"signed"`
	Identity   identityPayload `json:"identity"`
	Snapshot   string          `json:"snapshot,omitempty"`
	AnchoredAt string          `json:"anchoredAt,omitempty"`
	External   bool            `json:"external"`
	Peers      []feed.PeerView `json:"peers,omitempty"`
}

func (s *Server) hello(ctx context.Context) helloPayload {
	head, _ := s.store.Tip(ctx)
	snap, anchor, peers := s.state.Snapshot()

	p := helloPayload{
		Head:     head,
		Signed:   s.store.Signed(),
		Snapshot: snap.Digest,
		Peers:    peers,
		Identity: identityPayload{Signed: s.store.Signed()},
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

// handleSnapshot 返回本机对全部 feed 的看法 —— gossip 的交换单位。
func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	snap, anchor, peers := s.state.Snapshot()
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
		"digest":   snap.Digest,
		"refs":     snap.Refs,
		"at":       snap.At,
		"anchor":   anchor,
		"peers":    peers,
		"identity": identity,
		"witness":  verdict.Witness,
		"tip":      verdict.Current,
		"ok":       verdict.OK,
		"reason":   verdict.Reason,
		"feed":     s.store.FeedRef(),
		"remotes":  s.remotes,
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
		"anchored": anchor.OID != "",
	})
}

// ── 线协议适配 ────────────────────────────────────────────────────

// feedEventOf 把领域消息翻译成线上事件。撤回不产生新气泡，只更新目标。
//
// 事件里带上 feed：客户端据此保证「撤回只对同一条 feed 内的消息生效」，
// 于是撤回是作者的权利，而不是谁都能对别人做的事。
//
// 密钥轮换公告是**结构性事件**，不进时间线（它的作用体现在身份区块与
// 链条校验上），所以返回 false。
func feedEventOf(m feed.Message) (Event, bool) {
	switch m.Kind {
	case feed.KindRotate:
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
