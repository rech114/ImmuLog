// SPDX-License-Identifier: Apache-2.0

package feed

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"immulog/core/gitx"
)

// ── 夹具 ──────────────────────────────────────────────────────────

// encStore 建一个启用了加密的 Store（各自独立的身份与 epoch 1）。
func encStore(t *testing.T) (*Store, *gitx.Repo, string) {
	t.Helper()
	repo, dir := node(t, "alice")
	s := storeOf(t, repo, "alice")
	if err := s.SetupEncryption(context.Background()); err != nil {
		t.Fatalf("SetupEncryption: %v", err)
	}
	return s, repo, dir
}

// actAs 把本机身份换成另一个人，并清掉所有本地 epoch 密钥 ——
// 模拟"另一台机器刚拿到这个仓库，必须靠封装才能解开"。
func actAs(t *testing.T, repoDir string, id *EncIdentity) {
	t.Helper()
	if err := SaveIdentity(repoDir, id); err != nil {
		t.Fatal(err)
	}
	matches, _ := filepath.Glob(filepath.Join(repoDir, keysDir, "epoch-*"))
	for _, m := range matches {
		if err := os.Remove(m); err != nil {
			t.Fatal(err)
		}
	}
}

// rawCommitBody 直接读 git 对象里的原始正文 —— 用来证明"密文里没有明文"。
func rawCommitBody(t *testing.T, dir, oid string) string {
	t.Helper()
	return rawGit(t, dir, "cat-file", "commit", oid)
}

// ── 建立与幂等 ────────────────────────────────────────────────────

func TestSetupEncryptionIsIdempotent(t *testing.T) {
	s, _, _ := encStore(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if err := s.SetupEncryption(ctx); err != nil {
			t.Fatal(err)
		}
	}
	epochs, err := s.Epochs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(epochs) != 1 {
		t.Fatalf("重复 Setup 不应凭空造出更多世代，得到 %d 个", len(epochs))
	}
	if epochs[0].N != 1 || epochs[0].Members != 1 || !epochs[0].Held {
		t.Fatalf("epoch 1 的形状不对：%+v", epochs[0])
	}
}

func TestBeforeSetupThereIsNoEpoch(t *testing.T) {
	repo, _ := node(t, "alice")
	s := storeOf(t, repo, "alice")
	if _, err := s.CurrentEpoch(context.Background()); !errors.Is(err, ErrNoEpoch) {
		t.Fatalf("未启用加密时应返回 ErrNoEpoch，得到 %v", err)
	}
}

// ── 加密落盘 ──────────────────────────────────────────────────────

func TestSendEncryptsBodyInGitObject(t *testing.T) {
	s, _, dir := encStore(t)
	ctx := context.Background()

	secret := "这句话不能出现在 git 对象里"
	m, err := s.Send(ctx, secret)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if m.Epoch != 1 {
		t.Fatalf("消息应带上世代号，得到 %d", m.Epoch)
	}

	// ① git 对象里必须看不到明文
	raw := rawCommitBody(t, dir, m.OID)
	if strings.Contains(raw, secret) {
		t.Fatal("明文出现在了 commit 对象里 —— 加密没生效")
	}
	// ② 但元数据仍然在：链的位置、作者、时间都还在
	for _, want := range []string{"ImmuLog-Kind: msg", "ImmuLog-Seq: 1", "ImmuLog-Epoch: 1"} {
		if !strings.Contains(raw, want) {
			t.Errorf("元数据 %q 不该被加密掉（否则链就不可验证了）", want)
		}
	}
	// ③ 本机读回来是明文
	got, _ := s.History(ctx, 1)
	if len(got) != 1 || got[0].Body != secret {
		t.Fatalf("本机应能解密：%+v", got)
	}
	if got[0].Locked {
		t.Fatal("本机持有密钥，不该是 Locked")
	}
}

