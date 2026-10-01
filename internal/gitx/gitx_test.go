package gitx

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ── 测试夹具 ──────────────────────────────────────────────────────

// git 直接在测试里跑 git 命令。生产代码禁止 os/exec，测试不受此限。
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(cmd.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// newRepo 建一个 bare 仓库并配好身份。全程隔离，不碰开发机的 ~/.gitconfig。
func newRepo(t *testing.T) *Repo {
	t.Helper()
	// 让 Identity() 只看到仓库本地配置 —— 否则测试会读到开发机的全局身份
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")

	dir := filepath.Join(t.TempDir(), "repo.git")
	if err := Init(context.Background(), dir); err != nil {
		t.Fatalf("Init: %v", err)
	}
	git(t, dir, "config", "user.name", "tester")
	git(t, dir, "config", "user.email", "tester@example.com")
	return Open(dir)
}

func tree(t *testing.T, r *Repo) string {
	t.Helper()
	tr, err := r.EmptyTree(context.Background())
	if err != nil {
		t.Fatalf("EmptyTree: %v", err)
	}
	return tr
}

// ── 生命周期 ──────────────────────────────────────────────────────

func TestInitCreatesBareRepo(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "x.git")
	if err := Init(context.Background(), dir); err != nil {
		t.Fatalf("Init: %v", err)
	}
	// bare 仓库没有工作区：这是刻意的（消息提交不落任何文件）
	if out := git(t, dir, "rev-parse", "--is-bare-repository"); strings.TrimSpace(out) != "true" {
		t.Fatalf("应为 bare 仓库，得到 %q", out)
	}
}

