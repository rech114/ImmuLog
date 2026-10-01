// SPDX-License-Identifier: Apache-2.0

package gitx

import (
	"context"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// ── 夹具 ──────────────────────────────────────────────────────────

// hub 建一个充当「共享中转」的 bare 仓库 —— 多源同步里最普通的那种。
func hub(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "hub.git")
	if err := Init(context.Background(), dir); err != nil {
		t.Fatalf("Init hub: %v", err)
	}
	return dir
}

// commit 造一个带序号 trailer 的提交。
func commit(t *testing.T, r *Repo, parent, body string, seq int) string {
	t.Helper()
	msg := body + "\n\nImmuLog-Kind: msg\nImmuLog-Seq: " + strconv.Itoa(seq) + "\n"
	oid, err := r.Commit(context.Background(), tree(t, r), parent, msg, false)
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return oid
}

// push 需要 receive-pack。本机（aarch64 + proot + f2fs）的 receive-pack
// 必定失败于 "bad pack"，这是环境限制而非代码问题 —— CI 上会真正执行。
func pushRef(t *testing.T, r *Repo, url, ref string) {
	t.Helper()
	err := r.PushRef(context.Background(), url, ref)
	if err == nil {
		return
	}
	if strings.Contains(err.Error(), "bad pack") || strings.Contains(err.Error(), "unpack should have generated") {
		t.Skipf("本机 receive-pack 不可用（环境限制，CI 上会执行）：%v", err)
	}
	t.Fatalf("PushRef: %v", err)
}

// seedViaFetch 把 src 的 feed 填进 hub。
//
// 这里走 fetch 而不是 push —— 因为在上述环境限制下只有 upload-pack 可用。
// 生产代码的 push 路径由 TestPushThenFetchIntoQuarantine 在 CI 上覆盖。
func seedViaFetch(t *testing.T, hubDir, srcDir string) {
	t.Helper()
	gitIn(t, hubDir, "", "fetch", "--quiet", srcDir, "+refs/feeds/*:refs/feeds/*")
}

// ── 传输 ──────────────────────────────────────────────────────────

func TestPushThenFetchIntoQuarantine(t *testing.T) {
	ctx := context.Background()
	a, h, b := newRepo(t), hub(t), newRepo(t)

	oid := commit(t, a, "", "来自 a", 1)
	if err := a.UpdateRef(ctx, "refs/feeds/alice", oid, ""); err != nil {
		t.Fatal(err)
	}
	pushRef(t, a, h, "refs/feeds/alice")

	if err := b.FetchInto(ctx, h, "refs/feeds/*", Quarantine+"r0"); err != nil {
		t.Fatalf("FetchInto: %v", err)
	}
	got, err := b.Resolve(ctx, Quarantine+"r0/alice")
	if err != nil {
		t.Fatal(err)
	}
	if got != oid {
		t.Fatalf("隔离区里应是 %s，得到 %q", oid, got)
	}

	// 最关键的一条：fetch **绝不**写进可信命名空间
	if v, _ := b.Resolve(ctx, "refs/feeds/alice"); v != "" {
		t.Fatalf("fetch 不得写进 refs/feeds/*，却写入了 %q", v)
	}
}

// 隔离区本来就该被覆盖：对端强制改写历史时，本地可信状态仍不受影响。
func TestFetchIntoOverwritesQuarantineOnly(t *testing.T) {
	ctx := context.Background()
	a, h, b := newRepo(t), hub(t), newRepo(t)

	first := commit(t, a, "", "第一版", 1)
	if err := a.UpdateRef(ctx, "refs/feeds/alice", first, ""); err != nil {
		t.Fatal(err)
	}
	seedViaFetch(t, h, a.Dir)

	if err := b.FetchInto(ctx, h, "refs/feeds/*", Quarantine+"r0"); err != nil {
		t.Fatal(err)
	}
	if got, _ := b.Resolve(ctx, Quarantine+"r0/alice"); got != first {
		t.Fatalf("首次拉取 = %q", got)
	}

	// b 把可信状态钉在 first 上
	if err := b.UpdateRef(ctx, "refs/feeds/alice", first, ""); err != nil {
		t.Fatal(err)
	}

	// a 另起一条平行链并强推（force push 的等价操作）
	forced := commit(t, a, "", "另一条链", 2)
	if err := a.UpdateRef(ctx, "refs/feeds/alice", forced, ""); err != nil {
		t.Fatal(err)
	}
	seedViaFetch(t, h, a.Dir)

	if err := b.FetchInto(ctx, h, "refs/feeds/*", Quarantine+"r0"); err != nil {
		t.Fatal(err)
	}
	if got, _ := b.Resolve(ctx, Quarantine+"r0/alice"); got != forced {
		t.Fatalf("隔离区应被覆盖成 %s，得到 %q", forced, got)
	}
	if got, _ := b.Resolve(ctx, "refs/feeds/alice"); got != first {
		t.Fatalf("可信状态必须纹丝不动，却变成了 %q", got)
	}
}

