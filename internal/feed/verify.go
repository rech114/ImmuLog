package feed

import (
	"context"
)

// 见证锚是本地引用 refs/witness/<pub>：只由本机推进，**从不推送、从不 fetch**。
// 攻击者即使拿到远端控制权也碰不到它 —— 这是 docs/DESIGN.md §6.3 的 L1。
//
// 判定只用 git 自己的原语：
//
//	git merge-base --is-ancestor <见证锚> <当前 tip>
//
// 引用重写（force push）与回滚都会让见证锚不再是当前 tip 的祖先。
// 一句话概括：**引用重写不需要「防」，只需要「被发现」。**
const witnessPrefix = "refs/witness/"

// 判定结果的原因码。
const (
	ReasonRewrite  = "rewrite"  // 当前 tip 不是见证锚的后代
	ReasonRollback = "rollback" // 序号倒退（更细的信号）
)

// Verdict 是一次完整性判定的结果。
type Verdict struct {
	OK      bool   `json:"ok"`
	Reason  string `json:"reason,omitempty"`
	Witness string `json:"witness,omitempty"`
	Current string `json:"current,omitempty"`
}

func (s *Store) witnessRef() string { return witnessPrefix + s.pub }

// Witness 返回当前见证锚；未建立时返回空串。
func (s *Store) Witness(ctx context.Context) (string, error) {
	return s.repo.Resolve(ctx, s.witnessRef())
}

// Verify 判定当前 tip 是否仍以见证锚为祖先。
//
// 对线性 feed 而言，这一个 git 原语就足够完备：
//
//	· 回滚 —— tip 变成见证锚的**祖先**   ⇒ IsAncestor(见证锚, tip) = false
//	· 改写 —— 两者变成两条无共同后代的链 ⇒ IsAncestor(见证锚, tip) = false
//
// 再反向查一次即可区分二者，给出更准确的告警文案（异常路径才多一次调用）。
func (s *Store) Verify(ctx context.Context) (Verdict, error) {
	w, err := s.Witness(ctx)
	if err != nil {
		return Verdict{}, err
	}
	cur, err := s.repo.Resolve(ctx, s.ref)
	if err != nil {
		return Verdict{}, err
	}

	v := Verdict{Witness: w, Current: cur}
	if w == "" || cur == "" || w == cur {
		v.OK = true
		return v, nil
	}

	fwd, err := s.repo.IsAncestor(ctx, w, cur)
	if err != nil {
		return v, err
	}
	if fwd {
		v.OK = true
		return v, nil
	}

	// 当前 tip 是见证锚的祖先 ⇒ 链被指回了更早的点
	if back, err := s.repo.IsAncestor(ctx, cur, w); err == nil && back {
		v.Reason = ReasonRollback
	} else {
		v.Reason = ReasonRewrite
	}
	return v, nil
}

// advanceWitness 把见证锚推进到 oid。只在确认一致后调用。
func (s *Store) advanceWitness(ctx context.Context, oid string) error {
	return s.repo.UpdateRef(ctx, s.witnessRef(), oid, "")
}