func TestIdentityRequiresConfig(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")

	dir := filepath.Join(t.TempDir(), "y.git")
	if err := Init(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	// 没配 user.name/user.email 时必须报错，而不是返回空串
	if _, _, err := Open(dir).Identity(context.Background()); err == nil {
		t.Fatal("未配置身份时应报错")
	}
}

// ── 对象 ──────────────────────────────────────────────────────────

// Commit 必须能在没有 index、没有 worktree 的 bare 仓库里工作 —— 核心纪律之一。
func TestCommitTreeNeedsNoIndexNorWorktree(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()

	if out := git(t, r.Dir, "rev-parse", "--is-bare-repository"); strings.TrimSpace(out) != "true" {
		t.Fatal("前置条件：应为 bare")
	}

	oid, err := r.Commit(ctx, tree(t, r), "", "第一条\n\nImmutalk-Seq: 1\n", false)
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if !isHex40(oid) {
		t.Fatalf("应返回 40 位十六进制对象名，得到 %q", oid)
	}
	if ok, _ := r.Exists(ctx, oid); !ok {
		t.Fatal("commit 应已落盘")
	}
}

func TestLogParsesTrailersAndBody(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()

	body := "正文第一行\n正文第二行"
	oid, err := r.Commit(ctx, tree(t, r), "", body+"\n\nImmutalk-Kind: msg\nImmutalk-Seq: 7\n", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.UpdateRef(ctx, "refs/feeds/a", oid, ""); err != nil {
		t.Fatal(err)
	}

	got, err := r.Log(ctx, "refs/feeds/a", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("应读回 1 条，得到 %d", len(got))
	}
	if got[0].Seq != "7" {
		t.Errorf("Seq = %q，期望 7", got[0].Seq)
	}
	if got[0].Author != "tester" {
		t.Errorf("Author = %q", got[0].Author)
	}
	if !strings.Contains(got[0].Body, "正文第二行") {
		t.Errorf("Body 应保留多行正文，得到 %q", got[0].Body)
	}
}

// 对抗性输入：正文里塞记录分隔符 + 伪造 trailer，不得破坏解析。
func TestLogSurvivesAdversarialBody(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()

	// \x1e 是记录分隔、\x1f 是字段分隔，都是合法字节 —— 必须扛住
	evil := "a\x1eb\x1fc\n\nImmutalk-Seq: 999\n"
	oid, err := r.Commit(ctx, tree(t, r), "", evil+"\n\nImmutalk-Kind: msg\nImmutalk-Seq: 2\n", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.UpdateRef(ctx, "refs/feeds/a", oid, ""); err != nil {
		t.Fatal(err)
	}

	got, err := r.Log(ctx, "refs/feeds/a", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("正文里的分隔符不得切出多余记录：得到 %d 条", len(got))
	}
	// git 自己的 trailer 解析取最后一次出现 —— 伪造的 999 不应生效
	if got[0].Seq != "2" {
		t.Errorf("注入的 trailer 不应生效：Seq = %q，期望 2", got[0].Seq)
	}
}

// NUL 不由我们防 —— git 自己就拒绝。这里把这条保证固定下来当成回归护栏。
func TestNulByteInMessageIsRejectedByGit(t *testing.T) {
	r := newRepo(t)
	_, err := r.Commit(context.Background(), tree(t, r), "", "a\x00b\n\nImmutalk-Seq: 1\n", false)
	if err == nil {
		t.Fatal("git 应拒绝含 NUL 的 commit message")
	}
	if !strings.Contains(err.Error(), "NUL") {
		t.Errorf("错误信息应提到 NUL，得到 %v", err)
	}
}

func TestLogOnUnknownRefIsEmptyNotError(t *testing.T) {
	r := newRepo(t)
	got, err := r.Log(context.Background(), "refs/feeds/nope", 10)
	if err != nil {
		t.Fatalf("未知 ref 不应报错：%v", err)
	}
	if len(got) != 0 {
		t.Fatalf("应为空，得到 %d", len(got))
	}
}

// ── 引用与 CAS ────────────────────────────────────────────────────

// 这是整个防篡改设计的基石，逐条验证。
func TestUpdateRefCAS(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()
	tr := tree(t, r)

	mk := func(msg string) string {
		oid, err := r.Commit(ctx, tr, "", msg+"\n\nImmutalk-Seq: 1\n", false)
		if err != nil {
			t.Fatal(err)
		}
		return oid
	}
	a, b := mk("one"), mk("two")

	// ① 首次创建：old 为空 → 允许
	if err := r.UpdateRef(ctx, "refs/feeds/x", a, ""); err != nil {
		t.Fatalf("首次创建应成功：%v", err)
	}
	// ② old 正确 → 前进
	if err := r.UpdateRef(ctx, "refs/feeds/x", b, a); err != nil {
		t.Fatalf("old 正确时应成功：%v", err)
	}
	// ③ old 错误 → 必须被拒，且归一成 ErrCASFailed
	err := r.UpdateRef(ctx, "refs/feeds/x", a, a)
	if !errors.Is(err, ErrCASFailed) {
		t.Fatalf("old 不符时应返回 ErrCASFailed，得到 %v", err)
	}
	if got, _ := r.Resolve(ctx, "refs/feeds/x"); got != b {
		t.Fatalf("CAS 失败后引用不得被改动：%q", got)
	}
	// ④ 已存在时用全零 old 创建 → 拒绝
	if err := r.EnsureRef(ctx, "refs/feeds/x", a); !errors.Is(err, ErrCASFailed) {
		t.Fatalf("EnsureRef 不应覆盖已存在的引用，得到 %v", err)
	}
}

func TestResolveMissingReturnsEmpty(t *testing.T) {
	r := newRepo(t)
	got, err := r.Resolve(context.Background(), "refs/feeds/ghost")
	if err != nil {
		t.Fatalf("缺失引用不应报错：%v", err)
	}
	if got != "" {
		t.Fatalf("应为空串，得到 %q", got)
	}
}

func TestIsAncestor(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()
	tr := tree(t, r)

	root, _ := r.Commit(ctx, tr, "", "root\n\nImmutalk-Seq: 1\n", false)
	child, _ := r.Commit(ctx, tr, root, "child\n\nImmutalk-Seq: 2\n", false)
	side, _ := r.Commit(ctx, tr, root, "side\n\nImmutalk-Seq: 2\n", false)

	if ok, _ := r.IsAncestor(ctx, root, child); !ok {
		t.Error("root 应是 child 的祖先")
	}
	if ok, _ := r.IsAncestor(ctx, child, root); ok {
		t.Error("child 不可能是 root 的祖先")
	}
	// 兄弟节点之间不构成祖先关系 —— 这正是「引用重写」的判定
	if ok, _ := r.IsAncestor(ctx, child, side); ok {
		t.Error("兄弟节点之间应为 false")
	}
	if ok, _ := r.IsAncestor(ctx, "", child); ok {
		t.Error("空值应为 false")
	}
}

func TestRefsSnapshotInOneCall(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()
	tr := tree(t, r)

	for _, name := range []string{"a", "b", "c"} {
		oid, err := r.Commit(ctx, tr, "", name+"\n\nImmutalk-Seq: 1\n", false)
		if err != nil {
			t.Fatal(err)
		}
		if err := r.UpdateRef(ctx, "refs/feeds/"+name, oid, ""); err != nil {
			t.Fatal(err)
		}
	}
	// 干扰项：不在 feeds 命名空间内，必须被前缀过滤掉
	w, _ := r.Commit(ctx, tr, "", "w\n\nImmutalk-Seq: 1\n", false)
	if err := r.UpdateRef(ctx, "refs/witness/a", w, ""); err != nil {
		t.Fatal(err)
	}

	refs, err := r.Refs(ctx, "refs/feeds/")
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 3 {
		t.Fatalf("应只拿到 3 条 feed 引用，得到 %d：%+v", len(refs), refs)
	}
	for _, ref := range refs {
		if !isHex40(ref.OID) {
			t.Errorf("%s 的 OID 形状不对：%q", ref.Name, ref.OID)
		}
	}
}

func TestCount(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()
	tr := tree(t, r)

	prev := ""
	for i := 1; i <= 4; i++ {
		oid, err := r.Commit(ctx, tr, prev, "m\n\nImmutalk-Seq: 1\n", false)
		if err != nil {
			t.Fatal(err)
		}
		prev = oid
	}
	if err := r.UpdateRef(ctx, "refs/feeds/n", prev, ""); err != nil {
		t.Fatal(err)
	}
	n, err := r.Count(ctx, "refs/feeds/n")
	if err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Fatalf("Count = %d，期望 4", n)
	}
}

// ── 边界 ──────────────────────────────────────────────────────────

// 参数是数组、正文走 stdin —— 因此 shell 元字符不具备任何特殊含义。
func TestArgumentInjectionIsInert(t *testing.T) {
	r := newRepo(t)
	nasty := "'; rm -rf / # `whoami` $(id) && | ; \n"
	oid, err := r.Commit(context.Background(), tree(t, r), "", nasty+"\n\nImmutalk-Seq: 1\n", false)
	if err != nil {
		t.Fatalf("含元字符的正文应被当作纯文本：%v", err)
	}
	if !isHex40(oid) {
		t.Fatalf("对象名形状不对：%q", oid)
	}
}

func TestErrorCarriesStderrAndCommand(t *testing.T) {
	r := newRepo(t)
	_, err := r.run(context.Background(), nil, "rev-parse", "--verify", "refs/heads/definitely-not-here")
	var ge *Error
	if !errors.As(err, &ge) {
		t.Fatalf("应返回 *Error，得到 %T (%v)", err, err)
	}
	if ge.Code == 0 {
		t.Error("非零退出码应被记录")
	}
	if !strings.Contains(ge.Error(), "git rev-parse") {
		t.Errorf("错误信息应包含命令：%s", ge.Error())
	}
}

func isHex40(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}
