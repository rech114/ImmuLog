# Immutalk 设计文档

> 一个把 Git 当作信任根（而非仅仅是数据库）的聊天系统。
> 目标不是「防止撤回」，而是 **撤回可证明**。

---

## 0. 一句话命题

**Git 的哈希给不了「防撤回」，它给的是「撤回会留下证据」。**

所以本项目的需求不是「让管理员删不掉」，而是：

> **任何一次删除、回滚、改写，都无法静默发生。**
> 攻击者可以拒绝服务、可以拖慢你，但**做不到不留痕迹**。

强度公式只有一条：

```
防撤回强度 = 副本分布广度 × 锚定外部性
```

哈希本身是免费附赠品；值钱的是「别人手里也有那份哈希」。

---

## 1. 目标与非目标

### 目标

| # | 目标 |
|---|---|
| G1 | 消息不可变；撤回是一条**追加事件**，不是删除操作 |
| G2 | 身份由**签名**决定，`author` 字段只是标签 |
| G3 | 引用重写（force push / 回滚 / split view）**可被客户端检测并留下证据** |
| G4 | 单二进制、零第三方依赖、自建节点即可运行 |
| G5 | 与 Git 生态完全兼容（`git log` 就是聊天记录） |

### 非目标

- ❌ 不追求实时音视频、大文件传输
- ❌ 不追求「绝对无法删除」（做不到，见 §6.4）
- ❌ 不自研密码学、不自研对象存储、不自研 HTTP 栈
- ❌ 不隐藏「撤回发生了」这件事——**透明化是产品要求，不是妥协**

### 技术选型结论

**Go + 原版 Git CLI。零第三方依赖。**

理由：「用原版 Git」这个决定把语言选择的重要性抽干了——剩下的只是路由、调子进程、推流三件胶水活。而 Go 的标准库里有一个生产级 HTTP 服务器，Rust 里没有。这是唯一无法抹平的差距。

| | Go | Rust |
|---|---|---|
| HTTP 服务器 | `net/http`（标准库） | axum + hyper + tower + **tokio** |
| 子进程 | `os/exec` | `std::process::Command`（**持平**） |
| 第三方依赖 | **0** | 5 直接 / 80+ 传递 |

**进程模型持平的这一条很关键**：选了原版 Git，Rust 的性能优势被架空；剩下的全是依赖数量的 0 比 80。对一个防篡改项目，**零依赖是安全属性**——供应链攻击面归零。

---

## 2. 威胁模型

签名 commit 决定了攻击者的**能力上限**：他不能伪造内容（没有作者私钥），也改不了任何已有 commit 的字节（改了哈希就变）。

他只剩四张牌：

| 攻击 | 手段 | 本质 |
|---|---|---|
| **吞** | 丢弃 object / 不回 ref | 让消息消失 |
| **退** | 把 ref 指回旧 commit | 回滚 rollback |
| **换** | force push 到另一条历史 | 引用重写 |
| **骗** | 对不同客户端说不同的话 | 分裂视图 split view |

「换」和「退」本质相同：**ref 从 A 变成 B，而 B 不是 A 的后代**。统称**引用重写**。

> **设计原则：引用重写不需要「防」，只需要「被发现」。**

### 攻击者做不到什么

- 伪造内容（无私钥）
- 修改历史 commit 的字节（哈希会变）
- 让篡改**不留痕迹**（这是本项目的全部价值）

---

## 3. 数据模型

### 3.1 核心铁律

> **消息必须绑定在 object 上，绝不能绑定在 ref 上。**

因为：**ref 是可变的，object 是不可变的。** 所有「撤回」在 Git 里都等价于移动或删除 ref。

### 3.2 概念映射

| 聊天概念 | Git 原语 | 备注 |
|---|---|---|
| 消息 | commit object | 内容寻址 |
| 消息正文 | commit message（或 blob） | 见 §3.4 |
| **用户** | **`refs/feeds/<pubkey>`** | 每人一条 append-only feed |
| 撤回 | 一条带 trailer 的新 commit | 绝不删原对象 |
| 送达回执 | 反向引用对方 OID 的 trailer | 不可否认性来源 |
| 已读 | ref 或 tag | 可变，不作证据 |
| 签名 | commit 的 `gpgsig` 头 | `author` 字段是装饰品 |

### 3.3 为什么是 per-user feed，而不是共用一条分支

原型的做法是所有人往 `refs/heads/main` 写。后果是**第二条消息必然是非快进，必然被拒**（实测 `! [rejected] (non-fast-forward)`）。

Immutalk **每人一条 feed**：

```
refs/feeds/<pubkey-alice>
refs/feeds/<pubkey-bob>
refs/feeds/<pubkey-carol>
```

