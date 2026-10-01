package feed

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"immutalk/internal/gitx"
)

// ── 夹具 ──────────────────────────────────────────────────────────

// node 是一台独立的节点：自己的仓库 + 自己的 feed。
func node(t *testing.T, who string) (*gitx.Repo, string) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")

	dir := filepath.Join(t.TempDir(), who+".git")
	if err := gitx.Init(context.Background(), dir); err != nil {
		t.Fatalf("Init: %v", err)
	}
	rawGit(t, dir, "config", "user.name", who)
	rawGit(t, dir, "config", "user.email", who+"@example.com")
	return gitx.Open(dir), dir
}

// hub 充当共享中转的 bare 仓库。
func hub(t *testing.T, name string) (string, Remote) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")

	dir := filepath.Join(t.TempDir(), "hub.git")
	if err := gitx.Init(context.Background(), dir); err != nil {
		t.Fatalf("Init hub: %v", err)
	}
	return dir, Remote{Name: name, URL: dir}
}

// publish 把 src 的 feed 填进 hub。
//
// 走 fetch 而不是 push：本机 sandbox 的 receive-pack 不可用（见 gitx 的传输测试）。
// 生产代码的 push 路径由 gitx 包在 CI 上覆盖；这里只关心同步与校验逻辑。
func publish(t *testing.T, hubDir, srcDir string) {
	t.Helper()
	rawGitIn(t, hubDir, "", "fetch", "--quiet", srcDir, "+refs/feeds/*:refs/feeds/*")
}

func storeOf(t *testing.T, repo *gitx.Repo, who string) *Store {
	t.Helper()
	s, err := New(context.Background(), repo, FeedID(who))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// ── 正常同步 ──────────────────────────────────────────────────────

func TestSyncAdvancesFromRemote(t *testing.T) {
	ctx := context.Background()
	aRepo, aDir := node(t, "alice")
	hubDir, remote := hub(t, "hub")
	bRepo, _ := node(t, "bob")

	a := storeOf(t, aRepo, "alice")
	mustSend(t, a, "第一句")
	mustSend(t, a, "第二句")
	publish(t, hubDir, aDir)

	res, err := Sync(ctx, bRepo, []Remote{remote}, 100)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(res.Advanced) != 2 {
		t.Fatalf("应同步到 2 条，得到 %d：%+v", len(res.Advanced), res.Advanced)
	}
	// 从旧到新，与链上顺序一致
	if res.Advanced[0].Body != "第一句" || res.Advanced[1].Body != "第二句" {
		t.Fatalf("顺序不对：%q, %q", res.Advanced[0].Body, res.Advanced[1].Body)
	}
	if res.Advanced[0].Author != "alice" {
		t.Errorf("作者应来自提交，得到 %q", res.Advanced[0].Author)
	}
	if len(res.Alarms) != 0 {
		t.Fatalf("正常同步不应有告警：%+v", res.Alarms)
	}
	if res.Reached != 1 || len(res.Peers) != 1 || !res.Peers[0].OK {
		t.Fatalf("远端状况不对：%+v", res.Peers)
	}

	// 外来 feed 也要落到可信状态，并且建立见证锚
	ref := FeedRef(FeedID("alice"))
	tip, _ := bRepo.Resolve(ctx, ref)
	if tip == "" {
		t.Fatal("外来 feed 应已落进 refs/feeds/*")
	}
	w, _ := WitnessOf(ctx, bRepo, ref)
	if w != tip {
		t.Fatalf("见证锚应推进到外来链尾：%q vs %q", w, tip)
	}
}

func TestSyncIsIdempotent(t *testing.T) {
	ctx := context.Background()
	aRepo, aDir := node(t, "alice")
	hubDir, remote := hub(t, "hub")
	bRepo, _ := node(t, "bob")

	mustSend(t, storeOf(t, aRepo, "alice"), "只有一条")
	publish(t, hubDir, aDir)

	first, _ := Sync(ctx, bRepo, []Remote{remote}, 100)
	if len(first.Advanced) != 1 {
		t.Fatalf("首次应同步 1 条，得到 %d", len(first.Advanced))
	}
	second, _ := Sync(ctx, bRepo, []Remote{remote}, 100)
	if len(second.Advanced) != 0 {
		t.Fatalf("再同步不应重复投递，得到 %d", len(second.Advanced))
	}
}

func TestSyncReportsUnreachablePeer(t *testing.T) {
	bRepo, _ := node(t, "bob")
	bad := Remote{Name: "dead", URL: filepath.Join(t.TempDir(), "nope.git")}

	res, err := Sync(context.Background(), bRepo, []Remote{bad}, 100)
	if err != nil {
		t.Fatalf("单个远端不可达不应让 Sync 失败：%v", err)
	}
	if res.Reached != 0 {
		t.Fatalf("不该有可达远端，得到 %d", res.Reached)
	}
	if len(res.Peers) != 1 || res.Peers[0].OK || res.Peers[0].Note == "" {
		t.Fatalf("应如实报告不可达：%+v", res.Peers)
	}
}

// ── 安全：外来历史同样不许被静默改写 ──────────────────────────────

func TestSyncRejectsRewrittenForeignHistory(t *testing.T) {
	ctx := context.Background()
	aRepo, aDir := node(t, "alice")
	hubDir, remote := hub(t, "hub")
	bRepo, _ := node(t, "bob")
	aRef := FeedRef(FeedID("alice"))

	a := storeOf(t, aRepo, "alice")
	mustSend(t, a, "一")
	second := mustSend(t, a, "二")
	publish(t, hubDir, aDir)

	if res, _ := Sync(ctx, bRepo, []Remote{remote}, 100); len(res.Alarms) != 0 {
		t.Fatalf("首次同步不该告警：%+v", res.Alarms)
	}
	trusted, _ := bRepo.Resolve(ctx, aRef)
	if trusted != second.OID {
		t.Fatalf("b 的可信链尾应为 %s，得到 %q", second.OID, trusted)
	}

	// alice 的仓库被外部攻击者改写：从第一条另起平行链
	parent := rawGit(t, aDir, "rev-parse", second.OID+"^")
	tree := rawGit(t, aDir, "hash-object", "-w", "-t", "tree", "--stdin")
	forged := rawGitIn(t, aDir, "被改写\n\nImmutalk-Kind: msg\nImmutalk-Seq: 2\n",
		"commit-tree", tree, "-p", parent)
	rawGit(t, aDir, "update-ref", aRef, forged)
	publish(t, hubDir, aDir)

	res, err := Sync(ctx, bRepo, []Remote{remote}, 100)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(res.Alarms) != 1 {
		t.Fatalf("应恰好一条告警，得到 %d：%+v", len(res.Alarms), res.Alarms)
	}
	if res.Alarms[0].Reason != ReasonRewrite {
		t.Fatalf("原因应为 rewrite，得到 %q", res.Alarms[0].Reason)
	}
	if res.Alarms[0].Peer != "hub" {
		t.Errorf("告警应指明来源：%+v", res.Alarms[0])
	}
	if len(res.Advanced) != 0 {
		t.Fatalf("被改写的提交不得进入时间线：%+v", res.Advanced)
	}

	// 核心断言：本地可信状态与见证锚纹丝不动
	if got, _ := bRepo.Resolve(ctx, aRef); got != trusted {
		t.Fatalf("本地副本必须保留原样，却变成了 %q", got)
	}
	if w, _ := WitnessOf(ctx, bRepo, aRef); w != trusted {
		t.Fatalf("见证锚不得被攻击者带动，却变成了 %q", w)
	}
	if !res.Peers[0].OK == false && res.Peers[0].Note == "" {
		t.Errorf("远端状况应标为异常：%+v", res.Peers[0])
	}
}

// liar 造一个「持有另一条平行历史」的仓库。
//
// 用 clone 复制一份，再改写链尾 —— 这正是 force push 的等价物。
// 注意 clone **不会**复制 user.* 配置，所以身份要显式补上。
func liar(t *testing.T, srcDir, ref, parent string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "liar.git")
	rawGitIn(t, srcDir, "", "clone", "--bare", "--quiet", srcDir, dir)
	rawGit(t, dir, "config", "user.name", "alice")
	rawGit(t, dir, "config", "user.email", "alice@example.com")

	tree := rawGit(t, dir, "hash-object", "-w", "-t", "tree", "--stdin")
	forged := rawGitIn(t, dir, "另一条历史\n\nImmutalk-Kind: msg\nImmutalk-Seq: 2\n",
		"commit-tree", tree, "-p", parent)
	rawGit(t, dir, "update-ref", ref, forged)
	return dir
}

