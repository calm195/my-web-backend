// Package config 校验后端环境配置，密钥只保留在 Go 进程中。
package config

import (
	"encoding/hex"
	"errors"
	"net"
	"net/url"
	"os"
	"path/filepath"
)

// Config 保存服务配置；所有目录都属于后端私有运行区。
type Config struct {
	Addr, DatabasePath, AssetDir, PublicOrigin string
	CSRFKey, RateLimitKey                      []byte
}

// Load 按用途加载配置；运维 CLI 无需 HTTP 会话密钥即可迁移、初始化和改密。
func Load(server bool) (Config, error) {
	c := Config{Addr: value("SITE_ADDR", "127.0.0.1:8080"), DatabasePath: os.Getenv("SITE_DATABASE_PATH"), AssetDir: os.Getenv("SITE_ASSET_DIR"), PublicOrigin: value("SITE_PUBLIC_ORIGIN", "https://localhost:8443")}
	if c.DatabasePath == "" || !filepath.IsAbs(c.DatabasePath) {
		return c, errors.New("SITE_DATABASE_PATH 必须为绝对路径")
	}
	if c.AssetDir == "" || !filepath.IsAbs(c.AssetDir) {
		return c, errors.New("SITE_ASSET_DIR 必须为绝对路径")
	}
	u, err := url.Parse(c.PublicOrigin)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return c, errors.New("SITE_PUBLIC_ORIGIN 必须为无路径的 HTTPS 来源")
	}
	host, _, err := net.SplitHostPort(c.Addr)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		return c, errors.New("SITE_ADDR 必须监听明确的回环地址")
	}
	if server {
		c.CSRFKey, err = key("SITE_CSRF_KEY")
		if err != nil {
			return c, err
		}
		c.RateLimitKey, err = key("SITE_RATE_LIMIT_KEY")
		if err != nil {
			return c, err
		}
	}
	return c, nil
}

func value(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}
func key(name string) ([]byte, error) {
	b, err := hex.DecodeString(os.Getenv(name))
	if err != nil || len(b) != 32 {
		return nil, errors.New(name + " 必须为 32 字节随机密钥的 64 位十六进制编码")
	}
	return b, nil
}
