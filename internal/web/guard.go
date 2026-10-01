// guard.go —— 完整性守护：周期性校验并广播告警。
//
// 告警不是 toast，是**插入时间线里的一条封条**（docs/DESIGN.md §8.5）：
// 它由客户端渲染成不可自动消失的卡片，攻击者删不掉。
package web

import (
	"context"
	"time"

	"immutalk/internal/feed"
)

// GuardInterval 是完整性巡检间隔。
const GuardInterval = 5 * time.Second

// alarmEvent 把一次判定翻译成 alarm 事件。判定通过时返回 false。
func alarmEvent(v feed.Verdict) (Event, bool) {
	if v.OK || v.Reason == "" {
		return Event{}, false
	}
	title := "检测到历史改写"
	detail := "本地见证锚已不是当前链尾的祖先：有人重写了这段历史，且没有留下重写公告。本地副本已保留，拒绝覆盖。"
	if v.Reason == feed.ReasonRollback {
		title = "检测到历史回滚"
		detail = "消息序号出现倒退：链尾被指回了一个更早的点。"
	}
	return Event{
		ID:   v.Current,
		Type: "alarm",
		Data: map[string]any{
			"oid":    v.Current,
			"title":  title,
			"detail": detail,
			"local":  short(v.Witness),
			"remote": short(v.Current),
			"reason": v.Reason,
		},
	}, true
}

// Watch 周期性校验本机 feed；一旦发现不一致就广播告警。
// 本机是唯一写者，所以正常情况下这里永远是静默的。
func (s *Server) Watch(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = GuardInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()

	lastReason := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			v, err := s.store.Verify(ctx)
			if err != nil {
				s.log.Warn("完整性校验失败", "err", err)
				continue
			}
			if v.OK {
				lastReason = ""
				continue
			}
			if v.Reason == lastReason {
				continue // 同一个告警只播一次
			}
			lastReason = v.Reason
			if ev, ok := alarmEvent(v); ok {
				s.hub.Broadcast(ev)
			}
		}
	}
}

func short(oid string) string {
	if len(oid) > 6 {
		return oid[:6]
	}
	return oid
}