func TestFetchFromUnreachableRemoteFailsLoudly(t *testing.T) {
	r := newRepo(t)
	err := r.FetchInto(context.Background(),
		filepath.Join(t.TempDir(), "nope.git"), "refs/feeds/*", Quarantine+"r0")
	if err == nil {
		t.Fatal("不可达的远端必须报错，不能静默")
	}
}

// ── 对象原语 ──────────────────────────────────────────────────────

func TestHashBlobMatchesGit(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()
	data := []byte("refs/feeds/a 1234567890\n")

	only, err := r.HashBlob(ctx, data, false)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.TrimSpace(gitIn(t, r.Dir, string(data), "hash-object", "--stdin"))
	if only != want {
		t.Fatalf("HashBlob = %q，git hash-object = %q", only, want)
	}
	if blobExists(t, r.Dir, only) {
		t.Fatal("write=false 时不应落盘")
	}

	if _, err := r.HashBlob(ctx, data, true); err != nil {
		t.Fatal(err)
	}
	if !blobExists(t, r.Dir, only) {
		t.Fatal("write=true 时应已落盘")
	}
}

// blobExists 直接问 git 要答案 —— Repo.Exists 是给 commit 用的。
func blobExists(t *testing.T, dir, oid string) bool {
	t.Helper()
	cmd := exec.Command("git", "-C", dir, "cat-file", "-e", oid)
	cmd.Env = append(cmd.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	return cmd.Run() == nil
}

func TestLogRangeReturnsOnlyNewOldestFirst(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()

	a := commit(t, r, "", "一", 1)
	b := commit(t, r, a, "二", 2)
	c := commit(t, r, b, "三", 3)
	if err := r.UpdateRef(ctx, "refs/feeds/x", c, ""); err != nil {
		t.Fatal(err)
	}

	got, err := r.LogRange(ctx, "refs/feeds/x", a, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("应只有 2 条新提交，得到 %d", len(got))
	}
	if got[0].OID != b || got[1].OID != c {
		t.Fatalf("应为从旧到新 [二,三]，得到 [%s,%s]", got[0].OID, got[1].OID)
	}
	if got[0].Seq != "2" || got[1].Seq != "3" {
		t.Fatalf("序号不对：%q %q", got[0].Seq, got[1].Seq)
	}

	all, _ := r.LogRange(ctx, "refs/feeds/x", "", 10)
	if len(all) != 3 {
		t.Fatalf("无 since 时应全量，得到 %d", len(all))
	}
}

func TestTrailerValueReadsMultipleKeys(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()
	msg := "正文\n\nImmuLog-Snapshot: aaaa\nImmuLog-At: 2026-01-02T03:04:05Z\n"
	oid, err := r.Commit(ctx, tree(t, r), "", msg, false)
	if err != nil {
		t.Fatal(err)
	}
	vals, err := r.TrailerValue(ctx, oid, "ImmuLog-Snapshot", "ImmuLog-At", "ImmuLog-Missing")
	if err != nil {
		t.Fatal(err)
	}
	if len(vals) != 3 {
		t.Fatalf("应返回 3 个槽位，得到 %d", len(vals))
	}
	if vals[0] != "aaaa" {
		t.Errorf("Snapshot = %q", vals[0])
	}
	if vals[1] != "2026-01-02T03:04:05Z" {
		t.Errorf("At = %q", vals[1])
	}
	if vals[2] != "" {
		t.Errorf("缺失的 key 应为空串，得到 %q", vals[2])
	}
}

// 签名状态由 git 自己判定（%G?），我们只翻译。
func TestSigStatusMapping(t *testing.T) {
	for in, want := range map[string]string{
		"G": "good", "U": "untrusted", "B": "bad", "N": "", "": "",
	} {
		if got := sigStatus(in); got != want {
			t.Errorf("sigStatus(%q) = %q，期望 %q", in, got, want)
		}
	}
}
