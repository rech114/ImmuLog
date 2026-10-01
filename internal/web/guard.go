// SPDX-License-Identifier: AGPL-3.0-or-later

// guard.go —— 后台巡检：完整性、多源同步、外部锚定。
//
// 三件事各一个循环，各一个间隔。它们产生的告警不是 toast，
// 而是**插进时间线里的一条封条**（docs/DESIGN.md §8.5）：客户端渲染成
// 不可自动消失的卡片，攻击者删不掉。
package web

import (
	"context"
	"sync"
	"time"

	"immulog/core/feed"
)

// 后台循环的默认间隔。环境变量可覆盖（测试与运维都靠它）。
const (
	IntegrityInterval = 5 * time.Second
	SyncInterval      = 5 * time.Second
	AnchorInterval    = 60 * time.Second
)

// syncBatch 是单次同步每条 feed 最多取回多少条新消息。
const syncBatch = 200

// State 是本机对外的整体状况：由后台循环维护，由 handler 读取。
type State struct {
	mu     sync.RWMutex
	snap   feed.Snapshot
	anchor feed.Anchor
	peers  []feed.PeerView
}

// Snapshot 读回当前状况的一份拷贝。
func (s *State) Snapshot() (feed.Snapshot, feed.Anchor, []feed.PeerView) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snap, s.anchor, s.peers
}

func (s *State) setSnapshot(snap feed.Snapshot) {
	s.mu.Lock()
	s.snap = snap
	s.mu.Unlock()
}

func (s *State) setAnchor(a feed.Anchor) {
	s.mu.Lock()
	s.anchor = a
	s.mu.Unlock()
}

func (s *State) setPeers(p []feed.PeerView) {
	s.mu.Lock()
	s.peers = p
	s.mu.Unlock()
}

// Watch 启动三个后台循环并立即返回。ctx 取消即全部退出。
func (s *Server) Watch(ctx context.Context, syncEvery, anchorEvery time.Duration) {
	if syncEvery <= 0 {
		syncEvery = SyncInterval
	}
	if anchorEvery <= 0 {
		anchorEvery = AnchorInterval
	}
	go s.loop(ctx, syncEvery, s.syncOnce)
	go s.loop(ctx, anchorEvery, s.anchorOnce)
	go s.loop(ctx, IntegrityInterval, s.verifyOnce)
	go s.pushLoop(ctx)
}

func (s *Server) loop(ctx context.Context, every time.Duration, fn func(context.Context)) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			fn(ctx)
		}
	}
}

// syncOnce 从所有远端拉取、校验、只允许快进地推进，然后广播增量。
func (s *Server) syncOnce(ctx context.Context) {
	res, err := feed.Sync(ctx, s.repo, s.remotes, syncBatch)
	if err != nil {
		s.log.Warn("同步失败", "err", err)
		return
	}
	if len(s.remotes) > 0 {
		s.state.setPeers(res.Peers)
	}
	for _, m := range res.Advanced {
		if ev, ok := feedEventOf(m); ok {
			s.hub.Broadcast(ev)
		}
	}
	for _, v := range res.Alarms {
		s.log.Warn("检测到不一致",
			"feed", v.Feed, "peer", v.Peer, "reason", v.Reason,
			"witness", v.Witness, "current", v.Current)
		s.announce(v)
	}
	s.refreshSnapshot(ctx)
}

// verifyOnce 巡检本机 feed。本机是唯一写者，正常情况下这里永远静默。
func (s *Server) verifyOnce(ctx context.Context) {
	v, err := s.store.Verify(ctx)
	if err != nil {
		s.log.Warn("完整性校验失败", "err", err)
		return
	}
	if !v.OK {
		s.announce(v)
		return
	}
	s.refreshSnapshot(ctx)
}

// anchorOnce 把当前快照追加进锚定链，并尽力交给外部服务。
func (s *Server) anchorOnce(ctx context.Context) {
	a, err := feed.AnchorNow(ctx, s.repo, s.publisher, s.store.Signed())
	if err != nil {
		s.log.Warn("锚定失败", "err", err)
		return
	}
	s.state.setAnchor(a)
	s.log.Info("已锚定快照",
		"seq", a.Seq, "snapshot", short(a.Snapshot), "external", a.External != "")
}

func (s *Server) refreshSnapshot(ctx context.Context) {
	if snap, err := feed.Capture(ctx, s.repo); err == nil {
		s.state.setSnapshot(snap)
	}
}

// announce 去重后广播告警：同一个问题只播一次，避免刷屏。
func (s *Server) announce(v feed.Verdict) {
	key := v.Feed + "|" + v.Reason + "|" + v.Peer + "|" + v.Current
	s.muGuard.Lock()
	seen := s.guardSeen[key]
	if s.guardSeen == nil {
		s.guardSeen = map[string]bool{}
	}
	s.guardSeen[key] = true
	s.muGuard.Unlock()
	if seen {
		return
	}
	if ev, ok := alarmEvent(v); ok {
		s.hub.Broadcast(ev)
	}
}

// alarmEvent 把一次判定翻译成 alarm 事件。判定通过时返回 false。
func alarmEvent(v feed.Verdict) (Event, bool) {
	if v.OK || v.Reason == "" {
		return Event{}, false
	}

	title, detail := "检测到历史改写",
		"本地见证锚已不是当前链尾的祖先：有人重写了这段历史，且没有留下重写公告。本地副本已保留，拒绝覆盖。"
	switch v.Reason {
	case feed.ReasonRollback:
		title = "检测到历史回滚"
		detail = "消息序号出现倒退：链尾被指回了一个更早的点。"
	case feed.ReasonSplit:
		title = "检测到分裂视图"
		detail = "两个远端对同一条 feed 给出了互不构成祖先关系的链尾：有人在对你和他人说不同的话。"
	case feed.ReasonKeyChanged:
		title = "检测到密钥被换掉"
		detail = "链上出现了没有轮换公告背书的密钥变更：新密钥既不是上一条的，也没有被上一条签名声明。日志里那条消息不是本人发的。"
	}
	if v.Peer != "" {
		detail += "（来源：" + v.Peer + "）"
	}

	return Event{
		ID:   v.Current + "|" + v.Reason,
		Type: "alarm",
		Data: map[string]any{
			"oid":    v.Current,
			"title":  title,
			"detail": detail,
			"local":  short(v.Witness),
			"remote": short(v.Current),
			"reason": v.Reason,
			"feed":   feed.FeedName(v.Feed),
			"peer":   v.Peer,
		},
	}, true
}

func short(oid string) string {
	if len(oid) > 6 {
		return oid[:6]
	}
	return oid
}
