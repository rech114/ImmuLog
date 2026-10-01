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
//
// %G? 是 git 自己的签名状态：N=未签名 G=有效 U=密钥未知 B=签名损坏。
// %GK 是签名者的密钥指纹 —— 密钥链条就是靠它逐条串起来的。
const logFormat = "%H%x1f%an%x1f%at%x1f%G?%x1f%GK%x1f" +
	"%(trailers:key=Immutalk-Seq,valueonly)%x1f" +
	"%(trailers:key=Immutalk-Retracts,valueonly)%x1f" +
	"%(trailers:key=Immutalk-Reason,valueonly)%x1f" +
	"%B"

// RawCommit 是 log 解析出的一条原始提交。
// Trailer 由 git 自己解析（不重造轮子）；Kind / Declared 从正文尾部的
// trailer 段落里取，与 git 的 trailer 语法同一套规则。
type RawCommit struct {
	OID      string
	Author   string
	At       time.Time
	Sig      string // "" | "good" | "untrusted" | "bad"
	Key      string // 签名密钥指纹；未签名时为空
	Seq      string
	Retracts string
	Reason   string
	Kind     string // `Immutalk-Kind` trailer
	Declared string // `Immutalk-Key` trailer（只有轮换公告才有）
	Body     string
}

// sigStatus 把 git 的 %G? 单字符翻译成人能读的状态。
func sigStatus(c string) string {
	switch c {
	case "G":
		return "good"
	case "U":
		return "untrusted"
	case "B":
		return "bad"
	default:
		return ""
	}
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
	return r.commitTree(ctx, args, message)
}

// CommitAs 用**指定密钥**签名一个 commit。
//
// 密钥轮换公告必须由旧密钥签名，而用户此时已经把 user.signingkey 指向新密钥，
// 所以只能显式指定。`-S<keyid>` 是 git 自带的写法。
func (r *Repo) CommitAs(ctx context.Context, tree, parent, message, key string) (string, error) {
	args := []string{"commit-tree", tree}
	if parent != "" {
		args = append(args, "-p", parent)
	}
	if key != "" {
		args = append(args, "-S"+key)
	}
	return r.commitTree(ctx, args, message)
}

func (r *Repo) commitTree(ctx context.Context, args []string, message string) (string, error) {
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
		// 只切前 8 个分隔符，正文（第 9 项）原样保留，哪怕它含 \x1f
		f := strings.SplitN(rec, "\x1f", 9)
		if len(f) != 9 {
			continue
		}
		c := RawCommit{
			OID:      strings.TrimSpace(f[0]),
			Author:   strings.TrimSpace(f[1]),
			Sig:      sigStatus(strings.TrimSpace(f[3])),
			Key:      strings.TrimSpace(f[4]),
			Seq:      strings.TrimSpace(f[5]),
			Retracts: strings.TrimSpace(f[6]),
			Reason:   strings.TrimSpace(f[7]),
			Body:     f[8],
		}
		c.Declared = trailerIn(f[8], "Immutalk-Key")
		c.Kind = trailerIn(f[8], "Immutalk-Kind")
		if secs, err := strconv.ParseInt(strings.TrimSpace(f[2]), 10, 64); err == nil {
			c.At = time.Unix(secs, 0).UTC()
		}
		if c.OID != "" {
			commits = append(commits, c)
		}
	}
	return commits
}

// trailerIn 在正文的最后一个段落里找 key 的值。找不到返回空串。
// 用的是 git 自己的 trailer 规则：trailer 必须落在最后一段。
func trailerIn(body, key string) string {
	s := strings.TrimRight(strings.ReplaceAll(body, "\r\n", "\n"), " \t\n")
	i := strings.LastIndex(s, "\n\n")
	if i < 0 {
		return ""
	}
	for _, line := range strings.Split(s[i+2:], "\n") {
		k, v, ok := strings.Cut(line, ":")
		if ok && strings.TrimSpace(k) == key {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// LogRange 只读 since 之后（不含）到 ref 之间的提交，从旧到新。
// 同步时用它算出"这次到底新到了哪几条"，一次进程调用。
func (r *Repo) LogRange(ctx context.Context, ref, since string, limit int) ([]RawCommit, error) {
	if ref == "" {
		return nil, nil
	}
	spec := ref
	if since != "" {
		spec = since + ".." + ref
	}
	args := []string{"log", "-z", "--reverse", "--format=" + logFormat}
	if limit > 0 {
		args = append(args, "-n", strconv.Itoa(limit))
	}
	args = append(args, spec)

	out, err := r.run(ctx, nil, args...)
	if err != nil {
		var ge *Error
		if errors.As(err, &ge) && (ge.Code == 128 || ge.Code == 1) {
			return nil, nil
		}
		return nil, err
	}
	return parseLog(string(out)), nil
}

// TrailerValue 读一个提交的若干个 trailer 值（一次进程调用）。
// 顺序与 keys 一致；不存在时该位为空串。
func (r *Repo) TrailerValue(ctx context.Context, oid string, keys ...string) ([]string, error) {
	if oid == "" || len(keys) == 0 {
		return nil, nil
	}
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = "%(trailers:key=" + k + ",valueonly)"
	}
	out, err := r.run(ctx, nil, "log", "-1", "--format="+strings.Join(parts, "%x1f"), oid)
	if err != nil {
		return nil, err
	}
	fields := strings.Split(strings.TrimRight(string(out), "\n"), "\x1f")
	vals := make([]string, len(keys))
	for i := range keys {
		if i < len(fields) {
			vals[i] = strings.TrimSpace(fields[i])
		}
	}
	return vals, nil
}

// HashBlob 计算数据的 blob 对象名。<type> <len>\0<content> 的 SHA-1 —— git 自己的定义。
// write=false 时只算不存，适合周期性算摘要。
func (r *Repo) HashBlob(ctx context.Context, data []byte, write bool) (string, error) {
	args := []string{"hash-object", "--stdin"}
	if write {
		args = append(args, "-w")
	}
	out, err := r.run(ctx, data, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
