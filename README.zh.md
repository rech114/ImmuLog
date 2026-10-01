# ImmuLog

把 Git 当作信任根的聊天系统。**撤回可证明，而非不可删除。**

> 攻击者可以拒绝服务、可以拖慢你，但**做不到静默地**删除、回滚或改写历史。
> 他每一次动手，都会在聊天流里长出一条你自己生成、他删不掉的**系统消息**。

完整设计见 **[docs/DESIGN.md](docs/DESIGN.md)**。

---

## 技术选型

| | |
|---|---|
| 后端 | **Go + 原版 Git CLI**（`os/exec`，不用 go-git / libgit2） |
| **第三方依赖** | **0**（`go.sum` 不存在） |
| 前端 | **Beer CSS 5.0.3**（CDN）+ 原生 ES modules，**无构建链** |
| 通信 | 上行 `POST` + 下行 **SSE** |

## 状态

| 部分 | 状态 |
|---|---|
| 设计文档 | ✅ 15 节 |
| 前端界面 | ✅ 可运行 |
| 后端 | ✅ 完成 |
| 多源同步 / 快照 / 锚定链 | ✅ 完成 |
| 签名 / 密钥轮换 | ✅ 完成 |
| **端到端加密 / 可遗忘** | ✅ 完成 |

## 跑起来

```bash
go build -o immulog .
./immulog                      # 监听 :8081，仓库默认为 ./repoDB
```

需要先在 git config 里有身份：

```bash
git config --global user.name "你的名字"
git config --global user.email "you@example.com"
```

### 签名（强烈建议）

没有签名时，`author` 字段只是装饰品——任何能推送的人都能冒名。开启方式：

```bash
git config --global gpg.format ssh
git config --global user.signingkey ~/.ssh/id_ed25519.pub
# 让 git 能判定"这个签名有效、属于谁"（否则状态只是"密钥未知"）
printf '%s %s\n' "you@example.com" "$(cat ~/.ssh/id_ed25519.pub)" > ~/.config/immulog/allowed_signers
git config --global gpg.ssh.allowedSignersFile ~/.config/immulog/allowed_signers
```

**配了密钥但签不出来时，服务会拒绝启动**，而不是悄悄发出没有签名的消息：

```
启动失败 err=user.signingkey 已配置但无法签名：签名密钥不可用: ...
  修好它，或清空该配置以明确地以「不签名」身份运行
```

未配置密钥时，启动日志与「完整性」页都会**明说"身份可被冒名"**——它不假装安全。

**换密钥**（顺序不能反）：

```bash
ssh-keygen -t ed25519 -C you@example.com -f ~/.ssh/new_key
ssh-keygen -lf ~/.ssh/new_key.pub        # 拿到指纹，形如 SHA256:...
# 1) 先发轮换公告（用当前这把旧密钥签名）
curl -XPOST localhost:8081/api/rotate -d '{"key":"SHA256:..."}'
# 2) 再把配置指过去
git config --global user.signingkey ~/.ssh/new_key.pub
```

轮换公告由**旧密钥**签名并声明新密钥；链条因此连续可审计。
任何**没有公告背书**的密钥变更都会被同步方判为攻击并保留告警。

### 环境变量

| 变量 | 默认 | 说明 |
|---|---|---|
| `IMMULOG_REPO` | `./repoDB` | 仓库目录（自动 `git init --bare`） |
| `PORT` | `8081` | 监听端口 |
| `IMMULOG_REMOTES` | 空 | 同步源，逗号分隔。`url` 或 `name=url`，走 git 自己的 transport（ssh/https/file/git） |
| `IMMULOG_ANCHOR_URL` | 空 | 外部锚定服务。收到 `POST {snapshot, at}` 后把响应体当回执记进锚定链 |
| `IMMULOG_SYNC_INTERVAL` | `5s` | 同步间隔 |
| `IMMULOG_ANCHOR_INTERVAL` | `60s` | 锚定间隔 |
| `IMMULOG_ENCRYPT` | 空 | 设为 `1` 启用端到端加密。**一旦启用就粘住**（见下） |

两个「没配」都会在启动时**明确警告**，不假装安全：

```
WARN 未配置 IMMULOG_REMOTES：单节点模式，不会与任何对端同步
WARN 未配置 IMMULOG_ANCHOR_URL：锚定只落在本机，不是真正的外部锚定
```

### 多节点示例

```bash
# 一个共享中转（任何 git 托管都行）
git init --bare /srv/chat.git

# 节点 A
IMMULOG_REPO=./a IMMULOG_REMOTES=hub=/srv/chat.git PORT=8081 ./immulog
# 节点 B
IMMULOG_REPO=./b IMMULOG_REMOTES=hub=/srv/chat.git PORT=8082 ./immulog
```

