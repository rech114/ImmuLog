# 参与 ImmuLog

## 许可与 DCO

本项目采用**分层许可**，每个源文件顶部的 `SPDX-License-Identifier` 就是权威声明：

| 目录 | 许可 |
|---|---|
| `core/` | **Apache-2.0** |
| `internal/web/` · `main.go` · `web/` | **AGPL-3.0-or-later** |

提交必须签署 [Developer Certificate of Origin](DCO)（DCO 1.1）：

```bash
git commit -s            # 自动加 Signed-off-by 尾行
git rebase --signoff     # 给已有提交补上
```

CI 会检查每个提交都带 `Signed-off-by:`，缺少就拒绝。

> **注意：我们用的是 DCO 而不是 CLA。**
> 这意味着**你保留自己贡献的版权**，项目也**无法在不经你同意的情况下重新授权**。
> 这是有意的——和这个项目的去中心化立场一致。
> 如果你将来打算卖商业例外许可，就需要换成 CLA。现在选了 DCO 就不要指望那扇门还开着。

---

## 开发

```bash
go build -o immulog .          # 零第三方依赖
go vet ./... && go test ./...  # 全量测试
```

前端与端到端检查（需要 Node）：

```bash
cd tools && npm install
node smoke.mjs      # jsdom：逻辑链路 + 样式表完整性
node run.mjs        # Chromium：布局/形状/韧性/SSE/无障碍/端到端/联邦
```

## 架构约束

两条**镜像**的规则，改动时请守住：

> **后端：`core/gitx` 是全项目唯一允许出现 `os/exec` 的包。**
> 其他任何地方出现 `exec.Command` 都算设计缺陷。

> **前端：`web/app/stream.js` 是唯一允许出现 `EventSource` 的文件。**

由此派生出的四条纪律（详见 `docs/DESIGN.md` §10）：

1. 参数一律拼 `[]string`，正文一律走 stdin —— 永不拼接 shell 字符串
2. 每次 git 调用带 `context` 超时
3. **永不用 `git commit`**，只用 `commit-tree`（无 index、无锁）
4. **永不在循环里调 git** —— 一次调用取一页

## 新增 `<p>` 时务必带上 `no-margin`

Beer CSS 有一条特异性 `(0,4,1)` 的全局规则会给每个跟在兄弟元素后的 `<p>` 加
`margin-block-start: 1rem`，你的 `(0,1,1)` 规则压不住。详见 `docs/DESIGN.md` §8.9。

## 提 PR 之前

- [ ] `gofmt -l .` 无输出
- [ ] `go vet ./...` 与 `go test ./...` 通过
- [ ] 改了前端就本地跑一遍 `node smoke.mjs`
- [ ] 每个提交带 `Signed-off-by`
- [ ] 新文件带上正确的 SPDX 头