func TestPlaintextHistoryStaysReadable(t *testing.T) {
	repo, _ := node(t, "alice")
	ctx := context.Background()
	s := storeOf(t, repo, "alice")

	mustSend(t, s, "加密之前的话") // 明文
	if err := s.SetupEncryption(ctx); err != nil {
		t.Fatal(err)
	}
	mustSend(t, s, "加密之后的话")

	got, _ := s.History(ctx, 10)
	if len(got) != 2 {
		t.Fatalf("应有 2 条，得到 %d", len(got))
	}
	if got[1].Body != "加密之前的话" || got[1].Epoch != 0 {
		t.Fatalf("明文历史应原样可读：%+v", got[1])
	}
	if got[0].Body != "加密之后的话" || got[0].Epoch != 1 {
		t.Fatalf("加密消息应可读：%+v", got[0])
	}
}

// ── 核心属性 ①：后来者读不到加入之前的世代 ────────────────────────

func TestLateJoinerCannotReadOldEpoch(t *testing.T) {
	s, _, dir := encStore(t)
	ctx := context.Background()

	// epoch 1：只有 alice 自己
	mustSend(t, s, "加入之前的秘密")
	before, _ := s.History(ctx, 1)
	if before[0].Body != "加入之前的秘密" {
		t.Fatal("前置条件：alice 自己应能读")
	}

	// bob 带着自己的公钥加入；alice 轮换，把 epoch 2 封装给他
	bob, err := GenerateEncIdentity()
	if err != nil {
		t.Fatal(err)
	}
	e2, err := s.RotateEpoch(ctx, []string{bob.Public()})
	if err != nil {
		t.Fatalf("RotateEpoch: %v", err)
	}
	if e2.N != 2 || e2.Members != 2 {
		t.Fatalf("epoch 2 应含 2 个收件人：%+v", e2)
	}
	mustSend(t, s, "加入之后的公开消息")

	// 换到 bob 的机器上：只有 bob 的私钥，本地没有任何 epoch 密钥
	actAs(t, dir, bob)
	bobStore := storeOf(t, s.repo, "alice")

	got, err := bobStore.History(ctx, 10)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("应有 2 条，得到 %d", len(got))
	}

	// bob 读不了 epoch 1
	if !got[1].Locked || got[1].Body != "" {
		t.Fatalf("bob 不该读到加入之前的消息：%+v", got[1])
	}
	if got[1].OID == "" || got[1].Seq != 1 {
		t.Error("读不到正文，但消息的存在与位置必须仍然可见")
	}
	// bob 读得了 epoch 2 —— 封装就是给他做的
	if got[0].Locked || got[0].Body != "加入之后的公开消息" {
		t.Fatalf("bob 应能读 epoch 2：%+v", got[0])
	}
}

// 不是收件人的人，连 epoch 记录里的封装都解不开。
func TestNonMemberCannotUnwrap(t *testing.T) {
	s, repo, dir := encStore(t)
	ctx := context.Background()
	mustSend(t, s, "只给成员看")

	carol, _ := GenerateEncIdentity()
	actAs(t, dir, carol)

	if _, err := openEpochKey(ctx, repo, 1); !errors.Is(err, ErrNotRecipient) {
		t.Fatalf("非成员应得到 ErrNotRecipient，得到 %v", err)
	}
	got, _ := storeOf(t, repo, "alice").History(ctx, 1)
	if !got[0].Locked {
		t.Fatal("非成员不该读到正文")
	}
}

// ── 核心属性 ②：丢弃密钥 = 密文仍在但无人能解 ────────────────────

