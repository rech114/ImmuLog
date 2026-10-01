package feed

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// recordingPublisher 记录被交给外部服务的摘要，模拟一个外部的公证方。
type recordingPublisher struct {
	got  []string
	body string
	err  error
}

func (p *recordingPublisher) Publish(_ context.Context, digest string) (string, error) {
	p.got = append(p.got, digest)
	if p.err != nil {
		return "", p.err
	}
	return p.body, nil
}

// ── 锚定链 ────────────────────────────────────────────────────────

// 每个锚定的 parent 是上一个 —— 想改写历史里的某个锚定，必须连带改写它之后的全部。
func TestAnchorChainLinksToPrevious(t *testing.T) {
	repo, _ := node(t, "alice")
	ctx := context.Background()
	s := storeOf(t, repo, "alice")

	mustSend(t, s, "一")
	a1, err := AnchorNow(ctx, repo, nil, false)
	if err != nil {
		t.Fatalf("AnchorNow: %v", err)
	}
	if a1.Seq != 1 || a1.Prev != "" {
		t.Fatalf("首个锚定的形状不对：%+v", a1)
	}

	mustSend(t, s, "二")
	a2, err := AnchorNow(ctx, repo, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if a2.Seq != 2 || a2.Prev != a1.OID {
		t.Fatalf("第二个锚定应指向第一个：%+v", a2)
	}
	// 到 git 层面确认父子关系真的落下了
	if parent := rawGit(t, repo.Dir, "rev-parse", a2.OID+"^"); parent != a1.OID {
		t.Fatalf("git 里的 parent 应是 %s，得到 %s", a1.OID, parent)
	}
	// 状态变了，摘要必须跟着变
	if a1.Snapshot == a2.Snapshot {
		t.Fatal("链尾前进后快照摘要必须变化")
	}
}

func TestAnchorHeadReadsBackLatest(t *testing.T) {
	repo, _ := node(t, "alice")
	ctx := context.Background()
	mustSend(t, storeOf(t, repo, "alice"), "一")

	// 从未锚定过 → 零值，不是错误
	if a, err := AnchorHead(ctx, repo); err != nil || a.OID != "" {
		t.Fatalf("未锚定时应返回零值：%+v %v", a, err)
	}

	want, _ := AnchorNow(ctx, repo, nil, false)
	got, err := AnchorHead(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if got.OID != want.OID || got.Snapshot != want.Snapshot || got.Seq != 1 {
		t.Fatalf("读回的锚定不符：%+v vs %+v", got, want)
	}
	if got.At.IsZero() {
		t.Error("时间戳应能从 trailer 解析回来")
	}
}

// 快照内容本身要落进锚定提交 —— 事后任何人拿到这个对象就能复算摘要。
func TestAnchorCarriesSnapshotContent(t *testing.T) {
	repo, _ := node(t, "alice")
	ctx := context.Background()
	s := storeOf(t, repo, "alice")
	mustSend(t, s, "一")

	a, _ := AnchorNow(ctx, repo, nil, false)
	body := rawGit(t, repo.Dir, "log", "-1", "--format=%B", a.OID)

	if !strings.Contains(body, FeedRef(s.Pub())) {
		t.Fatalf("锚定正文应包含 feed 列表，实际：%q", body)
	}
	// 再算一次摘要必须与锚定时一致 —— 这就是"可复算"
	snap, _ := Capture(ctx, repo)
	if snap.Digest != a.Snapshot {
		t.Fatalf("复算摘要不一致：%q vs %q", snap.Digest, a.Snapshot)
	}
}

// ── 外部锚定 ──────────────────────────────────────────────────────

func TestAnchorPublishesExternally(t *testing.T) {
	repo, _ := node(t, "alice")
	ctx := context.Background()
	mustSend(t, storeOf(t, repo, "alice"), "一")

	pub := &recordingPublisher{body: "ots-receipt-xyz"}
	a, err := AnchorNow(ctx, repo, pub, false)
	if err != nil {
		t.Fatal(err)
	}

	if len(pub.got) != 1 {
		t.Fatalf("外部服务应被调用一次，实际 %d 次", len(pub.got))
	}
	if pub.got[0] != a.Snapshot {
		t.Fatalf("交给外部的应是快照摘要：%q vs %q", pub.got[0], a.Snapshot)
	}
	if a.External != "ots-receipt-xyz" {
		t.Fatalf("回执应被记录：%q", a.External)
	}

	// 回执必须落到链上，事后可查 —— 这才是"外部锚定"的意义
	vals, err := repo.TrailerValue(ctx, a.OID, "Immutalk-External")
	if err != nil {
		t.Fatal(err)
	}
	if vals[0] != "ots-receipt-xyz" {
		t.Fatalf("回执未落到链上：%q", vals[0])
	}
}

// 外部不可用不能阻断本地锚定 —— 本地链本身已经有价值。
func TestAnchorSurvivesPublisherFailure(t *testing.T) {
	repo, _ := node(t, "alice")
	ctx := context.Background()
	mustSend(t, storeOf(t, repo, "alice"), "一")

	pub := &recordingPublisher{err: errors.New("503 service unavailable")}
	a, err := AnchorNow(ctx, repo, pub, false)
	if err != nil {
		t.Fatalf("外部失败不应让锚定失败：%v", err)
	}
	if a.External != "" {
		t.Fatalf("失败时不应记录回执：%q", a.External)
	}
	if a.OID == "" || a.Snapshot == "" {
		t.Fatalf("本地锚定仍应成立：%+v", a)
	}
}

// 没有配置外部服务时，锚定照样工作，只是"不外部"。
func TestAnchorWithoutPublisher(t *testing.T) {
	repo, _ := node(t, "alice")
	ctx := context.Background()
	mustSend(t, storeOf(t, repo, "alice"), "一")

	a, err := AnchorNow(ctx, repo, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if a.External != "" || a.OID == "" {
		t.Fatalf("无外部服务时应得到一条本地锚定：%+v", a)
	}
}

// 回执里的控制字符不得破坏 trailer 结构。
func TestAnchorSanitizesReceipt(t *testing.T) {
	repo, _ := node(t, "alice")
	ctx := context.Background()
	mustSend(t, storeOf(t, repo, "alice"), "一")

	pub := &recordingPublisher{body: "ok\x1f\x1e\nImmutalk-Snapshot: forged"}
	a, err := AnchorNow(ctx, repo, pub, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(a.External, "\x1f\x1e") {
		t.Fatalf("回执里的控制字符应被清掉：%q", a.External)
	}
	// 摘要不得被回执里的伪造 trailer 带偏
	got, _ := AnchorHead(ctx, repo)
	if got.Snapshot != a.Snapshot {
		t.Fatalf("摘要被注入影响：%q vs %q", got.Snapshot, a.Snapshot)
	}
}
