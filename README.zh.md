# ImmuLog

**一个基于 Git 的聊天系统，用可验证的历史记录保存消息。**

[![CI](https://github.com/rech114/ImmuLog/actions/workflows/ci.yml/badge.svg)](https://github.com/rech114/ImmuLog/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/go-1.24-00ADD8.svg)](go.mod)
[![Go dependencies](https://img.shields.io/badge/Go%20dependencies-0-brightgreen.svg)](go.mod)

Read this in [English](README.md).

---

ImmuLog 把每条消息存成一个 Git commit。每个节点都保留一份本地历史，因此中转节点仍然可以拒绝服务或停止同步，但已经保存过旧历史的节点可以检测到后续的历史改写。

Web 界面把这套历史呈现成聊天记录。消息可以使用 Git SSH 签名，可以通过普通 Git remote 在多个节点之间同步，也可以比较不同节点看到的历史是否发生冲突。

## 功能

- 基于 Git 的追加式消息 feed
- 使用 Git SSH 签名认证消息
- 本地 witness anchor，用于检测历史改写
- 多源同步：先进入隔离区，再校验，只允许 fast-forward
- Snapshot gossip（快照流言）：与对端比对**整体视图**，能发现 relay 从未告诉你的 feed
- 多节点之间的 split-view 检测
- 带 token 门禁的 HTTP 服务：cookie 解锁页 + 一批严格的安全响应头
- 可选的外部锚定服务
- 可选的消息体加密，使用按 epoch 划分的密钥
- 撤回通过追加事件实现，不直接删除原始对象
- 前端嵌入二进制，部署时无需 Node.js
- Go 模块没有第三方依赖，前端运行时也不访问外部网络

## 快速开始

需要 **Go 1.24+**，以及 `PATH` 中可用的 **`git` 命令**。

```bash
git clone https://github.com/rech114/ImmuLog.git
cd ImmuLog
go build -o immulog .
```

ImmuLog 从本机 Git 配置读取身份：

```bash
git config --global user.name  "你的名字"
git config --global user.email "you@example.com"
```

启动：

```bash
./immulog
```

默认监听 http://localhost:8081，Git 仓库默认使用 `./repoDB`。

前端会直接嵌入二进制，运行节点不需要安装 Node.js。

## 访问

HTTP 服务默认是关闭的。首次启动时节点会生成一个 token，并且只打印一次：

```
INFO generated an HTTP token for this run token=3f2a...
INFO open this once to store it as a cookie url=http://localhost:8081/?token=3f2a...
```

打开那个 URL 一次即可：token 会被存进 `HttpOnly` cookie，并且立刻从地址栏重定向掉。把 `IMMULOG_TOKEN` 固定下来可以让它在重启后保持稳定。

token 是**服务凭证，不是身份**。真实性来自 commit 签名：偷到 token 的人可以读这个节点、并且只能发送**他自己签名**的消息，无法冒充任何人。见 [DESIGN.md §7.9](docs/DESIGN.md)。

`GET /api/health` 不需要 token 就会应答（只回 `{"ok":true}`），这样存活探针能用；其余全部接口（包括 `/api/snapshot`）都需要。

`IMMULOG_OPEN=1` 可以在没有 token 的情况下运行。节点会在启动日志里警告，和它其它缺失的保护并列在一起。

**TLS 没有内置。** 请在最前面的反向代理上终止；TLS 和 token 各自保护什么、不保护什么，见 [DESIGN.md §7.11](docs/DESIGN.md)，两者都覆盖不到的那部分见 §7.12。

## 签名

签名不是强制的，但没有签名密钥时，Git 的 `author` 字段并不能证明身份。

使用 SSH 密钥签名：

```bash
git config --global gpg.format ssh
git config --global user.signingkey ~/.ssh/id_ed25519.pub

mkdir -p ~/.config/immulog
printf '%s %s\n' "you@example.com" "$(cat ~/.ssh/id_ed25519.pub)" \
  > ~/.config/immulog/allowed_signers

git config --global gpg.ssh.allowedSignersFile ~/.config/immulog/allowed_signers
```

如果配置了签名密钥，但 Git 实际无法使用它，ImmuLog 会拒绝启动，而不是继续创建未签名消息。

没有配置签名密钥时，启动日志和 Integrity 页面都会明确提示身份可能被冒名。

## 密钥轮换

密钥更换必须由旧密钥签名的轮换公告完成。

```bash
ssh-keygen -t ed25519 -C you@example.com -f ~/.ssh/new_key
ssh-keygen -lf ~/.ssh/new_key.pub

curl -XPOST localhost:8081/api/rotate \
  -d '{"key":"SHA256:..."}'

git config --global user.signingkey ~/.ssh/new_key.pub
```

轮换公告把旧密钥和新密钥连接起来。没有有效轮换公告的密钥变化会在同步检查中被拒绝。

## 多节点

任何支持 Git fetch/push 的仓库都可以作为中转。

例如使用本地 bare repository：

```bash
git init --bare /srv/chat.git

IMMULOG_REPO=./a \
IMMULOG_REMOTES=hub=/srv/chat.git \
PORT=8081 \
./immulog

IMMULOG_REPO=./b \
IMMULOG_REMOTES=hub=/srv/chat.git \
PORT=8082 \
./immulog
```

每个节点都会发布自己的 feed，并从配置的 remote 拉取其他 feed。

网络数据首先进入隔离区 ref。只有通过本地检查，并且能够以 fast-forward 方式推进的历史，才会进入正常 feed。

当两个 remote 为同一个 feed 提供互相冲突的历史时，ImmuLog 会报告 split view（分裂视图），不会静默地任选其中一个。

上述比对是**逐条 feed** 进行的，因此看不见"某个源干脆不告诉你某条 feed"这种情况。gossip 补上这个缺口：把 `IMMULOG_PEERS` 指向几个节点，每隔一个间隔就会与它们比对各自的整体视图。

```bash
IMMULOG_PEERS="alice=http://10.0.0.7:8082,carol=http://10.0.0.9:8082" ./immulog
```

一次视图比对只需一个请求，因此 gossip 能覆盖到你并不从中拉取的节点。它是**只读的**：对端声称的任何内容都不会被写进 `refs/feeds/*`。只有对端能看到、本机没有的 feed 会列在 Peers 面板里，而不会被当作告警。另外，少于三个对端时无法判断分歧是哪一方的问题，启动日志会说明这一点。

对端如果开了 token 门禁，需要在 URL 里带上那个节点的 token —— `IMMULOG_PEERS="carol=http://10.0.0.9:8083/?token=..."`；节点会在请求发出前把它移到 `Authorization` 头里，因此它不会落到对端的访问日志中。

## 消息体加密

默认关闭。

```bash
IMMULOG_ENCRYPT=1 ./immulog
```

启用后，消息正文会在写入 Git object 前进行加密。epoch 密钥与 Git 仓库分开保存，并使用收件人的加密公钥分别进行封装。

| 数据 | 保存位置 |
|------|----------|
| 消息正文 | 加密后的 Git object |
| epoch 密钥 | 本机密钥存储 |
| 收件人密钥封装 | Git 元数据 |
| 作者、时间、序号、OID | 公开元数据 |

每个 epoch 使用独立密钥。新成员只能获得加入之后创建的 epoch 的访问能力，后续 epoch 的密钥泄露也不会直接得到之前 epoch 的密钥。

## 丢弃密钥

可以丢弃某个 epoch 的密钥：

```bash
curl -XPOST localhost:8081/api/shred \
  -d '{"epoch":1}'
```

加密对象及其 Git 历史仍然存在。本机会删除对应密钥，同时在历史中记录这次操作。

这提供的是 crypto-shredding（密码学销毁），不是物理删除。

## 加密边界

当前实现不提供匿名性，也不会隐藏 Git 元数据。

另外，ImmuLog 节点会在把消息发送给浏览器前解密正文。因此它与传统意义上的“客户端到客户端端到端加密聊天”不是同一种模型：运行节点本身能够读取该节点负责解密的消息。

丢弃密钥也无法删除其他参与者已经解密或保存的副本。

完整的威胁模型和限制见 [DESIGN.md §6.5](docs/DESIGN.md)。

## 工作方式

一个 feed 本质上是一条 Git commit 链：

```
refs/feeds/<feed>

A
│
B
│
C
```

每条消息对应一个 commit，feed ref 指向当前链尾。

由于 ref 本身可以被移动，每个节点还会记录自己最后接受过的 feed tip：

```
refs/witness/<feed>
```

witness 是本地状态，不会被 push 或 fetch。

正常更新就是继续向前：

```
A → B → C → D
```

如果历史被重写或回滚，就可能出现不再属于 witness 后代的 tip：

```
A → B → C
       \
        X
```

本地检查随后可以报告这次不一致，而不需要相信远端对于历史的描述。

### 写入路径

```
浏览器
   │
   │ POST
   ▼
internal/web
   │
   ▼
core/feed
   │
   ├── 构造消息
   ├── 签名 / 加密
   ├── 创建 Git commit
   └── CAS 更新 feed
   │
   ▼
core/gitx
   │
   ▼
git
```

### 同步路径

```
remote
   │
   ▼
refs/quarantine/*
   │
   ├── 与本地 witness 比较
   ├── 与其他 remote 比较
   └── 检查密钥链
   │
   ▼
只允许 fast-forward
   │
   ▼
refs/feeds/*
```

核心约束如下：

| 规则 | 目的 |
|------|------|
| `core/gitx` 是唯一使用 `os/exec` 的包 | 把 Git 子进程处理集中在一个边界 |
| 网络 ref 先进入 quarantine | 远端输入不会直接写入可信状态 |
| feed 更新只允许 fast-forward | 历史改写不会静默替换本地历史 |
| witness ref 只保存在本机 | 远端无法改写检测器 |
| 撤回使用追加事件 | 撤回消息不会修改过去的历史 |
| commit metadata 使用 Git trailers | 可读文本和机器可解析元数据共存于同一 object |

## Git 自带的能力

完整的完整性模型主要建立在 Git 本身提供的对象和 ref 操作上。

不需要 worktree 或 index 就可以创建 commit：

```bash
git commit-tree <tree> -p <parent>
```

带旧值比较的 ref 更新：

```bash
git update-ref refs/feeds/alice <new> <old>
```

检查历史是否仍然是某个已知节点的后代：

```bash
git merge-base --is-ancestor <witness> <tip>
```

一次读取所有 feed tip：

```bash
git for-each-ref \
  --format='%(refname)%1f%(objectname)' \
  refs/feeds/
```

ImmuLog 没有重新实现 Git object storage 或 Git transport，而是把这些操作集中封装在 `core/gitx` 中。

## 配置

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `IMMULOG_REPO` | `./repoDB` | 本地 bare Git 仓库 |
| `PORT` | `8081` | HTTP 监听端口 |
| `IMMULOG_TOKEN` | 每次运行随机生成 | HTTP 服务的 token；固定它可以让重启后保持稳定 |
| `IMMULOG_OPEN` | 未设置 | 设为 `1` 可在无 token 下运行；节点会在启动日志里警告 |
| `IMMULOG_REMOTES` | 未设置 | Git 同步源，支持 `url` 或 `name=url`，多个源用逗号分隔 |
| `IMMULOG_ANCHOR_URL` | 未设置 | 可选的外部锚定服务 |
| `IMMULOG_SYNC_INTERVAL` | `5s` | 同步间隔 |
| `IMMULOG_PEERS` | 未设置 | gossip 对端，支持 `url` 或 `name=url`，多个用逗号分隔；URL 是 HTTP 地址，如 `http://host:8082` |
| `IMMULOG_GOSSIP_INTERVAL` | `5s` | 快照 gossip 间隔 |
| `IMMULOG_ANCHOR_INTERVAL` | `60s` | 外部锚定间隔 |
| `IMMULOG_ENCRYPT` | 未设置 | 设置为 `1` 开启消息体加密 |

未配置的可选安全能力会在启动时明确报告。

例如：

```
WARN no IMMULOG_REMOTES: single-node mode, no peer sync
WARN no IMMULOG_ANCHOR_URL: anchors stay local, not truly external
WARN no user.signingkey: messages are unsigned, identity can be impersonated
```

## HTTP API

| 方法 | 路径 | 作用 |
|------|------|------|
| `GET` | `/` | 内嵌前端 |
| `GET` | `/api/stream` | SSE 事件流 |
| `POST` | `/api/commit` | 追加消息或事件 |
| `POST` | `/api/epoch` | 创建新的加密 epoch |
| `POST` | `/api/shred` | 丢弃某个 epoch 密钥 |
| `POST` | `/api/rotate` | 发布签名密钥轮换公告 |
| `GET` | `/api/snapshot` | 当前节点看到的所有 feed |
| `GET` | `/api/health` | 存活状态与身份信息 |

事件流使用 commit OID 作为 SSE event ID。浏览器重连时会发送 `Last-Event-ID`，从对应位置继续接收事件。

## 项目结构

```
immulog/
├── go.mod
├── main.go
├── docs/
│   └── DESIGN.md
├── core/
│   ├── gitx/
│   │   ├── exec.go
│   │   ├── object.go
│   │   ├── ref.go
│   │   └── transport.go
│   └── feed/
│       ├── feed.go
│       ├── key.go
│       ├── verify.go
│       ├── snapshot.go
│       ├── sync.go
│       ├── gossip.go
│       ├── anchor.go
│       ├── crypto.go
│       ├── epoch.go
│       └── keyring.go
├── internal/
│   └── web/
│       ├── http.go
│       ├── auth.go
│       ├── headers.go
│       ├── sse.go
│       └── guard.go
├── web/
│   ├── index.html
│   ├── unlock.html
│   ├── style.css
│   └── app/
│       ├── main.js
│       ├── stream.js
│       ├── api.js
│       ├── store.js
│       ├── render.js
│       └── mock.js
└── tools/
```

`core/` 刻意放在 Go 的 `internal/` 之外，这样外部程序可以直接导入它。

前端中，`stream.js` 是唯一访问 `EventSource` 的文件，`api.js` 是唯一调用 `fetch` 的文件，`render.js` 是唯一直接操作 DOM 的文件。

后端中，`gitx` 是唯一允许调用 `os/exec` 的包。

这些边界的完整说明见 [DESIGN.md](docs/DESIGN.md)。

## 前端

前端使用原生 HTML、CSS 和 JavaScript ES modules，没有运行时框架，也没有浏览器端构建步骤。

第三方前端资源已经 vendored 到 `web/vendor/`，并嵌入最终二进制。CI 会检查 `web/` 中的页面、CSS 和 JavaScript 是否仍引用外部 URL。

开发与 CI 检查使用：

- `jsdom`：DOM 和样式完整性检查
- Playwright：真实浏览器检查
- axe-core：可访问性检查

这些都是开发工具，不属于运行时依赖。

## 测试

Go：

```bash
go vet ./...
go test ./...
```

前端和浏览器：

```bash
cd tools
npm install

npm run smoke
npm run visual
```

CI 分为三个 job：

- Go 构建、格式检查、vet、DCO、依赖检查和 race 检测
- jsdom smoke test
- Chromium 布局、恢复能力、SSE、可访问性、联邦同步和端到端检查

截图与测量结果会作为 CI artifact 保存。

## 开发环境限制

项目当前的 aarch64/proot 开发环境存在内核与 VMA 限制，会影响 `go test -race` 以及 Git 本地 `receive-pack`。

CI 使用原生 x86_64 的 GitHub Actions runner 执行这些检查。

具体复现过程和影响范围见 [DESIGN.md](docs/DESIGN.md)。

## 文档

[设计文档](docs/DESIGN.md) 包含完整的威胁模型、数据模型、同步协议、加密设计、前端架构、测试策略、已经发现的漏洞以及已知限制。

[贡献指南](CONTRIBUTING.md) 包含开发约束和 DCO 要求。

## 贡献

项目使用 Developer Certificate of Origin (DCO)，不使用 CLA。

每个 commit 都必须包含 `Signed-off-by:`：

```bash
git commit -s
```

CI 会检查整个分支历史中的 commit 是否都满足这一要求。

提交 Pull Request 前请先阅读 [CONTRIBUTING.md](CONTRIBUTING.md)。

## 许可证

Apache License 2.0。

完整许可证文本和第三方组件声明见 [LICENSE](LICENSE) 与 [NOTICE](NOTICE)。