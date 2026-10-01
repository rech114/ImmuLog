package feed

import (
	"context"
	"sort"
	"strings"

	"immutalk/internal/gitx"
)

// Remote 是一个同步源。URL 走 git 自己的 transport（ssh / https / file / git）。
type Remote struct {
	Name string
	URL  string
}

// PeerView 是本机看到的某个远端的状况。
type PeerView struct {
	Name string `json:"name"`
	URL  string `json:"url"`
	OK   bool   `json:"ok"`
	Tip  string `json:"tip,omitempty"`
	Note string `json:"note,omitempty"`
}

// SyncResult 是一次同步的产物。
type SyncResult struct {
	Advanced []Message  // 这次真正新到的消息（已按链上顺序）
	Alarms   []Verdict  // 检测到的不一致
	Peers    []PeerView // 每个远端的状况
	Reached  int        // 可达的远端数
}

// Sync 从所有远端拉取、逐个 feed 校验、只允许快进地推进本地状态。
//
// remotes 的 Name 必须两两不同 —— 它决定隔离区槽位，重名会互相覆盖。
// （main.go 的 parseRemotes 已经去重；直接调用的调用方需自己保证。）
//
// 四层纪律（docs/DESIGN.md §8.10 / §6.3 / §4.3）：
//
//  1. **网络输入先进隔离区** refs/quarantine/<槽位>/ —— 绝不直接写可信状态
//  2. **每条 feed 都拿本机见证锚比** —— 外来历史同样不许被静默改写
//  3. **只允许快进** —— 非快进一律不动本地，并留下告警
//  4. **密钥链条必须连续** —— 换密钥只能经由旧密钥签名的轮换公告
//
// 远端之间互相矛盾（两个源对同一条 feed 给出无共同后代的两个链尾）判为分裂视图。
func Sync(ctx context.Context, repo *gitx.Repo, remotes []Remote, maxNew int) (SyncResult, error) {
	res := SyncResult{}

	// ① 拉到隔离区
	reachable := make(map[string]bool, len(remotes))
	for _, rm := range remotes {
		slot := FeedID(rm.Name)
		if err := repo.FetchInto(ctx, rm.URL, FeedPrefix+"*", gitx.Quarantine+slot); err != nil {
			reachable[rm.Name] = false
			continue
		}
		reachable[rm.Name] = true
		res.Reached++
	}

	// ② 按 feed 归拢各远端的说法
	type claim struct{ peer, tip string }
	claims := map[string][]claim{}
	slots := map[string]string{} // 槽位 → 远端名
	for _, rm := range remotes {
		slots[FeedID(rm.Name)] = rm.Name
	}

	qr, err := repo.Refs(ctx, gitx.Quarantine)
	if err != nil {
		return res, err
	}
	for _, r := range qr {
		slot, name, ok := strings.Cut(strings.TrimPrefix(r.Name, gitx.Quarantine), "/")
		if !ok || name == "" {
			continue
		}
		peer, ok := slots[slot]
		if !ok {
			continue
		}
		ref := FeedRef(name)
		claims[ref] = append(claims[ref], claim{peer: peer, tip: r.OID})
	}

	bad := map[string]string{} // 远端名 → 问题描述

	// ③ 逐 feed 校验并推进
	feeds := make([]string, 0, len(claims))
	for ref := range claims {
		feeds = append(feeds, ref)
	}
	sort.Strings(feeds)

	for _, ref := range feeds {
		w, err := WitnessOf(ctx, repo, ref)
		if err != nil {
			return res, err
		}
		local, err := repo.Resolve(ctx, ref)
		if err != nil {
			return res, err
		}

		var best string
		for _, cl := range claims[ref] {
			if !reachable[cl.peer] {
				continue
			}
			// 与**本机见证锚**比：外来历史同样不许被静默改写
			if w != "" {
				fwd, err := repo.IsAncestor(ctx, w, cl.tip)
				if err != nil {
					return res, err
				}
				if !fwd {
					back, _ := repo.IsAncestor(ctx, cl.tip, w)
					reason := ReasonRewrite
					if back {
						reason = ReasonRollback
					}
					res.Alarms = append(res.Alarms, Verdict{
						Feed: ref, Peer: cl.peer, Reason: reason,
						Witness: w, Current: cl.tip,
					})
					bad[cl.peer] = "与本机见证锚不一致"
					continue
				}
			}
			// 与**其它远端**比：两个源互相矛盾就是分裂视图
			switch {
			case best == "" || best == cl.tip:
				best = cl.tip
			default:
				fwd, _ := repo.IsAncestor(ctx, best, cl.tip)
				back, _ := repo.IsAncestor(ctx, cl.tip, best)
				if fwd {
					best = cl.tip
				} else if !back {
					res.Alarms = append(res.Alarms, Verdict{
						Feed: ref, Peer: cl.peer, Reason: ReasonSplit,
						Witness: best, Current: cl.tip,
					})
					bad[cl.peer] = "与其它远端互相矛盾"
				}
			}
		}

		if best == "" || best == local {
			continue
		}
		// 只允许快进：非快进一律不动本地（告警已在上面产生）
		if local != "" {
			fwd, err := repo.IsAncestor(ctx, local, best)
			if err != nil || !fwd {
				continue
			}
		}

		// 密钥链条：换密钥必须经由旧密钥签名的轮换公告。
		// 注意这里用 best（OID）而不是 ref —— ref 此刻还指向 local，区间会是空的。
		raw, err := repo.LogRange(ctx, best, local, maxNew)
		if err != nil {
			continue
		}
		prev, err := KeyAt(ctx, repo, local)
		if err != nil {
			continue
		}
		if kv, err := CheckKeyChain(prev, raw); err == nil && !kv.OK {
			kv.Feed = ref
			kv.Peer = claims[ref][0].peer
			res.Alarms = append(res.Alarms, kv)
			bad[kv.Peer] = "密钥链条断裂"
			continue // 拒绝推进
		}

		if err := repo.UpdateRef(ctx, ref, best, local); err != nil {
			continue
		}
		if err := AdvanceWitness(ctx, repo, ref, best); err != nil {
			continue
		}
		res.Advanced = append(res.Advanced, Decode(raw, ref)...)
	}

	// ④ 汇总远端状况
	for _, rm := range remotes {
		v := PeerView{Name: rm.Name, URL: rm.URL, OK: reachable[rm.Name]}
		switch {
		case !reachable[rm.Name]:
			v.Note = "不可达"
		default:
			if note, ok := bad[rm.Name]; ok {
				v.OK, v.Note = false, note
			}
		}
		res.Peers = append(res.Peers, v)
	}
	return res, nil
}

// Publish 把本机 feed 推给每个远端。
//
// 失败**不致命**：本地提交才是事实，远端只是搬运工。
// 返回每个远端的错误（成功的为空串），供 UI 显示可达性。
func Publish(ctx context.Context, repo *gitx.Repo, remotes []Remote, feedRef string) map[string]string {
	out := make(map[string]string, len(remotes))
	if feedRef == "" {
		return out
	}
	for _, rm := range remotes {
		if err := repo.PushRef(ctx, rm.URL, feedRef); err != nil {
			out[rm.Name] = err.Error()
		}
	}
	return out
}