func TestShredMakesBodyUnreadableButKeepsTheRecord(t *testing.T) {
	s, repo, dir := encStore(t)
	ctx := context.Background()

	m, err := s.Send(ctx, "说完就忘掉")
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := s.History(ctx, 1); got[0].Body != "说完就忘掉" {
		t.Fatal("前置条件：丢弃之前应能读")
	}

	if _, err := s.ShredEpoch(ctx, 1); err != nil {
		t.Fatalf("ShredEpoch: %v", err)
	}

	// ① 本机再也解不开
	got, _ := s.History(ctx, 10)
	if !got[0].Locked || got[0].Body != "" {
		t.Fatalf("丢弃密钥后不该还能读：%+v", got[0])
	}
	// ② 但密文仍在 git 对象里 —— 这不是删除，是"不可读"
	raw := rawCommitBody(t, dir, m.OID)
	if len(raw) < 100 {
		t.Fatal("commit 对象不该消失")
	}
	// ③ 链本身完好：见证锚仍指向链尾，消息仍在
	if v, _ := s.Verify(ctx); !v.OK {
		t.Fatalf("丢弃密钥不该破坏完整性：%+v", v)
	}
	if _, err := openEpochKey(ctx, repo, 1); err == nil {
		t.Fatal("本机不该还留着 epoch 1 的密钥")
	}
}

// 丢弃这个动作本身要留痕、可审计 —— 这是「可证明的遗忘」。
func TestShredIsAuditableOnChain(t *testing.T) {
	s, _, dir := encStore(t)
	ctx := context.Background()
	mustSend(t, s, "要被遗忘的话")

	ann, err := s.ShredEpoch(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if ann.Kind != KindShred || ann.Epoch != 1 {
		t.Fatalf("丢弃公告的形状不对：%+v", ann)
	}

	raw := rawGit(t, dir, "log", "-1", "--format=%B", ann.OID)
	for _, want := range []string{"ImmuLog-Kind: shred", "ImmuLog-Epoch: 1"} {
		if !strings.Contains(raw, want) {
			t.Errorf("公告里应有 %q，实际：%q", want, raw)
		}
	}
}

func TestShredUnknownEpochFails(t *testing.T) {
	s, _, _ := encStore(t)
	if _, err := s.ShredEpoch(context.Background(), 99); !errors.Is(err, ErrNoKey) {
		t.Fatalf("丢弃不存在的世代应返回 ErrNoKey，得到 %v", err)
	}
}

// ── 轮换链 ────────────────────────────────────────────────────────

func TestRotateChainsEpochRecords(t *testing.T) {
	s, repo, _ := encStore(t)
	ctx := context.Background()
	mustSend(t, s, "一")

	if _, err := s.RotateEpoch(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RotateEpoch(ctx, nil); err != nil {
		t.Fatal(err)
	}

	epochs, _ := s.Epochs(ctx)
	if len(epochs) != 3 {
		t.Fatalf("应有 3 个世代，得到 %d", len(epochs))
	}
	for i := 1; i < len(epochs); i++ {
		parent := rawGit(t, repo.Dir, "rev-parse", epochs[i].OID+"^")
		if parent != epochs[i-1].OID {
			t.Fatalf("世代记录应串成链：epoch %d 的前驱不是 epoch %d", epochs[i].N, epochs[i-1].N)
		}
	}

	// 新消息用最新世代
	m := mustSend(t, s, "二")
	if m.Epoch != 3 {
		t.Fatalf("新消息应使用最新世代，得到 %d", m.Epoch)
	}
	// 老世代的密钥本机仍持有（没被丢弃）
	if !HasEpochKey(repo.Dir, 1) {
		t.Error("轮换不该顺手丢掉旧密钥 —— 那会让历史全部读不了")
	}
	got, _ := s.History(ctx, 10)
	for _, msg := range got {
		if msg.Locked {
			t.Fatalf("轮换后历史仍应可读：%+v", msg)
		}
	}
}

func TestKnownRecipientsIncludesSelfAndSeenPeers(t *testing.T) {
	s, _, _ := encStore(t)
	ctx := context.Background()
	mustSend(t, s, "我说的话")

	me, err := LoadIdentity(s.repo.Dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.KnownRecipients(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != me.Public() {
		t.Fatalf("应只认出自己，得到 %v", got)
	}
}
