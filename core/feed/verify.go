// SPDX-License-Identifier: Apache-2.0

package feed

import (
	"context"
	"strings"

	"immulog/core/gitx"
)

// feed 与 witness 的命名空间。
const (
	FeedPrefix    = "refs/feeds/"
	witnessPrefix = "refs/witness/"
)

// FeedRef 由 feed 名构造引用名。
func FeedRef(name string) string { return FeedPrefix + name }

// FeedName 从引用名取回 feed 名。
func FeedName(ref string) string { return strings.TrimPrefix(ref, FeedPrefix) }

// WitnessRef 由 feed 引用派生见证引用。
//
// 见证锚只由本机推进，**从不推送、从不 fetch** —— 攻击者即使拿到远端控制权
// 也碰不到它。这是 docs/DESIGN.md §6.3 的 L1。
func WitnessRef(feedRef string) string {
	return witnessPrefix + FeedName(feedRef)
}

// 判定结果的原因码。
const (
	ReasonRewrite  = "rewrite"  // 链尾不是见证锚的后代（平行链，即 force push）
	ReasonRollback = "rollback" // 链尾是见证锚的祖先（指回了更早的点）
	ReasonSplit    = "split"    // 两个远端互相矛盾（分裂视图）
)

// Verdict 是一次完整性判定的结果。
type Verdict struct {
	OK      bool   `json:"ok"`
	Reason  string `json:"reason,omitempty"`
	Feed    string `json:"feed,omitempty"`
	Peer    string `json:"peer,omitempty"`
	Witness string `json:"witness,omitempty"`
	Current string `json:"current,omitempty"`
}

// WitnessOf 读某条 feed 的见证锚；未建立时返回空串。
func WitnessOf(ctx context.Context, repo *gitx.Repo, feedRef string) (string, error) {
	return repo.Resolve(ctx, WitnessRef(feedRef))
}

// VerifyRef 判定 feedRef 的链尾是否仍以它的见证锚为祖先。
//
// 对线性 feed 而言一个 git 原语就完备：
//
//	回滚 —— 链尾变成见证锚的**祖先**
//	改写 —— 两者变成两条无共同后代的链
//
// 两种情况都让 IsAncestor(见证锚, 链尾) 为 false；再反向查一次即可区分，
// 给出更准确的告警文案（只有异常路径才多一次调用）。
//
// 一句话概括设计立场：**引用重写不需要「防」，只需要「被发现」。**
func VerifyRef(ctx context.Context, repo *gitx.Repo, feedRef string) (Verdict, error) {
	w, err := WitnessOf(ctx, repo, feedRef)
	if err != nil {
		return Verdict{}, err
	}
	cur, err := repo.Resolve(ctx, feedRef)
	if err != nil {
		return Verdict{}, err
	}

	v := Verdict{Feed: feedRef, Witness: w, Current: cur}
	if w == "" || cur == "" || w == cur {
		v.OK = true
		return v, nil
	}

	fwd, err := repo.IsAncestor(ctx, w, cur)
	if err != nil {
		return v, err
	}
	if fwd {
		v.OK = true
		return v, nil
	}

	if back, err := repo.IsAncestor(ctx, cur, w); err == nil && back {
		v.Reason = ReasonRollback
	} else {
		v.Reason = ReasonRewrite
	}
	return v, nil
}

// AdvanceWitness 把见证锚推进到 oid。**只在确认一致后调用。**
func AdvanceWitness(ctx context.Context, repo *gitx.Repo, feedRef, oid string) error {
	return repo.UpdateRef(ctx, WitnessRef(feedRef), oid, "")
}

// ── Store 上的便捷方法（本机 feed）────────────────────────────────

// Witness 返回本机 feed 的见证锚。
func (s *Store) Witness(ctx context.Context) (string, error) {
	return WitnessOf(ctx, s.repo, s.ref)
}

// Verify 判定本机 feed 是否仍自洽。
func (s *Store) Verify(ctx context.Context) (Verdict, error) {
	return VerifyRef(ctx, s.repo, s.ref)
}

func (s *Store) advanceWitness(ctx context.Context, oid string) error {
	return AdvanceWitness(ctx, s.repo, s.ref, oid)
}