- 天然零冲突：没有人写别人的 ref
- 天然 append-only：只允许快进
- 想伪造 alice 的 feed → 必须 force push → 而别人手里有旧副本 → **当场分叉暴露**

### 3.4 一条消息的物理形态

```
commit object
├── tree:        复用父的 tree（消息不产生任何文件）
├── parent:      同一作者的上一条 commit        ← 因果链
├── gpgsig:      作者密钥签名                     ← 身份
└── message:
      第二条消息                                ← 人类可读正文

      Immutalk-Seq: 2                          ← 单调序号（防回滚）
      Immutalk-Retracts: <被撤回的 OID>         ← 撤回（可选）
      Immutalk-Receipt: <我确认收到的对方 OID>   ← 回执（可选）
```

Trailer 是 Git 原生的键值语法，`%(trailers:key=...,valueonly)` 可直接提取，**人可读、机器可解析、`git log` 里直接看得见**。

**不使用 `--allow-empty`**——那是为了绕过 index 才需要的 hack（见 §5.1）。

---

## 4. 身份与签名

### 4.1 身份 = 密钥，不是 author 字段

原型把 `git config user.name/email` 当身份用，实测可以零成本冒名：

```
GIT_AUTHOR_NAME='管理员 <img src=x onerror=alert(1)>' git commit --author='管理员 <admin@x>'
→ author=管理员 <admin@x>     ← 无任何校验
```

Immutalk 用 **Git 原生 commit 签名**：

```bash
git config gpg.format ssh
git config user.signingkey ~/.ssh/id_ed25519.pub
git commit-tree -S <tree> -p <parent>     # 签名白送，零代码
```

签名覆盖 commit 内容 + tree，因此：

- 谁说的 = **可验证的**
- `author: 张三` = **只是给人看的标签**

### 4.2 身份不可由前端指定

前端**永远不发**作者字段。作者由服务端配置的密钥决定。这从结构上消灭了「伪造他人身份」这条路径。

### 4.3 密钥轮换

换密钥 = 一条**密钥轮换公告** commit，用旧密钥签名，声明新旧公钥的衔接。链不能断，断了即攻击。

---

## 5. 写入路径

### 5.1 只用 `commit-tree`，永不用 `commit`

| | `git commit` | `git commit-tree` |
|---|---|---|
| index | 需要 | **不需要** |
| worktree | 需要 | **不需要** |
| `.lock` 文件 | 会产生 | **不产生** |
| bare 仓库可用 | ❌ | ✅ |
| 并发写不同 ref | 冲突 | **天然安全** |
| 消息注入方式 | 参数拼接（**危险**） | **stdin**（安全） |

实测：在 bare 仓库里 `commit-tree` 无 worktree、无 index、无锁即可造 commit。

```bash
echo "第二条消息" | git commit-tree <tree> -p <parent>
```

### 5.2 ref 更新一律用 CAS

```bash
git update-ref refs/feeds/alice <new> <old>
```

`<old>` 不匹配 → **拒绝**（实测 `exit 128, cannot lock ref`）。

> **这一行就是「本地见证锚」，就是「拒绝静默改写」。Git 原生，零代码，原子操作。**

多点更新用 `git update-ref --stdin -z`（原子事务）。

### 5.3 写入的完整时序

```
POST /api/commit
   │
   ├─ 1. 校验签名密钥存在
   ├─ 2. 组装 trailer（seq / retracts / receipt）
   ├─ 3. git commit-tree -S          → 得到新 OID
   ├─ 4. git update-ref <ref> <new> <old>   ← CAS，失败即中止
   ├─ 5. 广播 SSE 事件
   └─ 6. 返回 201 { oid, seq }
```

**第 4 步失败 = 有人抢先了或本地被篡改**，必须原样返回错误，**不能吞掉**。

---

## 6. 撤回与完整性

### 6.1 撤回 = 一条追加事件

```bash
git commit-tree ... <<EOF
撤回请求

Immutalk-Retracts: <目标 OID>
Immutalk-Reason: 发错了
EOF
```

- **默认 UI**：渲染成 `[已撤回]`
- **可展开**：任何持有副本的人都能看到原文
- **永不删除**：原 object 仍在

如果做成「撤回后连自己也看不到」，用户会直接换软件——**那是产品自杀**。

### 6.2 让合法重写也必须留痕

用户本人也会 rebase、reset、换密钥。如果一见引用重写就报「攻击」，会天天误报。

**解法**：合法重写 = 一条**签名过的重写公告**，用旧密钥声明「我于 <时刻> 重写了从 `<OID>` 之后的历史」。

判定规则因此变得极干净：

