# Immutalk

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
| 设计文档 | ✅ 14 节 |
| 前端界面 | ✅ 可运行，128 项检查通过 |
| 后端 `gitx` / `feed` / `web` | ✅ 完成，44 个 Go 用例 |
| 多源同步 / 外部锚定 / 加密 | ⬜ 路线图 4–5 |

## 跑起来

```bash
go build -o immutalk .
./immutalk                      # 监听 :8081，仓库默认为 ./repoDB
```

需要先在 git config 里有身份：

```bash
git config --global user.name "你的名字"
git config --global user.email "you@example.com"
# 可选但强烈建议：签名（见 DESIGN.md §4）
git config --global gpg.format ssh
git config --global user.signingkey ~/.ssh/id_ed25519.pub
```

没有配置 `user.signingkey` 时，服务会在启动日志里**明确警告身份可被冒名**——它不假装安全。

环境变量：`IMMUTALK_REPO`（默认 `./repoDB`）、`PORT`（默认 `8081`）。

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
immutalk/
├── go.mod                  零第三方依赖（没有 go.sum）
├── main.go                 组装与生命周期
├── docs/DESIGN.md
├── internal/
│   ├── gitx/               ← 唯一允许出现 os/exec 的包
│   │   ├── exec.go         子进程边界：stdin 注入、超时、错误归一、Init
│   │   ├── object.go       commit-tree / hash-object / log
│   │   └── ref.go          CAS 读写 / for-each-ref / is-ancestor / count
│   ├── feed/               ← 领域语义
│   │   ├── feed.go         消息编解码 + 发送 / 撤回 / 历史
│   │   └── verify.go       见证锚与引用重写检测
│   └── web/                ← 传输
│       ├── http.go         路由与处理器（net/http）
│       ├── sse.go          EventSource 流（零依赖）
│       └── guard.go        完整性巡检与告警广播
└── web/                    前端，//go:embed
    ├── index.html
    ├── style.css
    └── app/                main / stream / api / store / render / mock
```

> **架构约束：同一条原则镜像到两侧。**
> 后端 `gitx` 是唯一能碰 `os/exec` 的包；前端 `stream` 是唯一能碰 `EventSource` 的文件。

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

---

## ⚠️ 发布前必做

1. **vendor Beer CSS**：CDN 是供应链漏洞。需连同 35 个 shape SVG 与图标字体一起 `embed`（约 1 MB），
   并把 `index.html` 里的外链换成 `/assets/`。
2. **配置签名密钥**：否则 `author` 字段只是装饰品。

---

## 路线图

| 阶段 | 内容 | 状态 |
|---|---|---|
| 0–3 | `gitx` + feed + SSE + 见证锚 + 告警 | ✅ |
| 4 | 快照 gossip + Merkle 一致性证明 + 外部锚定 | ⬜ |
| 5 | epoch 密钥加密（可遗忘） | ⬜ |

## License

待定。
