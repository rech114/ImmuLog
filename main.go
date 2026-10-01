// Command immutalk 是一个把 Git 当作信任根的聊天服务。
//
// 这个文件只负责组装与生命周期 —— 所有真实逻辑都在 internal/ 里。
// 目录即架构约束：gitx 是唯一碰 os/exec 的包，web 是唯一碰 net/http 的包。
package main

import (
	"context"
	"embed"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"immutalk/internal/feed"
	"immutalk/internal/gitx"
	"immutalk/internal/web"
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
	dir := env("IMMUTALK_REPO", "./repoDB")
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
	pub := feed.FeedID(name + "\x00" + email + "\x00" + key)

	store, err := feed.New(ctx, repo, pub)
	if err != nil {
		return err
	}

	// 3) 前端：内嵌同源 —— 零 CORS，零构建链
	files, err := fs.Sub(embedded, "web")
	if err != nil {
		return err
	}

	// 4) 组装
	hub := web.NewHub()
	srv := web.New(store, hub, repo, files)
	go srv.Watch(ctx, web.GuardInterval)

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

	log.Info("Immutalk 就绪",
		"addr", s.Addr, "repo", dir, "feed", pub, "signed", store.Signed())
	if key == "" {
		log.Warn("未配置 user.signingkey：消息不会被签名，身份可被冒名（见 DESIGN.md §4）")
	}

	if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