| 情况 | 判定 |
|---|---|
| 有公告 + 签名有效 + 公告锚点 = 本地见证 | 用户合法重写，接受 |
| **无公告 / 签名无效 / 锚点对不上** | **铁证**，无需犹豫 |

> **把攻击从「可以伪装成正常操作」变成「必须出示一份造不出来的签名」。**

### 6.3 完整性四层

| 层 | 机制 | 拦住什么 |
|---|---|---|
| L1 | **本地见证锚**（`update-ref` 的 CAS + 本地记录） | 回滚、引用重写 |
| L2 | **单调序号**（签名过的 `Immutalk-Seq`） | 回退到合法旧 commit |
| L3 | **回执链**（我的 commit 引用你的 OID） | 单方面删除（会留下悬挂引用） |
| L4 | **快照 gossip + 外部锚定** | 分裂视图、全局重写 |

**L3 值得单独说**：alice 的第 42 条被 bob 的回执引用着，而 bob 的 feed 由 bob 签名、你自己本地也有副本。管理员想删掉它，必须**同时改写 alice 和 bob 两条 feed**——**两把私钥他都没有**。他唯一能做的是「不回」，那你会立刻看到一个填不上的洞。

**L4 复用现成轮子**：快照用 Merkle root，一致性证明抄 Certificate Transparency 的 STH / consistency proof 设计，客户端之间互发快照哈希比对以检测 split view。周期性地把快照哈希交给 OpenTimestamps 之类的服务做外部锚定。

### 6.4 必须诚实说明的边界

| 限制 | 说明 |
|---|---|
| 私钥泄露 | 可以永久改写自己的历史，签名救不了。只能靠密钥轮换 + 撤销公告 |
| 时间戳不可信 | commit date 是本机时钟。只有外部锚定才有意义 |
| 全量重写 + 全部副本下线 | 无解。强度上限 = 副本分布广度 |
| 新用户 bootstrap | **无法验证全部历史**。只能信任一组独立见证人签名的 checkpoint |
| SHA-1 | 已实践碰撞。Git 有加固实现；SHA-256 仓库格式尚未完成互通迁移，`object-format` 要设计成可配置项 |

**新用户 bootstrap 是必须显式写出来的信任假设，不是漏洞。** 在 UI 里告诉用户「你的历史完整性由这 N 个见证人背书，你可以自己成为见证人以降低依赖」。

---

## 7. 前端 ↔ 后端通信设计

> 这一节回答：**浏览器和后端怎么连。**

### 7.1 结论：非对称双通道

```
                    ┌────────────────────────────────┐
   上行（低频）      │  普通 HTTP POST                │
   浏览器 ─────────► │  POST /api/commit              │
                    │  ← 同步返回 { oid, seq }        │
                    └────────────────────────────────┘

                    ┌────────────────────────────────┐
   下行（高频）      │  SSE  (text/event-stream)      │
   浏览器 ◄───────── │  GET /api/stream               │
                    │  ← 长连接，服务器单向推          │
                    └────────────────────────────────┘
```

**为什么要非对称？** 因为两个方向的本质不同：

| | 上行 | 下行 |
|---|---|---|
| 频率 | 低（人打字） | 高（所有人说话） |
| 需要 | **确认、幂等、重试、错误码** | 低延迟、广播、断线续传 |
| 最合适的轮子 | **HTTP** | **SSE** |

用 WebSocket 做上行，等于**自己重新发明 HTTP 的状态码、幂等和重试**。复用 HTTP 是「能复用轮子就复用轮子」的直接体现。

### 7.2 上行：只有两个端点

| 方法 | 路径 | 作用 |
|---|---|---|
| `GET` | `/` | `index.html`（`embed` 内嵌，**同源 → 零 CORS 配置**） |
| `GET` | `/api/stream` | SSE 事件流（下行 + 首屏历史） |
| `POST` | `/api/commit` | **唯一写入口** |
| `GET` | `/api/snapshot` | 当前快照哈希（供对端 gossip） |

`POST /api/commit` 请求体：

```json
{ "kind": "msg", "body": "你好" }
{ "kind": "retract", "retracts": "<oid>", "reason": "发错了" }
{ "kind": "receipt", "refs": ["<oid>", "<oid>"] }
```

响应：

```json
201 { "oid": "...", "seq": 42, "at": "..." }
4xx { "error": "cas_failed", "expected": "<old>", "actual": "<new>" }
```

**`cas_failed` 必须原样暴露给前端**，因为它是篡改检测的信号源，不能吞掉。

**用 `kind` 区分事件类型，而不是开三个端点。** 一个写入口，前端只需要一个 fetch 函数，语义扩展不用改路由——这同时满足「精简」和「易于扩展」。