每个节点把自己的 feed 推到中转站，并从**所有**中转站拉取、逐条校验、只允许快进。
配多个中转站时，它们之间互相矛盾会被判为**分裂视图**并告警。

### 只看界面（不需要后端）

```bash
cd web && python3 -m http.server 8099
# http://127.0.0.1:8099/?demo=1
```

`?demo=1` 走 `app/mock.js`；去掉它就是真实后端。

---

## 原版 Git 白送的能力

```bash
# 无 index / 无 worktree / 无锁地造一条「消息」
echo "第二条消息" | git commit-tree <tree> -p <parent>

# 原生的「见证锚」：old 不匹配就拒绝（exit 128）→ 就是我们的 cas_failed
git update-ref refs/feeds/alice <new> <old>

# 引用重写的判定：见证锚不再是链尾的祖先
git merge-base --is-ancestor <witness> <tip>

# 一次调用拿到全部 feed 锚点（gossip 快照数据源）
git for-each-ref --format='%(refname)%1f%(objectname)' refs/feeds/
```

---

## 目录

```
immulog/
├── go.mod                  零第三方依赖（没有 go.sum）
├── main.go                 组装与生命周期
├── docs/DESIGN.md
├── core/                   ← 可被外部导入的库
│   ├── gitx/               唯一允许出现 os/exec 的包
│   │   ├── exec.go         子进程边界：stdin 注入、超时、错误归一、Init
│   │   ├── object.go       commit-tree / hash-object / log / trailer 读取
│   │   ├── ref.go          CAS 读写 / for-each-ref / is-ancestor / count
│   │   └── transport.go    fetch / push（复用 git 自己的 transport）
│   └── feed/               领域语义：把 git 仓库变成可验证的消息日志
│       ├── feed.go         消息编解码 + 发送 / 撤回 / 历史
│       ├── key.go          签名自检、密钥链条、轮换公告
│       ├── verify.go       见证锚与引用重写检测
│       ├── snapshot.go     快照摘要与分裂视图判定
│       ├── sync.go         多源同步：隔离区 → 校验 → 快进
│       └── anchor.go       锚定链与外部锚定接口
├── internal/web/           传输
│   ├── http.go             路由与处理器（net/http）
│   ├── sse.go              EventSource 流（零依赖）
│   └── guard.go            后台循环：巡检 / 同步 / 锚定
└── web/                    前端，//go:embed
    ├── index.html
    ├── style.css
    └── app/                main / stream / api / store / render / mock
```

> **架构约束：同一条原则镜像到两侧。**
> 后端 `core/gitx` 是唯一能碰 `os/exec` 的包；前端 `stream` 是唯一能碰 `EventSource` 的文件。

`core/` 刻意不在 `internal/` 下 —— Go 禁止导入 `internal/`，
放在那里会让 Apache 授权变成一纸空文。

---

## 端到端加密

**默认关闭。** 打开：

```bash
IMMULOG_ENCRYPT=1 ./immulog
```

开启后：

| | |
|---|---|
| 消息正文 | AEAD 加密后进 git 对象。**任何拿到副本的人都只能看到密文** |
| 密钥 | 按 epoch 分组，用**每个收件人的公钥各封一份** → `refs/keys/<n>` |
| 明文密钥 | **只存本机** `<repo>/immulog-keys/`，不进 git、不参与同步 |
| 元数据 | 作者、时间、序号、哈希链**保持公开**——否则链就不可验证了 |

```
$ git cat-file commit <oid>
  ... 看不到明文 ...
  ImmuLog-Kind: msg
  ImmuLog-Seq: 1
  ImmuLog-Epoch: 1
```

**一旦开启就粘住**：只要本机存在加密身份，重启时会自动继续加密。
否则忘了带环境变量，后续消息会悄悄退回明文——而话一旦说出去就收不回来了。

### 白得的两条属性

1. **后来者读不到加入之前的世代** —— 新成员只会被封装进之后的 epoch，**不依赖任何人配合**
2. **密钥泄露的影响面被限制在一个 epoch 内** —— 拿到 epoch N+1 的密钥，读不了 epoch N

### 忘记（crypto-shredding）

```bash
curl -XPOST localhost:8081/api/shred -d '{"epoch":1}'
```

密文仍在、哈希链完整、副本仍在——**但没人解得开**。而且这个动作会在链上留下
**可审计的公告**：任何人都能看到「谁在何时丢弃了哪个世代」，却看不到被保护的内容。

