// Command api 启动独立 Go 服务；schema 不匹配时拒绝运行，不执行自动迁移。
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"personal-website/backend/internal/config"
	"personal-website/backend/internal/httpapi"
	"personal-website/backend/internal/store"
	"syscall"
	"time"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("服务启动或运行失败，请检查私有配置和 schema")
		os.Exit(1)
	}
}
func run(log *slog.Logger) error {
	c, err := config.Load(true)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	db, err := store.Open(ctx, c.DatabasePath, false)
	if err != nil {
		return err
	}
	defer db.Close()
	if err = db.CheckSchema(ctx); err != nil {
		return err
	}
	if info, err := os.Stat(c.AssetDir); err != nil || !info.IsDir() {
		return errors.New("资源目录未准备")
	}
	handler, err := httpapi.New(c, db, log)
	if err != nil {
		return err
	}
	srv := &http.Server{Addr: c.Addr, Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 * 1024}
	done := make(chan error, 1)
	go func() { log.Info("API 服务启动", "addr", c.Addr); done <- srv.ListenAndServe() }()
	select {
	case err = <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdown)
	}
}