### 7.3 下行：SSE，零依赖

`text/event-stream` 是 `net/http` + `http.Flusher` 就能实现的标准协议，**不需要任何第三方库**。

事件格式：

```
retry: 3000

id: 3f2a1b9c...
event: msg
data: {"oid":"3f2a...","author":"...","body":"你好","seq":42}

id: 7d1e0f4a...
event: retract
data: {"oid":"7d1e...","retracts":"3f2a...","reason":"发错了"}

event: alarm
data: {"kind":"rewrite","feed":"<pubkey>","local":"3f2a...","remote":"9c8b..."}

: ping
```

事件类型：

| `event` | 含义 |
|---|---|
| `hello` | 连接建立，带当前 head / 游标 / 服务器时间 |
| `msg` | 新消息 |
| `retract` | 撤回 |
| `alarm` | **篡改告警**（§6.3 的检测结果，前端渲染成删不掉的系统消息） |
| `snapshot` | 快照哈希（gossip 用） |
| `checkpoint` | 定期锚点 |

### 7.4 断线续传：白送的机制

**这是选 SSE 而不是 WebSocket 的核心收益。**

SSE 的 `id:` 字段和浏览器的 `EventSource` 组合，天然提供：

1. **自动重连**（无需写一行重连代码）
2. **游标续传**：重连时浏览器自动带上请求头 `Last-Event-ID: <最后收到的事件 id>`

而我们把**事件 id 设成 commit 的 OID**，于是：

```
Last-Event-ID: 3f2a1b9c...
        ↓
服务端:    git log --format=... 3f2a1b9c..<tip>
        ↓
精确续传，一条不多一条不少
```

> **事件 ID 就是内容哈希 —— 天然全局唯一、天然可验证、天然是续传游标。**
> 哈希的第三个用途，白送。

**注意区分两种游标**（这是关键的架构纪律）：

| 游标 | 位置 | 用途 | 可信度 |
|---|---|---|---|
| `Last-Event-ID` | 浏览器 | **性能机制**：续传 | 由服务器间接影响，**不可信** |
| 本地见证锚 | 服务端 | **安全机制**：检测改写 | 本地持有，**可信** |

> **绝对不能用 SSE 游标代替见证锚。** 服务器能重置你手里的游标，但不能伪造你本地的见证文件。

### 7.5 首屏历史复用同一条连接

不需要单独的「加载历史」接口：

```
GET /api/stream                      ← 前端启动时唯一要做的事
  event: msg     × N                 ← 服务端从最近 N 条开始回放（git log -n）
  event: hello                       ← 回放完毕，切换成实时模式
  event: msg     ...                 ← 之后是增量
```

一个连接，从历史无缝过渡到实时，**前端不需要维护「已加载到哪」的状态机**。

分页由服务端掌握：首次先推最近 50 条，更早的历史在用户上滑时用 `GET /api/stream?since=<oid>&backward=1` 补拉（同一套代码路径）。

### 7.6 乐观投递：把验证移出关键路径

```
用户按回车
   │
   ├─ 立即上屏（灰色，标记 ⚠️ 未确认）      ← 零延迟
   │
   └─ POST /api/commit
         ├─ 201 → 用返回的 OID 把临时气泡替换成真身
         ├─ 4xx cas_failed → 标红，附「被抢先/历史冲突」
         └─ 网络失败 → 自动重试（指数退避）

同时：SSE 可能也推来这条消息
   → 用 OID 去重（已经乐观渲染过了）
```

**上行确认和下行广播是两条独立路径，靠 OID 去重收敛。** 这是「可验证（verifiable），而非已验证（verified）」在协议层的落地。

### 7.7 心跳与背压

| 问题 | 做法 |
|---|---|
| 中间代理掐断空闲连接 | 每 15–30 秒发一行注释 `: ping` |
| 客户端太慢 | per-client 有界缓冲；满了就断开，让浏览器重连并走 `Last-Event-ID` 续传 |
| 慢客户端拖垮服务端 | 广播用非阻塞 send，丢弃慢客户端**不做阻塞写** |

### 7.8 降级路径

如果某些企业代理会缓冲 `text/event-stream`：

- 先用 `X-Accel-Buffering: no` 响应头尝试关闭代理缓冲
- 仍失败 → 降级为 `GET /api/stream?poll=1`，服务端立即返回一批事件并关闭连接，浏览器靠 `retry:` 字段决定的间隔重试

**同一套服务端代码路径，只是连接不保持。** 前端 `EventSource` 代码完全不用改。

### 7.9 鉴权：分清两种信任

这是最容易混淆的地方，必须分清：

