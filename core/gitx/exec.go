// SPDX-License-Identifier: Apache-2.0

// Package gitx 是 ImmuLog 与 git 二进制的唯一边界。
//
// 全项目只有这个包允许出现 os/exec。其他任何地方出现 exec.Command 都算设计缺陷。
// 好处：换语言 / 换 libgit2 / 加缓存层，全部只动这个包。
//
// 四条纪律（对应 docs/DESIGN.md §10）：
//  1. 参数一律 []string，正文一律走 stdin —— 永不拼接 shell 字符串
//  2. 每次调用带 context 超时 —— 卡住的子进程不能拖垮 handler
//  3. 永不用 `git commit`（要 index、要锁），只用 `commit-tree`
//  4. 永不在循环里调 git —— 一次 `git log` 取一页
package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// ErrCASFailed 表示 update-ref 的 <old> 与实际不符 —— 有人抢先或本地被改写。
// 这是篡改检测的信号源，必须原样上报，绝不能吞。
var ErrCASFailed = errors.New("cas failed")

// Error 是 git 子进程的非零退出。
type Error struct {
	Args   []string
	Code   int
	Stderr string
}

func (e *Error) Error() string {
	s := strings.TrimSpace(e.Stderr)
	if s == "" {
		s = "exit " + strconv.Itoa(e.Code)
	}
	return "git " + strings.Join(e.Args, " ") + ": " + s
}

// Repo 是一个 git 仓库句柄。零值不可用，请走 Open。
type Repo struct {
	Dir     string
	Timeout time.Duration
}

const defaultTimeout = 10 * time.Second

// Open 绑定一个已有的仓库目录（bare 或非 bare 均可）。
func Open(dir string) *Repo { return &Repo{Dir: dir, Timeout: defaultTimeout} }

// Init 在 dir 建立一个 bare 仓库。bare 是有意的：
// 无 worktree、无 index，消息提交走 commit-tree 不落任何文件。
func Init(ctx context.Context, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	r := Open(dir)
	_, err := r.run(ctx, nil, "init", "--bare", "--quiet", "-b", "main")
	return err
}

// run 是全部 git 调用的唯一入口。stdin 为 nil 时不接管道。
func (r *Repo) run(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = r.Dir
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb

	if err := cmd.Run(); err != nil {
		stderr := errb.String()
		if isCASRejection(stderr) {
			return nil, fmt.Errorf("%w: %s", ErrCASFailed, strings.TrimSpace(stderr))
		}
		var ee *exec.ExitError
		code := -1
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		}
		return nil, &Error{Args: args, Code: code, Stderr: stderr}
	}
	return out.Bytes(), nil
}

// runLines 在 run 之上按行切分，去掉尾部空行。用于 hash / ref 这类单值输出。
func (r *Repo) runLines(ctx context.Context, stdin []byte, args ...string) ([]string, error) {
	out, err := r.run(ctx, stdin, args...)
	if err != nil {
		return nil, err
	}
	text := strings.TrimRight(string(out), "\n")
	if text == "" {
		return nil, nil
	}
	return strings.Split(text, "\n"), nil
}

// isCASRejection 把 git 的措辞归一到 ErrCASFailed。
func isCASRejection(stderr string) bool {
	return strings.Contains(stderr, "cannot lock ref") ||
		strings.Contains(stderr, "reference already exists") ||
		strings.Contains(stderr, "but expected")
}

// Config 读取一项 git 配置；不存在时返回空串而不是错误。
func (r *Repo) Config(ctx context.Context, key string) (string, error) {
	lines, err := r.runLines(ctx, nil, "config", "--get", key)
	if err != nil {
		var ge *Error
		if errors.As(err, &ge) && ge.Code == 1 {
			return "", nil // key 不存在
		}
		return "", err
	}
	if len(lines) == 0 {
		return "", nil
	}
	return strings.TrimSpace(lines[0]), nil
}

// Identity 返回仓库的身份。ImmuLog 的作者身份只来自这里，永远不来自请求体。
func (r *Repo) Identity(ctx context.Context) (name, email string, err error) {
	name, err = r.Config(ctx, "user.name")
	if err != nil {
		return "", "", err
	}
	email, err = r.Config(ctx, "user.email")
	if err != nil {
		return "", "", err
	}
	if name == "" || email == "" {
		return "", "", errors.New("git 身份未配置：请设置 user.name 与 user.email")
	}
	return name, email, nil
}

// SigningKey 返回配置的签名密钥；为空表示不做 commit 签名。
func (r *Repo) SigningKey(ctx context.Context) (string, error) {
	return r.Config(ctx, "user.signingkey")
}
