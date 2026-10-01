// SPDX-License-Identifier: AGPL-3.0-or-later

// Command immulog 是一个把 Git 当作信任根的聊天服务。
//
// 这个文件只负责组装与生命周期 —— 所有真实逻辑都在 internal/ 里。
// 目录即架构约束：gitx 是唯一碰 os/exec 的包，web 是唯一碰 net/http 的包。
package main

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"immulog/core/feed"
	"immulog/core/gitx"
	"immulog/internal/web"
)

// 前端由二进制内嵌 —— 部署即一个文件，用户自建节点不需要 Node.js。
//
// ⚠️ 发布前必须把 Beer CSS 一并 vendor 进 web/（见 docs/DESIGN.md §8.8）：
// 一个防篡改产品在运行时从第三方 CDN 拉样式表，是供应链漏洞。
//
//go:embed web
var embedded embed.FS

func main() {
	if err := run(); err != nil {
		slog.Error("启动失败", "err", err)
		os.Exit(1)
	}
}

func run() error {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 1) 仓库：bare、无 worktree、无 index —— 消息提交不落任何文件
	dir := env("IMMULOG_REPO", "./repoDB")
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		if err := gitx.Init(ctx, dir); err != nil {
			return err
		}
		log.Info("已初始化仓库", "dir", dir)
	}
	repo := gitx.Open(dir)

	// 2) 身份只来自 git 配置，永不来自请求体
	name, email, err := repo.Identity(ctx)
	if err != nil {
		return err
	}
	key, _ := repo.SigningKey(ctx)

	// feed 标识只由「是谁」决定，**不含密钥** —— 否则换密钥就等于换一条 feed，
	// 密钥轮换公告也就无从衔接（见 DESIGN.md §4.3）。
	pub := feed.FeedID(name + "\x00" + email)

	// 配了密钥就必须真的能签：签名不可用时拒绝启动，绝不静默降级成明文
	fingerprint := ""
	if key != "" {
		fingerprint, err = feed.ProbeSigning(ctx, repo)
		if err != nil {
			return fmt.Errorf("user.signingkey 已配置但无法签名：%w\n"+
				"  修好它，或清空该配置以明确地以「不签名」身份运行", err)
		}
	}

	store, err := feed.New(ctx, repo, pub)
	if err != nil {
		return err
	}

	// 3) 前端：内嵌同源 —— 零 CORS，零构建链
	files, err := fs.Sub(embedded, "web")
	if err != nil {
		return err
	}

	// 4) 多源与外部锚定（都可选）
	remotes := parseRemotes(os.Getenv("IMMULOG_REMOTES"))
	var publisher feed.Publisher
	if u := os.Getenv("IMMULOG_ANCHOR_URL"); u != "" {
		publisher = feed.HTTPPublisher{URL: u}
	}

	// 5) 组装
	hub := web.NewHub()
	srv := web.New(web.Config{
		Store:     store,
		Hub:       hub,
		Repo:      repo,
		Files:     files,
		Remotes:   remotes,
		Publisher: publisher,
	})
	srv.Watch(ctx,
		duration("IMMULOG_SYNC_INTERVAL", web.SyncInterval),
		duration("IMMULOG_ANCHOR_INTERVAL", web.AnchorInterval))

	// 不设 WriteTimeout：SSE 是长连接，会被它掐断
	s := &http.Server{
		Addr:              ":" + env("PORT", "8081"),
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.Shutdown(shut)
	}()

	log.Info("ImmuLog 就绪",
		"addr", s.Addr, "repo", dir, "feed", pub,
		"signed", store.Signed(), "remotes", len(remotes), "anchor", publisher != nil)
	if store.Signed() {
		log.Info("消息将使用密钥签名", "fingerprint", feed.ShortKey(fingerprint))
	} else {
		log.Warn("未配置 user.signingkey：消息不会被签名，身份可被冒名（见 DESIGN.md §4）")
	}
	if len(remotes) == 0 {
		log.Warn("未配置 IMMULOG_REMOTES：单节点模式，不会与任何对端同步")
	}
	if publisher == nil {
		log.Warn("未配置 IMMULOG_ANCHOR_URL：锚定只落在本机，不是真正的外部锚定")
	}

	if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// parseRemotes 解析 IMMULOG_REMOTES。
//
// 支持两种写法：`url`（自动命名）与 `name=url`。
// 名字只用于隔离区槽位，会经 FeedID 归一成 ref 安全的形式。
func parseRemotes(spec string) []feed.Remote {
	var out []feed.Remote
	seen := map[string]bool{}
	for i, item := range strings.Split(spec, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		name, url := "", item
		if n, u, ok := strings.Cut(item, "="); ok {
			name, url = strings.TrimSpace(n), strings.TrimSpace(u)
		}
		if url == "" {
			continue
		}
		if name == "" {
			name = "r" + strconv.Itoa(i)
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, feed.Remote{Name: name, URL: url})
	}
	return out
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func duration(k string, def time.Duration) time.Duration {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return def
	}
	return d
}