| 层 | 机制 | 保护什么 |
|---|---|---|
| **传输层** | 启动时生成 token → cookie | 「谁有权连到我的本地节点」 |
| **内容层** | **签名 commit** | 「这条消息真的是 alice 说的」 |

> **HTTP 鉴权只保护本地服务，不保护仓库。**
> 消息的真实性**永远不依赖** HTTP 认证——即使有人拿到了你的 token，他也只能发**他自己签名**的消息，伪造不了任何人。

这个区分很重要：它让鉴权退化成「防骚扰」而不是「保安全」，因此可以选择最简实现（一个 token 存 cookie），不必引入 OAuth/JWT。

### 7.10 为什么不用 WebSocket

| | SSE | WebSocket |
|---|---|---|
| 依赖 | **`net/http` + `Flusher`** | `gorilla/websocket` + `x/net` |
| 自动重连 | **浏览器内置** | 自己写 |
| 断线续传 | **`Last-Event-ID` 内置** | 自己做应用层协议 |
| 方向 | 单向 | 全双工 |
| 我们是否需要全双工 | **不需要**（上行走 POST） | — |

最后一行是关键：**上行已经交给 HTTP 了，全双工就是浪费。**

顺带一提：原型项目里的 WebSocket `readPump` 把浏览器发来的消息直接广播给所有人、**却不落库**——它本来需要的也是单向推送。把死代码变成设计决策，顺手砍掉整个 `Hub`/`Client`/channel 状态机。

---

## 8. 界面（MD3 Expressive）

### 8.1 视觉语言：三根支柱，不是材质

MD3 Expressive 用**形状、动效、色彩**表达情绪和层级，**不用纹理和材质**。

所以界面设计的第一原则是：

> **把状态编码成形状，而不是编码成文字。**

沉浸式的"审计日志"式拟物是反模式——它把产品信息量当成了视觉风格。正确做法是让信息**在需要时可见**，而不是时刻铺满屏幕。

### 8.2 技术选型：Beer CSS CDN + 原生 ESM，零构建

| 决策 | 值 |
|---|---|
| CSS 框架 | **Beer CSS 5.0.3**（`cdn.jsdelivr.net/npm/beercss@5.0.3`） |
| 动态色彩 | `material-dynamic-colors@1.1.4` |
| JS | **原生 ES modules**，无打包器 |
| 前端第三方 JS 依赖 | **0** |

**为什么 Vanilla JS 就够：**

> 前端状态是**单一的、单向的、只追加的**。框架解决的是"状态分散 + 双向绑定 + diff"，而我们没有 diff 需求——DOM 对尾部追加本来就是 O(1) 最优结构。
>
> 用框架渲染 append-only 日志，是拿最重的轮子干最轻的活。

**为什么不能引入构建链：** `//go:embed` 是"零依赖单二进制"的柱子。一旦有 `npm run build`，就多出 Node 运行时、`dist/` 同步、dev server，以及"为什么一个防篡改软件要 800 个 npm 包"这个答不上来的问题。

### 8.3 形状即状态

Beer CSS 自带 35 个 M3 形状，把它们变成状态语汇：

| 状态 | 形状 | 颜色 | 语义 |
|---|---|---|---|
| 已验签 | `gem` | `--primary` | 切面、完整、确定 |
| 待确认 | `loading-indicator` | `--tertiary` | 乐观投递中（旋转） |
| 未验签 | `circle` | `--secondary` | 未知 |
| 已撤回 | `slanted` | `--error` | 被切掉 |
| **篡改告警** | `burst` | `--error` | 炸开 |

撤回时形状从 `gem` **切到 `slanted`**——这正是 M3 shape-morph 的标准用法，而且它不是装饰，**它就是这个产品的核心叙事**。

### 8.4 主题色 = 房间的创世哈希

动态色彩（Material You）的种子色取自**房间第一个 commit 的 OID 前 6 位**：

```js
ui('theme', `#${genesisOid.slice(0, 6)}`);
```

于是：

> **两个房间颜色相同，就意味着它们的历史同源。**

这不是装饰性的换肤——**配色是身份哈希的函数**，零成本地把"内容寻址"这件事透到了视觉层。

### 8.5 三条不可妥协的交互规则

| 规则 | 做法 | 反面 |
|---|---|---|
| **撤回必须留痕** | 原位删除线 + 形状切到 `slanted`，点开仍可见原文 | 撤回后连自己也看不到 → 用户直接换软件 |
| **告警不是 toast** | `alarm` 是**插进时间线里的一条封条**，永不自动消失 | 弹窗一闪而过 = 可以装作没看见 |
| **乐观投递** | 消息先上屏（`pending` 形状），POST 落地后原地换成正身 | 等全链验证再上屏 = 唯一"优化安全导致产品死亡"的路径 |

**临时项落地是原地替换**（OID 改写、DOM 不移动），所以用户看到的气泡不会跳。

### 8.6 前端结构（四个"唯一"）

```
web/
├── index.html              MD3 骨架（Beer CSS 由 CDN 提供）
├── style.css               只补 Beer 没有的：布局、形状语义、motion、字体
└── app/
    ├── main.js             组装与入口（唯一同时知道四层的地方）
    ├── stream.js           ← 唯一碰 EventSource
    ├── api.js              ← 唯一碰 fetch
    ├── store.js            ← 唯一持有状态
    ├── render.js           ← 唯一碰 document
    └── mock.js             演示数据源（?demo=1），契约与真实 SSE 相同
