package gitx

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"
)

// logFormat 一次进程调用读回一页历史。
//
// 用 `git log -z`（记录之间是 NUL）+ 字段之间 \x1f，并且把 %B 放在**最后**：
// 于是正文里出现 \x1e / 任意控制字符都不会把记录切错。
const logFormat = "%H%x1f%an%x1f%at%x1f" +
	"%(trailers:key=Immutalk-Seq,valueonly)%x1f" +
	"%(trailers:key=Immutalk-Retracts,valueonly)%x1f" +
	"%(trailers:key=Immutalk-Reason,valueonly)%x1f" +
	"%B"

// RawCommit 是 log 解析出的一条原始提交。
// Trailer 三项由 git 自己解析（不重造轮子）；Body 是完整原始正文，由 feed 剥离。
type RawCommit struct {
	OID      string
	Author   string
	At       time.Time
	Seq      string
	Retracts string
	Reason   string
	Body     string
}

// EmptyTree 返回空 tree 的对象名并写入对象库。
// 消息不产生任何文件，所有 commit 都复用这棵空树。
func (r *Repo) EmptyTree(ctx context.Context) (string, error) {
	out, err := r.run(context.WithoutCancel(ctx), []byte{}, "hash-object", "-w", "-t", "tree", "--stdin")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// Commit 造一个 commit 对象。parent 为空表示链的起点。
//
// 这里刻意不用 `git commit`：commit-tree 不需要 index、不需要 worktree、
// 不产生 .lock，因此在 bare 仓库里可用，且并发写不同 ref 天然安全。
// 正文从 stdin 进入，天然免除参数注入。
func (r *Repo) Commit(ctx context.Context, tree, parent, message string, sign bool) (string, error) {
	args := []string{"commit-tree", tree}
	if parent != "" {
		args = append(args, "-p", parent)
	}
	if sign {
		args = append(args, "-S")
	}
	out, err := r.run(ctx, []byte(message), args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// Exists 判断对象是否存在于本地对象库。
func (r *Repo) Exists(ctx context.Context, oid string) (bool, error) {
	if oid == "" {
		return false, nil
	}
	_, err := r.run(ctx, nil, "cat-file", "-e", oid+"^{commit}")
	if err == nil {
		return true, nil
	}
	var ge *Error
	if errors.As(err, &ge) && (ge.Code == 1 || ge.Code == 128) {
		return false, nil
	}
	return false, err
}

// Log 一次调用读回一页历史，按提交时间倒序。
func (r *Repo) Log(ctx context.Context, ref string, limit int) ([]RawCommit, error) {
	if ref == "" {
		return nil, nil
	}
	args := []string{"log", "-z", "--format=" + logFormat}
	if limit > 0 {
		args = append(args, "-n", strconv.Itoa(limit))
	}
	args = append(args, ref)

	out, err := r.run(ctx, nil, args...)
	if err != nil {
		var ge *Error
		// 空仓库 / 未知 ref：语义上等于没有历史，不是错误
		if errors.As(err, &ge) && (ge.Code == 128 || ge.Code == 1) {
			return nil, nil
		}
		return nil, err
	}
	return parseLog(string(out)), nil
}

func parseLog(raw string) []RawCommit {
	var commits []RawCommit
	for _, rec := range strings.Split(raw, "\x00") {
		rec = strings.Trim(rec, "\n")
		if rec == "" {
			continue
		}
		// 只切前 6 个分隔符，正文（第 7 项）原样保留，哪怕它含 \x1f
		f := strings.SplitN(rec, "\x1f", 7)
		if len(f) != 7 {
			continue
		}
		c := RawCommit{
			OID:      strings.TrimSpace(f[0]),
			Author:   strings.TrimSpace(f[1]),
			Seq:      strings.TrimSpace(f[3]),
			Retracts: strings.TrimSpace(f[4]),
			Reason:   strings.TrimSpace(f[5]),
			Body:     f[6],
		}
		if secs, err := strconv.ParseInt(strings.TrimSpace(f[2]), 10, 64); err == nil {
			c.At = time.Unix(secs, 0).UTC()
		}
		if c.OID != "" {
			commits = append(commits, c)
		}
	}
	return commits
}
