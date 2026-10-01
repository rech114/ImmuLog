package feed

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"immutalk/internal/gitx"
)

// ── 夹具：现场生成 SSH 密钥，不依赖开发机环境 ─────────────────────

// signRepo 建一个配好 SSH 签名的仓库，返回仓库与密钥路径。
func signRepo(t *testing.T) (*gitx.Repo, *Store, string) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")

	base := t.TempDir()
	key := filepath.Join(base, "id_ed25519")
	sshKeygen(t, key)

	// allowed_signers 让 git 能判定"这个签名有效且属于谁"
	allowed := filepath.Join(base, "allowed_signers")
	pub, err := os.ReadFile(key + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(allowed,
		[]byte("alice@example.com "+strings.TrimSpace(string(pub))+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	dir := filepath.Join(base, "repo.git")
	if err := gitx.Init(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	rawGit(t, dir, "config", "user.name", "alice")
	rawGit(t, dir, "config", "user.email", "alice@example.com")
	rawGit(t, dir, "config", "gpg.format", "ssh")
	rawGit(t, dir, "config", "user.signingkey", key+".pub")
	rawGit(t, dir, "config", "gpg.ssh.allowedSignersFile", allowed)

	repo := gitx.Open(dir)
	s, err := New(context.Background(), repo, FeedID("alice"))
	if err != nil {
		t.Fatal(err)
	}
	return repo, s, key
}

func sshKeygen(t *testing.T, path string) {
	t.Helper()
	cmd := exec.Command("ssh-keygen", "-t", "ed25519", "-N", "", "-C", "alice@example.com", "-f", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v\n%s", err, out)
	}
}

// ── 签名自检 ──────────────────────────────────────────────────────

func TestProbeSigningReturnsFingerprint(t *testing.T) {
	repo, _, _ := signRepo(t)
	fp, err := ProbeSigning(context.Background(), repo)
	if err != nil {
		t.Fatalf("自检应通过：%v", err)
	}
	if !strings.HasPrefix(fp, "SHA256:") {
		t.Fatalf("应返回密钥指纹，得到 %q", fp)
	}
}

func TestProbeSigningWithoutKey(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	dir := filepath.Join(t.TempDir(), "r.git")
	if err := gitx.Init(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	if _, err := ProbeSigning(context.Background(), gitx.Open(dir)); !errors.Is(err, ErrNoSigningKey) {
		t.Fatalf("未配置密钥时应返回 ErrNoSigningKey，得到 %v", err)
	}
}

// 配了密钥但签不出来 —— 必须报错，绝不静默降级成明文。
func TestProbeSigningWithBrokenKeyFailsLoudly(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	dir := filepath.Join(t.TempDir(), "r.git")
	if err := gitx.Init(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	rawGit(t, dir, "config", "user.name", "alice")
	rawGit(t, dir, "config", "user.email", "alice@example.com")
	rawGit(t, dir, "config", "gpg.format", "ssh")
	rawGit(t, dir, "config", "user.signingkey", "/nonexistent/key.pub")

	if _, err := ProbeSigning(context.Background(), gitx.Open(dir)); !errors.Is(err, ErrSigningBroken) {
		t.Fatalf("密钥不可用时应返回 ErrSigningBroken，得到 %v", err)
	}
}

// ── 签名真的落到了消息上 ──────────────────────────────────────────

func TestSignedMessagesCarryVerifiableIdentity(t *testing.T) {
	_, s, _ := signRepo(t)
	if !s.Signed() {
		t.Fatal("配了密钥时 Signed() 应为 true")
	}

	ctx := context.Background()
	if _, err := s.Send(ctx, "签名过的消息"); err != nil {
		t.Fatalf("Send: %v", err)
	}

	got, err := s.History(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("应有 1 条，得到 %d", len(got))
	}
	if got[0].Sig != "签名有效" {
		t.Fatalf("git 应判定签名有效，得到 %q", got[0].Sig)
	}
	if got[0].Key == "" {
		t.Fatal("应带上签名密钥指纹")
	}

	// 与 git 自己读出来的一致
	raw, _ := s.repo.Log(ctx, s.FeedRef(), 1)
	if raw[0].Sig != "good" {
		t.Fatalf("git 的签名状态应为 good，得到 %q", raw[0].Sig)
	}
	if !strings.HasSuffix(raw[0].Key, strings.TrimPrefix(got[0].Key, "")) {
		t.Fatalf("指纹不一致：%q vs %q", raw[0].Key, got[0].Key)
	}
}

// 未签名时不得谎称签过。
func TestUnsignedMessagesReportNoSignature(t *testing.T) {
	s, _ := newStore(t)
	mustSend(t, s, "没签名的消息")
	got, _ := s.History(context.Background(), 1)
	if got[0].Sig != "" || got[0].Key != "" {
		t.Fatalf("未签名时不该有签名信息：%+v", got[0])
	}
}

// ── 密钥链条 ──────────────────────────────────────────────────────

// 纯函数：直接构造 RawCommit，不需要 git。
func TestCheckKeyChainRejectsUnexplainedChange(t *testing.T) {
	prev := gitx.RawCommit{OID: "a", Key: "SHA256:old"}
	newer := []gitx.RawCommit{{OID: "b", Key: "SHA256:attacker"}}

	v, err := CheckKeyChain(prev, newer)
	if err != nil {
		t.Fatal(err)
	}
	if v.OK {
		t.Fatal("没有轮换公告背书的密钥变更必须被判为不合法")
	}
	if v.Reason != ReasonKeyChanged {
		t.Fatalf("原因应为 %q，得到 %q", ReasonKeyChanged, v.Reason)
	}
	if v.Witness != "SHA256:old" || v.Current != "SHA256:attacker" {
		t.Fatalf("告警应带上新旧密钥：%+v", v)
	}
}

func TestCheckKeyChainAllowsDeclaredRotation(t *testing.T) {
	prev := gitx.RawCommit{OID: "a", Key: "SHA256:old"}
	newer := []gitx.RawCommit{
		// 公告：仍由旧密钥签（Key 不变），但声明了新密钥
		{OID: "b", Key: "SHA256:old", Declared: "SHA256:new"},
		{OID: "c", Key: "SHA256:new"},
	}
	v, err := CheckKeyChain(prev, newer)
	if err != nil {
		t.Fatal(err)
	}
	if !v.OK {
		t.Fatalf("有旧密钥签名的轮换公告时应通过：%+v", v)
	}
}

// 公告声明了一套、实际用了另一套 —— 同样不合法。
func TestCheckKeyChainRejectsMismatchedDeclaration(t *testing.T) {
	prev := gitx.RawCommit{OID: "a", Key: "SHA256:old"}
	newer := []gitx.RawCommit{
		{OID: "b", Key: "SHA256:old", Declared: "SHA256:new"},
		{OID: "c", Key: "SHA256:other"}, // 用了第三个密钥
	}
	v, _ := CheckKeyChain(prev, newer)
	if v.OK || v.Reason != ReasonKeyChanged {
		t.Fatalf("声明的密钥与实际使用的不符，应判为不合法：%+v", v)
	}
}

// 密钥不变时链条自然连续。
func TestCheckKeyChainAcceptsStableKey(t *testing.T) {
	prev := gitx.RawCommit{OID: "a", Key: "SHA256:k"}
	newer := []gitx.RawCommit{{OID: "b", Key: "SHA256:k"}, {OID: "c", Key: "SHA256:k"}}
	if v, _ := CheckKeyChain(prev, newer); !v.OK {
		t.Fatalf("密钥不变应通过：%+v", v)
	}
}

// 未签名的历史无法被任何东西背书：首次出现的密钥被接受，但要如实标注。
func TestCheckKeyChainAcceptsFirstKeyOnUnsignedHistory(t *testing.T) {
	prev := gitx.RawCommit{OID: "a"} // 未签名
	newer := []gitx.RawCommit{{OID: "b", Key: "SHA256:first"}}
	if v, _ := CheckKeyChain(prev, newer); !v.OK {
		t.Fatalf("未签名前缀之后的首次签名应被接受：%+v", v)
	}
}

// ── 轮换公告 ──────────────────────────────────────────────────────

func TestDeclareKeyChainsAndDeclares(t *testing.T) {
	_, s, _ := signRepo(t)
	ctx := context.Background()

	mustSend(t, s, "旧密钥下的消息")
	before, _ := s.CurrentKey(ctx)
	if before == "" {
		t.Fatal("前置条件：应先有签名密钥")
	}

	ann, err := s.DeclareKey(ctx, "SHA256:brand-new-key")
	if err != nil {
		t.Fatalf("DeclareKey: %v", err)
	}
	if ann.Kind != KindRotate {
		t.Fatalf("应是轮换公告：%+v", ann)
	}

	raw, _ := s.repo.Log(ctx, s.FeedRef(), 1)
	if raw[0].Declared != "SHA256:brand-new-key" {
		t.Fatalf("公告应声明新密钥，trailer = %q", raw[0].Declared)
	}
	// 公告本身由**旧**密钥签名
	if raw[0].Key != before {
		t.Fatalf("公告应由旧密钥签名：%q vs %q", raw[0].Key, before)
	}

	// 链条校验：公告 + 之后用新密钥的提交 —— 合法
	newer := []gitx.RawCommit{
		raw[0],
		{OID: "next", Key: "SHA256:brand-new-key"},
	}
	if v, _ := CheckKeyChain(gitx.RawCommit{OID: "prev", Key: before}, newer); !v.OK {
		t.Fatalf("合法轮换应通过链条校验：%+v", v)
	}
}

func TestDeclareKeyRequiresSigning(t *testing.T) {
	s, _ := newStore(t) // 没配密钥
	mustSend(t, s, "占位")
	if _, err := s.DeclareKey(context.Background(), "SHA256:x"); !errors.Is(err, ErrNoSigningKey) {
		t.Fatalf("未配置密钥时应拒绝轮换，得到 %v", err)
	}
}

func TestDeclareKeyRejectsSameKey(t *testing.T) {
	_, s, _ := signRepo(t)
	ctx := context.Background()
	mustSend(t, s, "起个头")
	cur, _ := s.CurrentKey(ctx)

	if _, err := s.DeclareKey(ctx, cur); err == nil {
		t.Fatal("新旧密钥相同应被拒")
	}
	if _, err := s.DeclareKey(ctx, "  "); err == nil {
		t.Fatal("空的新密钥应被拒")
	}
}

// 轮换公告是结构性事件，不该出现在时间线里。
func TestRotateIsNotATimelineEvent(t *testing.T) {
	_, s, _ := signRepo(t)
	ctx := context.Background()
	mustSend(t, s, "一条消息")
	if _, err := s.DeclareKey(ctx, "SHA256:next-key"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.History(ctx, 10)
	if len(got) != 2 {
		t.Fatalf("链上应有 2 条，得到 %d", len(got))
	}
	if got[0].Kind != KindRotate {
		t.Fatalf("链尾应是轮换公告：%+v", got[0])
	}
}