```

> **任何文件都不许跨层调用。**
> 这与后端的「`gitx` 是唯一能碰 `os/exec` 的包」是**同一条规则的镜像**。

校验方式（应始终成立）：

```
EventSource 只出现在 stream.js
fetch       只出现在 api.js
document.   只出现在 render.js
```

**数据流严格单向：** `stream → store → render`，写路径 `api → store → render`。

**`mock.js` 的存在是有意的**：它让界面在后端完成前就能跑，而且因为契约与真实 SSE 完全一致，接通时只需删掉 `?demo=1`——**这正是"四个唯一"纪律的回报**。

### 8.7 排版

| 用途 | 字体 | 理由 |
|---|---|---|
| 显示体 | **Bricolage Grotesque** | 可变（opsz/wdth/wght），有性格，未被用滥 |
| UI / 正文 | **Roboto Flex** | MD3 Expressive 的自有字体；Expressive 强调排版正是靠它的可变轴 |
| 哈希（仅元数据） | **Martian Mono** | 半窄技术等宽，只在 `.meta` / `.detail` 里出现 |

**哈希信息默认折叠**，点 `.meta` 展开 `.detail` 才看到完整 OID 与签名——这是 §8.1 原则的具体落地。

### 8.8 ⚠️ CDN 的供应链问题

**一个防篡改产品在运行时从第三方 CDN 拉 CSS，是供应链漏洞。**

这不是洁癖：本项目的全部论点就是「不要信任第三方」，而 `cdn.jsdelivr.net` 是一个能随时改变你界面的第三方。

| 阶段 | 做法 |
|---|---|
| 开发 | CDN（当前实现） |
| **发布** | **vendor 进 `embed`**，Beer CSS 官方文档有现成的 "LOCAL CDN VERSION" 一节 |

vendor 需要的文件（实测体积）：

| 文件 | 体积 |
|---|---|
| `beer.min.css` | ~88 KB |
| `beer.min.js` | ~19 KB |
| `material-dynamic-colors.min.js` | — |
| **35 个 shape SVG**（`gem.svg` `burst.svg` …） | ~40 KB |
| **3 个图标字体 woff2** | 主要开销 |

合计约 1 MB——对单二进制可以接受。注意两点：

1. **shape 的 SVG 是外部文件**（`mask-image: url(gem.svg)`），只 vendor CSS 会导致所有形状失效。
2. Beer CSS 未加 `-webkit-mask` 前缀，**旧版 Safari 上形状可能不渲染**（现代版本无前缀支持）。

### 8.9 两个踩过的 Beer CSS 坑（务必记住）

| # | 现象 | 真因 | 对策 |
|---|---|---|---|
| 1 | 副标题离标题一个多字符高 | Beer 有一条特异性 `(0,4,1)` 的全局规则，给**所有跟在兄弟元素后面的 `<p>`** 加 `margin-block-start: 1rem`；`.kv p` 这种 `(0,1,1)` 压不住 | 用它自留的逃生口 `:not([class*=margin])`——给每个 `<p>` 加 `no-margin`，间距改由显式规则控制 |
| 2 | 键值行整页竖排堆叠 | `style.css` 里一行 **`// 注释`**（CSS 不支持）被解析成选择器的一部分，导致 `.kv { display:flex }` 这条规则**从未生效** | 只能用 `/* */`。已在 `smoke.mjs` 加护栏：花括号平衡 + 选择器不得含 `//` + 关键规则必须在解析结果里 |

第 2 条尤其阴险：**语法错误不报错，只是规则静默消失**。而且它和坑 1 叠加时，会被误判成「改过头了」——**定位必须靠解析结果，不能靠看截图猜**。

> **新增 `<p>` 时务必带上 `no-margin`。**

### 8.10 验证方式：本机不跑浏览器，全部交给 CI

开发机是 aarch64，装 Chromium 有指令集风险。因此**所有浏览器检查都跑在 GitHub Actions 的 x86_64 runner 上**，截图与实测数据作为 artifact 回传。CI 通常 ≤2:30 完成。

