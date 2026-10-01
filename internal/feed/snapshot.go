package feed

import (
	"context"
	"sort"
	"strings"
	"time"

	"immutalk/internal/gitx"
)

// Snapshot 是某一时刻「本机认为每条 feed 的链尾在哪」的完整记录。
//
// 它的用途是**分裂视图检测**：两个节点各自拍快照，一比就知道
// 是不是有人对不同的人说了不同的话。
type Snapshot struct {
	Digest string         `json:"digest"`
	Refs   []gitx.RefInfo `json:"refs"`
	At     time.Time      `json:"at"`
	Text   string         `json:"-"`
}

// Snapshot 给本机当前认识的全部 feed 拍一张快照。
//
// 摘要 = 排序后的规范文本的 blob 对象名。**内容寻址本身就是 Merkle 叶子**，
// 所以不必自己造 Merkle 树 —— 这是「能复用轮子就复用轮子」。
//
// 为什么不做 RFC 6962 那种 inclusion / consistency proof：
// 那一套是给**轻客户端**用的 —— 它们不持有完整 ref 列表，需要 O(log n) 的
// 包含证明和一致性证明。我们的客户端本来就持有全量 ref，逐条比对比证明**更强**
// 也更简单。真到了要支持轻客户端那天再补。
func Capture(ctx context.Context, repo *gitx.Repo) (Snapshot, error) {
	refs, err := repo.Refs(ctx, FeedPrefix)
	if err != nil {
		return Snapshot{}, err
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Name < refs[j].Name })

	var b strings.Builder
	for _, r := range refs {
		b.WriteString(r.Name)
		b.WriteByte(' ')
		b.WriteString(r.OID)
		b.WriteByte('\n')
	}
	text := b.String()

	digest, err := repo.HashBlob(ctx, []byte(text), false)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{Digest: digest, Refs: refs, Text: text, At: time.Now().UTC()}, nil
}

// tipOf 取快照里某条 feed 的链尾。
func (s Snapshot) tipOf(ref string) string {
	for _, r := range s.Refs {
		if r.Name == ref {
			return r.OID
		}
	}
	return ""
}

// Diverged 逐条比对两张快照，返回**互相矛盾**的 feed 引用。
//
// 判定标准刻意严格：只有两张快照对同一条 feed 给出的链尾互不构成祖先关系，
// 才算矛盾。一方比另一方落后（是祖先）只是"还没同步到"，不是攻击。
//
// 要求两张快照涉及的提交对象都在本地 —— 调用方应先 fetch 再比对。
func Diverged(ctx context.Context, repo *gitx.Repo, mine, theirs Snapshot) ([]string, error) {
	var out []string
	for _, r := range mine.Refs {
		other := theirs.tipOf(r.Name)
		if other == "" || other == r.OID {
			continue
		}
		fwd, err := repo.IsAncestor(ctx, r.OID, other)
		if err != nil {
			return nil, err
		}
		if fwd {
			continue // 对端更新，正常
		}
		back, err := repo.IsAncestor(ctx, other, r.OID)
		if err != nil {
			return nil, err
		}
		if !back {
			out = append(out, r.Name) // 互不构成祖先 ⇒ 分裂
		}
	}
	sort.Strings(out)
	return out, nil
}
