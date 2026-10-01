// SPDX-License-Identifier: Apache-2.0

package feed

import (
	"context"
	"strings"
	"testing"

	"immulog/core/gitx"
)

// ── 快照 ──────────────────────────────────────────────────────────

func TestCaptureIsDeterministic(t *testing.T) {
	repo, _ := node(t, "alice")
	ctx := context.Background()
	mustSend(t, storeOf(t, repo, "alice"), "一条")

	a, err := Capture(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := Capture(ctx, repo)
	if a.Digest != b.Digest {
		t.Fatalf("同一状态下摘要必须稳定：%q vs %q", a.Digest, b.Digest)
	}
	if len(a.Digest) != 40 {
		t.Fatalf("摘要应是 git 对象名，得到 %q", a.Digest)
	}
	if len(a.Refs) != 1 || a.Refs[0].OID == "" {
		t.Fatalf("快照应包含全部 feed 锚点：%+v", a.Refs)
	}
}

func TestCaptureChangesWhenFeedAdvances(t *testing.T) {
	repo, _ := node(t, "alice")
	ctx := context.Background()
	s := storeOf(t, repo, "alice")

	mustSend(t, s, "一")
	before, _ := Capture(ctx, repo)
	mustSend(t, s, "二")
	after, _ := Capture(ctx, repo)

	if before.Digest == after.Digest {
		t.Fatal("链尾前进后摘要必须变化 —— 否则快照毫无意义")
	}
}

// 空仓库也要有稳定的摘要（空文本的 blob 对象名），而不是空串。
func TestCaptureOnEmptyRepoIsStable(t *testing.T) {
	repo, _ := node(t, "empty")
	a, err := Capture(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if a.Digest == "" || len(a.Refs) != 0 {
		t.Fatalf("空仓库摘要形状不对：%+v", a)
	}
}

// 规范文本必须按 ref 名排序 —— 否则 ref 顺序一变摘要就变，跨节点比对毫无意义。
func TestSnapshotTextIsCanonicallyOrdered(t *testing.T) {
	repo, _ := node(t, "alice")
	ctx := context.Background()
	mustSend(t, storeOf(t, repo, "alice"), "一")
	mustSend(t, storeOf(t, repo, "bob"), "二")
	mustSend(t, storeOf(t, repo, "carol"), "三")

	snap, _ := Capture(ctx, repo)
	if len(snap.Refs) != 3 {
		t.Fatalf("应有 3 条 feed，得到 %d", len(snap.Refs))
	}
	for i := 1; i < len(snap.Refs); i++ {
		if snap.Refs[i-1].Name >= snap.Refs[i].Name {
			t.Fatalf("ref 必须按名升序：%v", snap.Refs)
		}
	}
	// 文本与 Refs 必须一致 —— 摘要算的就是这段文本
	for _, r := range snap.Refs {
		if !strings.Contains(snap.Text, r.Name+" "+r.OID) {
			t.Fatalf("规范文本里缺 %s：%q", r.Name, snap.Text)
		}
	}
}

// ── 一致性比对 ────────────────────────────────────────────────────

// 一方落后不是矛盾 —— 只有互不构成祖先关系才算分裂。
func TestDivergedIgnoresStaleness(t *testing.T) {
	repo, _ := node(t, "alice")
	ctx := context.Background()
	s := storeOf(t, repo, "alice")

	mustSend(t, s, "一")
	old, _ := Capture(ctx, repo)
	mustSend(t, s, "二")
	fresh, _ := Capture(ctx, repo)

	d, err := Diverged(ctx, repo, old, fresh)
	if err != nil {
		t.Fatal(err)
	}
	if len(d) != 0 {
		t.Fatalf("落后的快照不算矛盾，得到 %v", d)
	}
}

func TestDivergedSpotsParallelChains(t *testing.T) {
	repo, dir := node(t, "alice")
	ctx := context.Background()
	s := storeOf(t, repo, "alice")
	ref := FeedRef(s.Pub())

	first := mustSend(t, s, "起点")
	mustSend(t, s, "诚实")
	mine, _ := Capture(ctx, repo)

	// 攻击者另起一条平行链；把对象拉到本地（比对需要对象在本地）
	liarDir := liar(t, dir, ref, first.OID)
	rawGit(t, dir, "fetch", "--quiet", liarDir, "+refs/feeds/*:refs/quarantine/x/*")
	forged := rawGit(t, dir, "rev-parse", "refs/quarantine/x/"+s.Pub())

	theirs := Snapshot{Refs: []gitx.RefInfo{{Name: ref, OID: forged}}}
	d, err := Diverged(ctx, repo, mine, theirs)
	if err != nil {
		t.Fatal(err)
	}
	if len(d) != 1 || d[0] != ref {
		t.Fatalf("平行链必须被判为分裂，得到 %v", d)
	}
}

// 缺少对象时不能瞎判 —— 应当报错而不是默默说"一致"。
func TestDivergedErrorsWhenObjectMissing(t *testing.T) {
	repo, _ := node(t, "alice")
	ctx := context.Background()
	s := storeOf(t, repo, "alice")
	mustSend(t, s, "一")
	mine, _ := Capture(ctx, repo)

	theirs := Snapshot{Refs: []gitx.RefInfo{
		{Name: FeedRef(s.Pub()), OID: strings.Repeat("a", 40)}, // 本地不存在的对象
	}}
	if _, err := Diverged(ctx, repo, mine, theirs); err == nil {
		t.Fatal("对象缺失时应报错，不能假装比对成功")
	}
}