`tools/` 下的检查，按「对这个项目是否真的必要」筛选：

| 检查 | 职责 | 依赖 |
|---|---|---|
| `smoke.mjs` | jsdom 黑盒驱动 DOM；逻辑链路 + **样式表完整性** | jsdom |
| `check/layout.mjs` | 3 视口 × 3 页签几何实测：横向溢出 / 越界裁剪 / 内容贴边 / 触摸目标 / 安全边距 / 键值行同排 | playwright |
| `check/shapes.mjs` | 形状 `mask-image` 是否真的指向可达 SVG；图标是否渲染成字形而非退化成文字 | playwright |
| `check/resilience.mjs` | Beer JS/CSS 挂掉、写接口 500 时的降级行为 | playwright |
| `check/sse.mjs` | 自建会掐断的 SSE 服务端 → 验证浏览器自动重连与 `Last-Event-ID` 续传 | playwright + node:http |
| `check/a11y.mjs` | axe-core WCAG A/AA + 键盘可用性 + 按钮无障碍名 | axe-core |

**明确没有引入**（并记录理由）：

| 候选 | 为什么不要 |
|---|---|
| `@playwright/test` | 需要的是「把实测数据回传」，不是测试框架的 green/red；裸 playwright 够了 |
| `pixelmatch` / `BackstopJS` / Percy / Chromatic | 视觉回归要有稳定基线，而设计还在改——**没有基线就没有差分**。截图回传给人看即可 |
| `@lhci/cli` | 性能不是本项目瓶颈（单二进制、10 万条消息 ≈ 20MB） |
| MSW | `page.route()` 已能拦网络，够用 |

---

## 9. 项目结构（一文件一职责）

```
immutalk/
├── go.mod                   module immutalk（零第三方依赖）
├── main.go                  组装与生命周期（唯一知道全部依赖的地方）
├── docs/DESIGN.md           本文档
├── internal/
│   ├── gitx/                ← 唯一允许出现 os/exec 的包
│   │   ├── exec.go          子进程边界：stdin 注入、超时、错误归一
│   │   ├── object.go        commit-tree / hash-object / cat-file
│   │   └── ref.go           ref 的 CAS 读写与批量快照（for-each-ref）
│   ├── feed/                ← 领域语义，不含 IO 细节
│   │   ├── feed.go          发消息 / 读消息 / 撤回
│   │   └── verify.go        验签、seq 单调、与本地见证比对
│   └── web/                 ← 传输，不含领域逻辑
│       ├── http.go          路由与处理器（net/http）
│       └── sse.go           事件流广播（text/event-stream）
└── web/                     ← 前端，//go:embed 整个目录
    ├── index.html           MD3 骨架
    ├── style.css            布局 / 形状语义 / motion / 字体
    └── app/
        ├── main.js          组装（唯一同时知道四层的地方）
        ├── stream.js        ← 唯一碰 EventSource
        ├── api.js           ← 唯一碰 fetch
        ├── store.js         ← 唯一持有状态
        ├── render.js        ← 唯一碰 document
        └── mock.js          演示数据源（?demo=1）
```

**目录即架构约束**——同一条原则，镜像到两侧：

| 侧 | 唯一接触点 |
|---|---|
| 后端 | **`gitx` 是唯一能碰 `os/exec` 的包** |
| 前端 | **`stream` 是唯一能碰 `EventSource` 的文件** |

这条约束买到三件事：换语言 / 换 libgit2 / 加缓存层，**全部只动一个包**；换传输协议（SSE → WebSocket）/ 换渲染方式（原生 → 框架），**全部只动一个文件**。

---

## 10. 工程纪律

| # | 纪律 | 原因 |
|---|---|---|
| 1 | **参数一律拼 `[]string`，消息一律走 stdin** | 永远不拼 shell 字符串 → 无注入 |
| 2 | 每次 git 调用带 `context` 超时 | 卡住的子进程不能拖垮 handler |
| 3 | **永不用 `git commit`**，只用 `commit-tree` | 无 index、无锁、并发安全 |
| 4 | **永不在循环里调 git** | 一次 `git log --format` 取一页 |
| 5 | ref 更新一律带 `<old>` 做 CAS | 这就是见证锚本身 |
| 6 | 不用 `--allow-empty` | `commit-tree` 不需要它 |
| 7 | `UI 线程不碰 Git，也不碰密码学` | 同步做必掉帧 |

**唯一的性能代价**是每次子进程调用约 5ms。发送路径 = `commit-tree` + `update-ref` ≈ 10ms，完全够用。

