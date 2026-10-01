# Immutalk

把 Git 当作信任根的聊天系统。**撤回可证明，而非不可删除。**

> 攻击者可以拒绝服务、可以拖慢你，但**做不到静默地**删除、回滚或改写历史。
> 他每一次动手，都会在聊天流里长出一条你自己生成、他删不掉的**系统消息**。

完整设计见 **[docs/DESIGN.md](docs/DESIGN.md)**。

---

## 技术选型（已定）

| | |
|---|---|
| 后端语言 | **Go** |
| Git | **原版 Git CLI**（`os/exec`，不用 go-git / libgit2） |
| 后端第三方依赖 | **0** |
| 前端框架 | **Beer CSS 5.0.3**（CDN）+ 原生 ES modules |
| 前端构建链 | **无** |
| 前端通信 | 上行 `POST` + 下行 **SSE** |

---

## 当前状态

| 部分 | 状态 |
|---|---|
| 设计文档 | ✅ 完成（14 节） |
| 前端界面 | ✅ **可运行 + 冒烟通过 47 项** |
| 后端 `gitx` / `feed` / `web` | ⬜ 未开始 |

### 立刻看界面

```bash
cd web && python3 -m http.server 8099
# 打开 http://127.0.0.1:8099/?demo=1
```

`?demo=1` 走 `app/mock.js`，脚本化演示五种状态：已验签、撤回、篡改告警、乐观投递、待确认。
**契约与真实 SSE 完全一致**——后端接通后删掉 `?demo=1` 即可。

### 跑冒烟测试

```bash
cd tools && npm install && npm run smoke
```

黑盒驱动真实 DOM（jsdom），两个场景共 47 项断言：

| 场景 | 覆盖 |
|---|---|
| A `?demo=1` | 模拟时间线 → 渲染链路、撤回状态迁移、告警封条、乐观投递 rekey、视图/主题切换 |
| B 无 demo | EventSource 帧解析、OID 去重、`cas_failed` 不被吞、fetch 载荷 |

**未覆盖**（真实浏览器才能验）：CSS 布局与 `mask-image` 形状渲染、CDN 加载、
浏览器原生 `EventSource` 的自动重连与 `Last-Event-ID` 续传、移动端响应式、无障碍。

---

## 目录

```
immutalk/
├── go.mod                  零第三方依赖
├── main.go                 组装与生命周期
├── docs/DESIGN.md
├── internal/
│   ├── gitx/               ← 唯一允许出现 os/exec 的包
│   ├── feed/               领域语义：发消息 / 读消息 / 撤回 / 验证
│   └── web/                传输：net/http + SSE
└── web/                    前端，//go:embed 整个目录
    ├── index.html
    ├── style.css
    └── app/
        ├── main.js         组装（唯一同时知道四层的地方）
        ├── stream.js       ← 唯一碰 EventSource
        ├── api.js          ← 唯一碰 fetch
        ├── store.js        ← 唯一持有状态
        ├── render.js       ← 唯一碰 document
        └── mock.js         演示数据源
```

> **架构约束：同一条原则镜像到两侧。**
> 后端 `gitx` 是唯一能碰 `os/exec` 的包；前端 `stream` 是唯一能碰 `EventSource` 的文件。

---

## 原版 Git 白送的能力

```bash
# 无 index / 无 worktree / 无锁地造一条「消息」
echo "第二条消息" | git commit-tree <tree> -p <parent>

# 原生的「见证锚」：old 不匹配就拒绝（exit 128）
git update-ref refs/feeds/alice <new> <old>

# 一次调用拿到全部 feed 锚点（gossip 快照数据源）
git for-each-ref --format='%(refname:short) %(objectname)' refs/feeds/
```

---

## 界面语言

MD3 Expressive 的三根支柱：**形状 / 动效 / 色彩**。

**形状即状态**——撤回是形状从 `gem` 切到 `slanted`，告警是 `burst`；
**主题色 = 房间创世 commit 的哈希前 6 位**——两个房间同色即历史同源。

---

## ⚠️ 发布前必做

1. **vendor Beer CSS**：CDN 是供应链漏洞。需连同 35 个 shape SVG 和图标字体一起 `embed`（约 1 MB）。
2. **CDN 换成 `/assets/`**：发布版不应有任何运行时外链。

---

## 路线图

| 阶段 | 内容 |
|---|---|
| 0 | `os/exec` + `commit-tree` + CAS 写路径 |
| 1 | per-user feed + trailer 元数据 |
| 2 | 接通 SSE（删掉 `?demo=1`） |
| 3 | commit 签名 + 本地见证锚 + `alarm` 事件 |
| 4 | 快照 gossip + 外部锚定 |
| 5 | epoch 密钥加密（可遗忘） |

## License

待定。