> **它做不到什么**：OID、作者、时间戳全都还在（**这不是匿名**）；
> 只丢本机那一份，别人手里的副本不会消失；对已经看到的人无效；
> 对闪存不构成物理擦除保证。见 `docs/DESIGN.md` §6.5。

### 相关接口

| 方法 | 路径 | 作用 |
|---|---|---|
| `POST` | `/api/epoch` | 轮换到新世代（收件人 = 近期见过的公钥 + 自己） |
| `POST` | `/api/shred` | 丢弃某个世代的密钥，并留下公告 |

---

## 许可

**Apache License 2.0** —— 见 [LICENSE](LICENSE)，第三方组件声明见 [NOTICE](NOTICE)。

整个仓库（`core/` 库 + 服务端 + 前端）统一使用 Apache-2.0。

`core/` 刻意不在 `internal/` 下 —— Go 禁止导入 `internal/`。
一个可复用的库应该真的能被 `go get`，而不是只在文档里叫「核心」。

> **一个明确接受的代价**：Apache-2.0 **不要求回馈改进**。
> 有人可以拿走这份代码、改进它、闭源、甚至当服务对外提供，且无需回馈一行。
> 这是选择宽松许可时**主动接受的**，不是疏忽。
>
> 对这个项目尤其无所谓：协议本身是开放的（git 远端 + 几个 HTTP 接口），
> 任何人照着协议重写一份都不需要碰这份代码。**开放协议是挡不住的，也不该挡。**

贡献走 **DCO**（不是 CLA）：你保留版权，项目也无法重新授权。见 [CONTRIBUTING.md](CONTRIBUTING.md)。

---

## 验证

```bash
# Go：44 个用例（含竞态检测）
go vet ./... && go test -race ./...

# 前端逻辑 + 浏览器 + 端到端
cd tools && npm install && npm run smoke && npm run visual
```

CI 分三个 job（`go` / `smoke` / `browser`），全部跑在 x86_64 runner 上，
截图与实测数据作为 artifact 回传。见 `.github/workflows/ci.yml`。

> 开发机若为 aarch64，`-race` 在 proot 沙箱里会因 VMA 受限失败 —— 那是环境限制，
> 竞态检测交给 CI。

### 已知的本机环境限制（不影响产品）

本机的 proot 沙箱里 **`receive-pack`（push 的服务端）必定失败**，任何消费方都如此：

```
$ git init --bare h.git && git push h.git HEAD:refs/x
error: unpack should have generated <sha>, but I can't find it!
 ! [remote rejected] HEAD -> refs/x (bad pack)
```

**已排除的可能**（都实测过）：

| 假设 | 实测结果 |
|---|---|
| 文件系统写不了 packfile | ✗ `git repack` / `git bundle` / `git clone --no-local` 全部正常 |
| upload-pack 也有问题 | ✗ fetch / clone 完全正常 |
| 是本地路径传输的锅 | ✗ `file://`、`file://localhost/` 同样失败 |
| 可以靠参数绕过 | ✗ `--no-thin` / `unpackLimit=1` / `fsync=none` / `threads=1` 全部失败 |

`GIT_TRACE` 显示失败发生在 receive-pack 的 **quarantine 迁移**：`unpack-objects`
以 `GIT_OBJECT_DIRECTORY=.../tmp_objdir-incoming-XXXX` 写入对象，之后的存在性检查
却找不到它。这是 proot + 该内核组合的问题，**与 ImmuLog 无关** —— 上面那段
复现里没有任何 ImmuLog 代码。

**影响范围**：仅限本机开发。CI 跑在 GitHub 的原生 x86_64 runner 上，push 正常执行。
`internal/gitx` 的 push 测试在本机会以精确条件跳过（只认 "bad pack" 这一个症状），
其余测试与端到端用 **fetch 播种中转仓库** 绕过 —— 这不改变被测代码路径，
因为同步逻辑只读。

---

## ⚠️ 发布前必做

1. **vendor Beer CSS**：CDN 是供应链漏洞。需连同 35 个 shape SVG 与图标字体一起 `embed`（约 1 MB），
   并把 `index.html` 里的外链换成 `/assets/`。
2. **配置签名密钥**：否则 `author` 字段只是装饰品。

---

## 路线图

| 阶段 | 内容 | 状态 |
|---|---|---|
| 0–4 | `gitx` + feed + SSE + 见证锚 + 告警 + 签名轮换 + 多源同步 + 快照 + 锚定链 | ✅ |
| 5 | 端到端加密 + crypto-shredding（可遗忘） | ✅ |

## License

待定。
