// SPDX-License-Identifier: Apache-2.0

package gitx

import (
	"context"
	"errors"
	"strconv"
	"strings"
)

// RefInfo 是一条引用的快照项。
type RefInfo struct {
	Name string `json:"name"`
	OID  string `json:"oid"`
}

// Count 返回某条引用的链长（提交数）。用于外来 commit 缺 Seq trailer 时兜底。
func (r *Repo) Count(ctx context.Context, ref string) (int, error) {
	out, err := r.run(ctx, nil, "rev-list", "--count", ref)
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	return n, err
}

// UpdateRef 用 CAS 更新引用。
//
// 这是整个防篡改设计的基石：old 与实际不符时 git 会拒绝并返回 ErrCASFailed。
// 「本地见证锚」「拒绝静默改写」都不需要额外代码 —— 就是这一个参数。
func (r *Repo) UpdateRef(ctx context.Context, ref, next, old string) error {
	args := []string{"update-ref", "--no-deref", ref, next}
	if old != "" {
		args = append(args, old)
	}
	_, err := r.run(ctx, nil, args...)
	return err
}

// EnsureRef 只在引用不存在时创建它（old 固定为全零）。
func (r *Repo) EnsureRef(ctx context.Context, ref, next string) error {
	return r.UpdateRef(ctx, ref, next, strings.Repeat("0", 40))
}

// DeleteRef 删除引用；old 非空时同样做 CAS。
func (r *Repo) DeleteRef(ctx context.Context, ref, old string) error {
	args := []string{"update-ref", "--no-deref", "-d", ref}
	if old != "" {
		args = append(args, old)
	}
	_, err := r.run(ctx, nil, args...)
	return err
}

// Resolve 返回引用的当前值；不存在时返回空串而非错误。
func (r *Repo) Resolve(ctx context.Context, ref string) (string, error) {
	out, err := r.run(ctx, nil, "rev-parse", "--verify", "--quiet", ref)
	if err != nil {
		var ge *Error
		if errors.As(err, &ge) && (ge.Code == 1 || ge.Code == 128) {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// Refs 一次调用读回某命名空间下的全部引用。
// 这是 gossip 快照（`{feed → tip}` 列表）的数据源 —— 一次进程调用拿到全量。
func (r *Repo) Refs(ctx context.Context, prefix string) ([]RefInfo, error) {
	args := []string{"for-each-ref", "--format=%(refname)%1f%(objectname)"}
	if prefix != "" {
		args = append(args, prefix)
	}
	out, err := r.run(ctx, nil, args...)
	if err != nil {
		return nil, err
	}
	var refs []RefInfo
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\x1f", 2)
		if len(parts) != 2 {
			continue
		}
		refs = append(refs, RefInfo{Name: parts[0], OID: strings.TrimSpace(parts[1])})
	}
	return refs, nil
}

// IsAncestor 判断 a 是否为 b 的祖先。
//
// 「引用重写」的判定就落在这一句上：若见证锚不再是当前 tip 的祖先，
// 说明历史被人动过 —— 这正是 docs/DESIGN.md §2 说的那四张牌里的三张。
func (r *Repo) IsAncestor(ctx context.Context, a, b string) (bool, error) {
	if a == "" || b == "" {
		return false, nil
	}
	_, err := r.run(ctx, nil, "merge-base", "--is-ancestor", a, b)
	if err == nil {
		return true, nil
	}
	var ge *Error
	if errors.As(err, &ge) && ge.Code == 1 {
		return false, nil // 1 = 不是祖先，是正常答案而非错误
	}
	return false, err
}