// 两个远端对同一条 feed 给出互不构成祖先关系的链尾 ⇒ 分裂视图。
func TestSyncDetectsSplitView(t *testing.T) {
	ctx := context.Background()
	aRepo, aDir := node(t, "alice")
	h1Dir, r1 := hub(t, "h1")
	h2Dir, r2 := hub(t, "h2")
	bRepo, _ := node(t, "bob")
	aRef := FeedRef(FeedID("alice"))

	a := storeOf(t, aRepo, "alice")
	first := mustSend(t, a, "共同起点")
	honest := mustSend(t, a, "诚实的历史")
	liarDir := liar(t, aDir, aRef, first.OID)

	publish(t, h1Dir, aDir)    // h1 说：链尾是 honest
	publish(t, h2Dir, liarDir) // h2 说：链尾是 forged

	res, err := Sync(ctx, bRepo, []Remote{r1, r2}, 100)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(res.Alarms) != 1 || res.Alarms[0].Reason != ReasonSplit {
		t.Fatalf("应报分裂视图，得到 %+v", res.Alarms)
	}
	if res.Reached != 2 {
		t.Errorf("两个远端都应可达：%+v", res.Peers)
	}
	_ = honest
}

// 告警里必须带上两个互不相同的锚点，否则用户无法取证。
func TestSplitAlarmCarriesBothAnchors(t *testing.T) {
	ctx := context.Background()
	aRepo, aDir := node(t, "alice")
	h1Dir, r1 := hub(t, "h1")
	h2Dir, r2 := hub(t, "h2")
	bRepo, _ := node(t, "bob")
	aRef := FeedRef(FeedID("alice"))

	a := storeOf(t, aRepo, "alice")
	first := mustSend(t, a, "起点")
	mustSend(t, a, "诚实")
	liarDir := liar(t, aDir, aRef, first.OID)

	publish(t, h1Dir, aDir)
	publish(t, h2Dir, liarDir)

	res, _ := Sync(ctx, bRepo, []Remote{r1, r2}, 100)
	if len(res.Alarms) != 1 {
		t.Fatalf("应有一条告警：%+v", res.Alarms)
	}
	v := res.Alarms[0]
	if v.Witness == "" || v.Current == "" || v.Witness == v.Current {
		t.Fatalf("告警应带上两个不同的锚点：%+v", v)
	}
	if !strings.HasPrefix(v.Feed, FeedPrefix) {
		t.Errorf("告警应指明是哪条 feed：%+v", v)
	}
}