**逃生口**：若将来进程开销成为真瓶颈，可换成 libgit2 / gix——**但不要为不存在的瓶颈付代价**。届时 `gitx` 包是唯一需要重写的地方，这正是它存在的意义。

### 性能与安全的兼容

| 原则 | 做法 |
|---|---|
| 关键路径只留 O(1) | 只做「验签名 + 验 tip 衔接」，其余全异步 |
| 保留 commit DAG | 用 `--filter=blob:none` 而非 `--depth`（浅克隆会摧毁验证能力） |
| 验证增量 | 从 checkpoint 往后验，成本 ∝ 新增量 |
| ref 爆炸 | protocol v2 的 `ls-refs` + ref-prefix 过滤 |
| 网络 | 聚合推送 / 聚合拉取，**一次往返 = 上万次验签** |

### 量级估算

一条消息 ≈ `commit(250B) + tree(60B)`（复用父 tree）≈ **300B 未压缩**

- 10 万条消息 ≈ **20–30 MB**
- 一年重度聊天 ≈ 一台手机随便存

**结论：存储成本可忽略。真正的敌人只有延迟和网络往返。**

---

## 11. 路线图

| 阶段 | 内容 | 影响范围 |
|---|---|---|
| **0** | `os/exec` 取代 go-git；`commit-tree` + `update-ref` CAS 写路径 | `gitx/` |
| **1** | per-user feed 模型；trailer 元数据；删掉 fsnotify | `feed/` |
| **2** | SSE 取代 WebSocket；Gin 换成 `net/http`；**依赖降到 0** | `web/` |
| **3** | commit 签名（`gpg.format=ssh`）+ 本地见证锚 + `alarm` 事件 | `feed/verify.go` |
| **4** | 快照 gossip + Merkle 一致性证明 + 外部锚定 | 新增 `feed/gossip.go` |
| **5** | epoch 密钥加密（crypto-shredding，实现「可遗忘」） | 新增 `feed/crypto.go` |

**每个阶段都能独立跑起来，都能回滚。** 先拿到最大的收益（阶段 0–2 即可得到一个可用的、零依赖的、原版 Git 的聊天），再谈密码学。

---

## 12. 反模式清单

| ❌ | 为什么 |
|---|---|
| 用 `--depth` 浅克隆换性能 | 没有历史 = 无法验证历史。用 `blob:none` |
| 每条消息一次 fetch/push | 用最贵的操作（往返）省最便宜的东西（字节） |
| 同步等全链验证再上屏 | 唯一一个「优化安全性会导致产品死亡」的地方 |
| 用 `git commit` / `Worktree.Pull()` | 引入锁，且 `Pull` 会 **merge 接受分叉** |
| 把 `author` 字段当身份 | 纯文本，零成本冒名 |
| 用文件系统事件当正确性来源 | `pack-refs` 后监听目录会**静默变空** |
| 用 `+` refspec | 明确授权强制覆盖本地引用 |
| 允许 `git replace` / grafts | 本地改写历史的合法后门，会污染验证链 |
| 依赖服务器的 reflog / 服务器自签快照 | split view 下毫无意义，他能签两份 |
| 撤回后连自己也看不到 | 用户会直接换软件 |

---

## 13. 术语表

| 术语 | 含义 |
|---|---|
| **feed** | 每个用户一条 append-only 的 commit 链，`refs/feeds/<pubkey>` |
| **见证锚 witness anchor** | 客户端本地记录的「我上次见过的 ref 值」，不可被服务器覆盖 |
| **引用重写** | ref 从 A 变到 B，且 B 不是 A 的后代（force push / 回滚） |
| **分裂视图 split view** | 服务器对不同客户端展示不同历史 |
| **重写公告** | 用户合法重写历史时必须留下的签名追加事件 |
| **回执链** | 我的 commit 引用你的 OID，形成互相咬合的 DAG |
| **tombstone** | 撤回事件；一条指向被撤回 OID 的新 commit |
| **可验证 verifiable** | 随时**可以**被验证——本项目的目标 |
| **已验证 verified** | 任何时候都**已经**被验证——昂贵，且不必要 |

---

## 14. 收束

> **内容层零信任**（哈希自证）
> **发现层多源共识**（本地见证 + gossip + 外部锚定）
> **重写必须留痕**（签名公告）
> **回执互相咬死**（DAG 互引）
> **撤回即追加**（tombstone，永不删除）

做到这五条，管理员就退化成**一个可被任意替换的搬运工**：他能拒绝服务、能拖慢你，但**做不到静默**。

他每一次动手，都会在聊天流里长出一条**你自己生成、他删不掉**的系统消息。

> 这就是「隐身不可能」——不是因为他不能改，
> 而是因为**改了这个动作必然留下签名都盖不住的痕迹**。
