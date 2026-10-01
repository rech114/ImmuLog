// SPDX-License-Identifier: Apache-2.0

package gitx

import "context"

// transport.go —— 网络传输。复用 git 自己的 transport（ssh / https / file / git），
// 因此本节点是 git 的**客户端**，不需要自己实现 git 协议服务端。
//
// 这就是「不要手写底层逻辑」：ssh、https、pack 协商、增量传输全部白送。

// Quarantine 是隔离区的根命名空间。
//
// **网络输入绝不直接写进可信状态。** 拉下来的引用先落这里，
// 校验通过之后才由 CAS 推进到 refs/feeds/*。
const Quarantine = "refs/quarantine/"

// FetchInto 把远端某个命名空间拉到本地隔离区。
//
// remotePattern 用通配符（如 refs/feeds/*），localPrefix 通常是
// Quarantine + <槽位名>。加 "+" 是刻意的：隔离区本来就是要被覆盖的，
// 而可信状态永远不会经这条路更新。
func (r *Repo) FetchInto(ctx context.Context, url, remotePattern, localPrefix string) error {
	refspec := "+" + remotePattern + ":" + localPrefix + "/*"
	_, err := r.run(ctx, nil, "fetch", "--no-tags", "--quiet", url, refspec)
	return err
}

// PushRef 把本地一条引用推到远端同名位置（best-effort，失败不致命）。
func (r *Repo) PushRef(ctx context.Context, url, ref string) error {
	_, err := r.run(ctx, nil, "push", "--quiet", url, ref+":"+ref)
	return err
}
