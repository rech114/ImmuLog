package feed

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"immutalk/internal/gitx"
)

// ── 测试夹具 ──────────────────────────────────────────────────────

func newStore(t *testing.T) (*Store, string) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")

	dir := filepath.Join(t.TempDir(), "repo.git")
	if err := gitx.Init(context.Background(), dir); err != nil {
		t.Fatalf("Init: %v", err)
	}
	rawGit(t, dir, "config", "user.name", "alice")
	rawGit(t, dir, "config", "user.email", "alice@example.com")

	repo := gitx.Open(dir)
	s, err := New(context.Background(), repo, FeedID("alice\x00alice@example.com\x00"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, dir
}

// rawGit 模拟**外部攻击者**：绕过我们的代码直接操作仓库。
// 威胁模型里的 force push 就是这么发生的，所以测试也必须走这条路径。
func rawGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return rawGitIn(t, dir, "", args...)
}

func rawGitIn(t *testing.T, dir, stdin string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(cmd.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func mustSend(t *testing.T, s *Store, body string) Message {
	t.Helper()
	m, err := s.Send(context.Background(), body)
	if err != nil {
		t.Fatalf("Send(%q): %v", body, err)
	}
	return m
}

// ── 发送 ──────────────────────────────────────────────────────────

func TestSendIncrementsSeqFromOne(t *testing.T) {
	s, _ := newStore(t)
	for want := 1; want <= 3; want++ {
		m := mustSend(t, s, "第 "+string(rune('0'+want))+" 条")
		if m.Seq != want {
			t.Fatalf("Seq = %d，期望 %d", m.Seq, want)
		}
		if len(m.OID) != 40 {
			t.Fatalf("OID 形状不对：%q", m.OID)
		}
		if m.Author != "alice" {
			t.Fatalf("作者应取自 git 配置，得到 %q", m.Author)
		}
	}
}

// 回归护栏：每条消息的 trailer 都必须能被 git 解析出来。
// 曾经因为空正文让 commit message 以空行开头，导致 Seq 读成空串。
func TestEveryMessageHasParseableSeqTrailer(t *testing.T) {
	s, dir := newStore(t)
	mustSend(t, s, "普通")
	mustSend(t, s, "多行\n第二行")
	target := mustSend(t, s, "待撤回")

	// 各条消息的 seq 必须能被 git 原样读回：1,2,3,4
	ref := FeedRef(s.Pub())
	out := rawGit(t, dir, "log", "--format=%(trailers:key=Immutalk-Seq,valueonly)", ref)
	var seqs []string
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if v := strings.TrimSpace(l); v != "" {
			seqs = append(seqs, v)
		}
	}
	if len(seqs) != 3 {
		t.Fatalf("3 条消息都应带可解析的 Seq，实际 %v（看 out=%q）", seqs, out)
	}

	// 撤回事件同理
	if _, err := s.Retract(context.Background(), target.OID, ""); err != nil {
		t.Fatal(err)
	}
	got, _ := s.History(context.Background(), 10)
	for i, m := range got {
		if m.Seq == 0 {
			t.Fatalf("第 %d 条消息的 Seq 未解析出来：%+v", i, m)
		}
	}
}

func TestHistoryIsNewestFirstAndBodyIsClean(t *testing.T) {
	s, _ := newStore(t)
	mustSend(t, s, "第一条")
	mustSend(t, s, "第二条")

	got, err := s.History(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("应读回 2 条，得到 %d", len(got))
	}
	if got[0].Body != "第二条" || got[1].Body != "第一条" {
		t.Fatalf("顺序应为新的在前：%q, %q", got[0].Body, got[1].Body)
	}
	// trailer 块必须被剥掉，正文里不能残留元数据
	for _, m := range got {
		if strings.Contains(m.Body, "Immutalk-") {
			t.Errorf("正文残留 trailer：%q", m.Body)
		}
		if m.Kind != KindMsg {
			t.Errorf("Kind = %q，期望 msg", m.Kind)
		}
	}
}

func TestMultiLineBodyRoundTrips(t *testing.T) {
	s, _ := newStore(t)
	body := "第一行\n第二行\n\n第四行"
	mustSend(t, s, body)

	got, _ := s.History(context.Background(), 1)
	if len(got) != 1 || got[0].Body != body {
		t.Fatalf("多行正文未能原样往返：%q", got[0].Body)
	}
}

// 正文里伪造同名 trailer 不得生效 —— git 的 trailer 解析取最后一次出现。
func TestFakeTrailersInBodyAreInert(t *testing.T) {
	s, _ := newStore(t)
	m := mustSend(t, s, "正文\n\nImmutalk-Seq: 999\nImmutalk-Retracts: "+strings.Repeat("f", 40))

	got, _ := s.History(context.Background(), 1)
	if got[0].Seq != 1 {
		t.Fatalf("注入的 Seq 生效了：%d", got[0].Seq)
	}
	if got[0].Kind != KindMsg {
		t.Fatalf("注入的 Retracts 改变了 Kind：%q", got[0].Kind)
	}
	if !strings.Contains(got[0].Body, "Immutalk-Seq: 999") {
		t.Errorf("正文应原样保留（只是不生效）：%q", got[0].Body)
	}
	_ = m
}

func TestSendRejectsTooLong(t *testing.T) {
	s, _ := newStore(t)
	_, err := s.Send(context.Background(), strings.Repeat("x", MaxBody+1))
	if !errors.Is(err, ErrTooLong) {
		t.Fatalf("应返回 ErrTooLong，得到 %v", err)
	}
}

// 空正文必须被拒：否则 commit message 以空行开头，git 的 trailer 解析会失效。
func TestSendRejectsEmptyBody(t *testing.T) {
	s, _ := newStore(t)
	for _, body := range []string{"", "   ", "\n\n", "\t"} {
		if _, err := s.Send(context.Background(), body); !errors.Is(err, ErrEmpty) {
			t.Fatalf("Send(%q) 应返回 ErrEmpty，得到 %v", body, err)
		}
	}
	if n, _ := s.repo.Count(context.Background(), s.ref); n != 0 {
		t.Fatalf("被拒的消息不得落盘，链长 = %d", n)
	}
}

// ── 撤回 ──────────────────────────────────────────────────────────

// 撤回是**追加事件**：链变长，原对象仍在。
func TestRetractIsAppendNotDelete(t *testing.T) {
	s, _ := newStore(t)
	target := mustSend(t, s, "说错了的话")

	r, err := s.Retract(context.Background(), target.OID, "发错频道")
	if err != nil {
		t.Fatalf("Retract: %v", err)
	}
	if r.Kind != KindRetract || r.Retracts != target.OID {
		t.Fatalf("撤回事件形状不对：%+v", r)
	}

	got, _ := s.History(context.Background(), 10)
	if len(got) != 2 {
		t.Fatalf("链应增长到 2 条，得到 %d", len(got))
	}
	// 被撤回的原消息必须仍然读得到
	var found bool
	for _, m := range got {
		if m.OID == target.OID && m.Body == "说错了的话" {
			found = true
		}
	}
	if !found {
		t.Fatal("原消息被删掉了 —— 这违反设计铁律（撤回即追加，永不删除）")
	}
}

func TestRetractRequiresTarget(t *testing.T) {
	s, _ := newStore(t)
	if _, err := s.Retract(context.Background(), "", "x"); !errors.Is(err, ErrNoTarget) {
		t.Fatal("缺少目标时应返回 ErrNoTarget")
	}
}

// 撤回目标是请求体来的字符串，必须严格校验格式 —— 否则又是一个注入面。
func TestRetractRejectsMalformedTarget(t *testing.T) {
	s, _ := newStore(t)
	mustSend(t, s, "占位")
	for _, bad := range []string{
		"not-an-oid",
		strings.Repeat("z", 40),
		strings.Repeat("a", 39),
		strings.Repeat("a", 41),
		"aaaa\nImmutalk-Seq: 999",
	} {
		if _, err := s.Retract(context.Background(), bad, "x"); !errors.Is(err, ErrBadTarget) {
			t.Fatalf("Retract(%q) 应返回 ErrBadTarget，得到 %v", bad, err)
		}
	}
}

// 只能撤回自己 feed 里的消息 —— 撤回是作者的权利。
func TestRetractOnlyOwnMessages(t *testing.T) {
	repo, _ := node(t, "alice")
	ctx := context.Background()
	me := storeOf(t, repo, "alice")
	other := storeOf(t, repo, "bob")

	mine := mustSend(t, me, "我说的")
	theirs := mustSend(t, other, "他说的")

	if _, err := me.Retract(ctx, theirs.OID, "越权"); !errors.Is(err, ErrNotMine) {
		t.Fatalf("不得撤回别人 feed 里的消息，得到 %v", err)
	}
	// 完全不存在的对象同样拒绝
	if _, err := me.Retract(ctx, strings.Repeat("a", 40), "凭空"); !errors.Is(err, ErrNotMine) {
		t.Fatalf("不存在的目标应被拒，得到 %v", err)
	}
	// 自己的可以
	if _, err := me.Retract(ctx, mine.OID, "我的"); err != nil {
		t.Fatalf("撤回自己的消息应成功：%v", err)
	}
}

// 撤回理由来自请求体，一个换行就能伪造 trailer —— 而 git 取最后一次出现，伪造的会赢。
func TestRetractReasonCannotInjectTrailers(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	target := mustSend(t, s, "原消息")

	nasty := "ok\nImmutalk-Seq: 999\nImmutalk-Retracts: " + strings.Repeat("f", 40) + "\n"
	if _, err := s.Retract(ctx, target.OID, nasty); err != nil {
		t.Fatalf("Retract: %v", err)
	}

	got, _ := s.History(ctx, 10)
	if len(got) != 2 {
		t.Fatalf("应有 2 条，得到 %d", len(got))
	}
	ev := got[0]
	if ev.Kind != KindRetract {
		t.Fatalf("链尾应是撤回事件：%+v", ev)
	}
	if ev.Seq != 2 {
		t.Fatalf("注入生效了：Seq = %d，期望 2", ev.Seq)
	}
	if ev.Retracts != target.OID {
		t.Fatalf("注入的 Retracts 覆盖了真值：%q", ev.Retracts)
	}
	if len(ev.Retracts) != 40 {
		t.Fatalf("Retracts 形状不对：%q", ev.Retracts)
	}
}

// ── 完整性：见证锚与引用重写 ──────────────────────────────────────

func TestVerifyOKAfterNormalAppend(t *testing.T) {
	s, _ := newStore(t)
	mustSend(t, s, "a")
	mustSend(t, s, "b")

	v, err := s.Verify(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !v.OK {
		t.Fatalf("正常追加后应判定一致：%+v", v)
	}
	if v.Witness != v.Current {
		t.Fatalf("见证锚应已推进到链尾：%s vs %s", v.Witness, v.Current)
	}
}

func TestVerifyDetectsRewrite(t *testing.T) {
	s, dir := newStore(t)
	mustSend(t, s, "a")
	b := mustSend(t, s, "b")

	// 外部攻击者：从 a 的位置另起一条平行链，然后把 ref 强行指过去
	parent := rawGit(t, dir, "rev-parse", b.OID+"^")
	tree := rawGit(t, dir, "hash-object", "-w", "-t", "tree", "--stdin")
	forged := rawGitIn(t, dir, "被改写的历史\n\nImmutalk-Kind: msg\nImmutalk-Seq: 2\n",
		"commit-tree", tree, "-p", parent)
	if forged == b.OID {
		t.Fatal("前置条件：伪造的提交应是一个不同的对象")
	}
	rawGit(t, dir, "update-ref", FeedRef(s.Pub()), forged)

	v, err := s.Verify(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if v.OK {
		t.Fatal("引用被改写后必须判定为不一致 —— 这是本项目存在的全部理由")
	}
	if v.Reason != ReasonRewrite {
		t.Fatalf("原因应为 %q，得到 %q", ReasonRewrite, v.Reason)
	}
	if v.Witness != b.OID {
		t.Errorf("见证锚不应被攻击者带动：%s", v.Witness)
	}
}

func TestVerifyDetectsRollback(t *testing.T) {
	s, dir := newStore(t)
	a := mustSend(t, s, "a")
	b := mustSend(t, s, "b")

	// 外部攻击者：把链尾指回一个更早的点
	rawGit(t, dir, "update-ref", FeedRef(s.Pub()), a.OID)

	v, err := s.Verify(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if v.OK {
		t.Fatal("回滚必须被判定为不一致")
	}
	if v.Reason != ReasonRollback {
		t.Fatalf("原因应为 %q，得到 %q", ReasonRollback, v.Reason)
	}
	if v.Witness != b.OID {
		t.Errorf("见证锚应仍停留在 b：%s", v.Witness)
	}
}

// 攻击者看不到也改不了见证锚：它不在 refs/feeds 命名空间里。
func TestWitnessLivesOutsideFeedNamespace(t *testing.T) {
	s, dir := newStore(t)
	mustSend(t, s, "a")

	w, err := s.Witness(context.Background())
	if err != nil || w == "" {
		t.Fatalf("见证锚应已建立：%q %v", w, err)
	}

	wrefs := strings.Fields(rawGit(t, dir, "for-each-ref", "--format=%(refname)", "refs/witness/"))
	if len(wrefs) != 1 || wrefs[0] != "refs/witness/"+s.Pub() {
		t.Fatalf("见证锚应位于 refs/witness/%s，实际 %v", s.Pub(), wrefs)
	}

	// 关键性质：只同步 refs/feeds 的一方永远拿不到见证锚
	feeds := rawGit(t, dir, "for-each-ref", "--format=%(refname)", "refs/feeds/")
	if strings.Contains(feeds, "witness") {
		t.Fatalf("见证锚不得出现在 refs/feeds 下：%q", feeds)
	}
	if strings.TrimSpace(feeds) != FeedRef(s.Pub()) {
		t.Fatalf("refs/feeds 下应恰好只有本机 feed，实际 %q", feeds)
	}
}

// ── 串行化 ────────────────────────────────────────────────────────

func TestConcurrentSendsAreSerialized(t *testing.T) {
	s, _ := newStore(t)
	const n = 24

	var wg sync.WaitGroup
	errs := make([]error, n)
	seqs := make([]int, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			m, err := s.Send(context.Background(), "并发 "+string(rune('a'+i%26)))
			errs[i], seqs[i] = err, m.Seq
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("第 %d 个并发写入失败：%v", i, err)
		}
	}
	sort.Ints(seqs)
	for i, got := range seqs {
		if got != i+1 {
			t.Fatalf("序号应恰好是 1..%d 且不重复，得到 %v", n, seqs)
		}
	}
	if v, _ := s.Verify(context.Background()); !v.OK {
		t.Fatal("并发写入后链应仍然自洽")
	}
}

// ── 派生 ──────────────────────────────────────────────────────────

// FeedID 的输出必须是 ref 安全的 —— 身份里的任何字符都不能变成路径穿越。
func TestFeedIDIsRefSafe(t *testing.T) {
	for _, seed := range []string{
		"alice\x00a@b\x00",
		"../../etc/passwd",
		"a b\tc\nd",
		"'; rm -rf / #",
		"refs/heads/main",
		"",
	} {
		id := FeedID(seed)
		if len(id) != 16 {
			t.Fatalf("FeedID(%q) 长度应为 16，得到 %d", seed, len(id))
		}
		for _, c := range id {
			if !strings.ContainsRune("0123456789abcdef", c) {
				t.Fatalf("FeedID(%q) = %q 含非十六进制字符", seed, id)
			}
		}
	}
	// 不同身份必须得到不同 feed
	if FeedID("a") == FeedID("b") {
		t.Fatal("不同身份不应得到同一个 feed")
	}
}

// 默认不做 commit 签名；配了 user.signingkey 才开。
func TestSigningOffByDefault(t *testing.T) {
	s, _ := newStore(t)
	if s.Signed() {
		t.Fatal("未配置 user.signingkey 时不应签名")
	}
}
