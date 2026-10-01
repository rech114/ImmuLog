// SPDX-License-Identifier: AGPL-3.0-or-later

// sse.go —— SSE 传输层。纯管道：不懂消息语义，只负责把 Event 写到线上。
//
// 用 text/event-stream 而不是 WebSocket 的三个理由（docs/DESIGN.md §7.10）：
//
//	· 零依赖 —— net/http + Flusher 就够，不需要 gorilla/websocket
//	· 浏览器原生 EventSource 自带重连，不用自己写
//	· 事件 id 就是 commit OID，重连时浏览器自动带 Last-Event-ID ⇒ 断线续传白送
package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
)

const (
	subBuffer   = 64
	retryMillis = 3000
)

// Heartbeat 是心跳间隔。发一行注释，防止中间代理掐断空闲连接。
const Heartbeat = 20 * time.Second

// ErrNoFlush 表示底层 ResponseWriter 不支持流式推送。
var ErrNoFlush = errors.New("sse: ResponseWriter 不支持 Flusher")

// Event 是一条待推送的事件。ID 即 commit OID。
type Event struct {
	ID   string
	Type string
	Data any
}

// Hub 是广播中心。慢客户端直接断开，让它重连走 Last-Event-ID 续传 ——
// 宁可丢弃也绝不阻塞广播。
type Hub struct {
	mu      sync.Mutex
	clients map[*subscriber]struct{}
}

type subscriber struct {
	ch   chan Event
	done chan struct{}
}

// NewHub 建一个空的广播中心。
func NewHub() *Hub { return &Hub{clients: make(map[*subscriber]struct{})} }

// Subscribe 注册一个订阅者；返回事件通道与注销函数。
func (h *Hub) Subscribe() (<-chan Event, func()) {
	s := &subscriber{ch: make(chan Event, subBuffer), done: make(chan struct{})}
	h.mu.Lock()
	h.clients[s] = struct{}{}
	h.mu.Unlock()

	return s.ch, func() {
		h.mu.Lock()
		if _, ok := h.clients[s]; ok {
			delete(h.clients, s)
			close(s.done)
		}
		h.mu.Unlock()
	}
}

// Broadcast 非阻塞地推给所有订阅者。
func (h *Hub) Broadcast(ev Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.clients {
		select {
		case s.ch <- ev:
		default:
			delete(h.clients, s) // 慢客户端：断开，让它重连续传
			close(s.done)
		}
	}
}

// ClientCount 返回当前订阅者数量（供 /api/health 观测）。
func (h *Hub) ClientCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients)
}

// Session 是一条已建立的 SSE 连接。
type Session struct {
	w http.ResponseWriter
	f http.Flusher
}

// NewSession 写好响应头并返回可写的会话。
func NewSession(w http.ResponseWriter) (*Session, error) {
	f, ok := w.(http.Flusher)
	if !ok {
		return nil, ErrNoFlush
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("X-Accel-Buffering", "no") // 让反代不要缓冲
	w.WriteHeader(http.StatusOK)
	return &Session{w: w, f: f}, nil
}

// Open 下发重连间隔并立刻 flush，让浏览器知道这是一条流。
func (s *Session) Open() error {
	return s.raw(fmt.Sprintf("retry: %d\n\n", retryMillis))
}

// Send 写一条事件。id 用 commit OID —— 续传游标就是内容哈希本身。
func (s *Session) Send(ev Event) error {
	var prefix string
	if ev.ID != "" {
		prefix += "id: " + ev.ID + "\n"
	}
	if ev.Type != "" {
		prefix += "event: " + ev.Type + "\n"
	}
	payload, err := json.Marshal(ev.Data)
	if err != nil {
		return err
	}
	return s.raw(prefix + "data: " + string(payload) + "\n\n")
}

// Ping 发一行注释保活。
func (s *Session) Ping() error { return s.raw(": ping\n\n") }

func (s *Session) raw(text string) error {
	if _, err := s.w.Write([]byte(text)); err != nil {
		return err
	}
	s.f.Flush()
	return nil
}
