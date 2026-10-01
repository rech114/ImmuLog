// SPDX-License-Identifier: Apache-2.0

package feed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"immulog/core/gitx"
)

// anchorRef 是锚定链的链尾。锚定只追加，永不改写。
const anchorRef = "refs/anchors/latest"

// anchorPrefix 供外部遍历锚定链。
const anchorPrefix = "refs/anchors/"

// maxReceipt 是外部回执的截断长度。
const maxReceipt = 512

// Publisher 把快照摘要交给一个**本机之外**的归宿，返回回执。
//
// 这是「外部锚定」的全部接口。真正的**外部性**来自这一跳：
// 只要回执落在别人的地盘上，事后谁都无法统一口径。
type Publisher interface {
	Publish(ctx context.Context, digest string) (receipt string, err error)
}

// Anchor 是一次锚定。
type Anchor struct {
	OID      string    `json:"oid"`
	Seq      int       `json:"seq"`
	Snapshot string    `json:"snapshot"`
	Prev     string    `json:"prev,omitempty"`
	At       time.Time `json:"at"`
	External string    `json:"external,omitempty"`
}

// AnchorNow 把当前快照追加进锚定链。
//
// 每个锚定的 parent 是上一个锚定 —— **链由 git 的哈希链保证**：
// 想改写历史里的某个锚定，必须连带改写它之后的全部锚定，
// 而客户端手里可能还留着旧的。这与撤回的处理是同一条哲学。
func AnchorNow(ctx context.Context, repo *gitx.Repo, pub Publisher, sign bool) (Anchor, error) {
	snap, err := Capture(ctx, repo)
	if err != nil {
		return Anchor{}, err
	}
	prev, err := repo.Resolve(ctx, anchorRef)
	if err != nil {
		return Anchor{}, err
	}
	now := time.Now().UTC()

	// 外部失败不阻断本地锚定：本地链本身已经有价值
	receipt := ""
	if pub != nil {
		if r, err := pub.Publish(ctx, snap.Digest); err == nil {
			// 回执是外来字符串：压成单行，否则一个换行就能伪造 trailer
			receipt = truncate(sanitizeValue(r), maxReceipt)
		}
	}

	// 规范文本在前，trailer 块在后，中间空一行 —— 与消息编解码同一套约定
	var b strings.Builder
	b.WriteString(snap.Text)
	b.WriteByte('\n')
	b.WriteString("ImmuLog-Snapshot: " + snap.Digest + "\n")
	b.WriteString("ImmuLog-At: " + now.Format(time.RFC3339) + "\n")
	if receipt != "" {
		b.WriteString("ImmuLog-External: " + receipt + "\n")
	}

	tree, err := repo.EmptyTree(ctx)
	if err != nil {
		return Anchor{}, err
	}
	oid, err := repo.Commit(ctx, tree, prev, b.String(), sign)
	if err != nil {
		return Anchor{}, err
	}
	// CAS：并发锚定时不覆盖，重试即可
	if err := repo.UpdateRef(ctx, anchorRef, oid, prev); err != nil {
		return Anchor{}, err
	}

	seq := 0
	if n, err := repo.Count(ctx, anchorRef); err == nil {
		seq = n
	}
	return Anchor{OID: oid, Seq: seq, Snapshot: snap.Digest, Prev: prev, At: now, External: receipt}, nil
}

// AnchorHead 读回链尾锚定；从未锚定过时返回零值。
func AnchorHead(ctx context.Context, repo *gitx.Repo) (Anchor, error) {
	head, err := repo.Resolve(ctx, anchorRef)
	if err != nil || head == "" {
		return Anchor{}, err
	}
	vals, err := repo.TrailerValue(ctx, head,
		"ImmuLog-Snapshot", "ImmuLog-At", "ImmuLog-External")
	if err != nil {
		return Anchor{}, err
	}
	a := Anchor{OID: head, Snapshot: vals[0], External: vals[2]}
	if t, perr := time.Parse(time.RFC3339, vals[1]); perr == nil {
		a.At = t.UTC()
	}
	if n, cerr := repo.Count(ctx, anchorRef); cerr == nil {
		a.Seq = n
	}
	return a, nil
}

// HTTPPublisher 把摘要 POST 给一个外部服务，把响应体当作回执。
//
// 接 OpenTimestamps 网关、RFC3161 网关、自建公证服务都行 —— 换服务只是换 URL。
// 这也是唯一需要写代码的"外部"部分：**真正的信任边界在 URL 上**。
type HTTPPublisher struct {
	URL    string
	Client *http.Client
}

// Publish 实现 Publisher。
func (p HTTPPublisher) Publish(ctx context.Context, digest string) (string, error) {
	client := p.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	body, err := json.Marshal(map[string]string{
		"snapshot": digest,
		"at":       time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.URL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("锚定服务返回 %d", resp.StatusCode)
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, maxReceipt))
	return strings.TrimSpace(string(b)), nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
