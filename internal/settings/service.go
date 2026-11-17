// Package settings 只选择允许公开的站点字段，不能复用管理配置响应。
package settings

import (
	"context"
	"personal-website/backend/internal/store"
)

// PublicSite 与 OpenAPI 的公开站点 DTO 对应，不包含 generation 或仓库配置。
type PublicSite struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	Timezone    string `json:"timezone"`
}

// ReadPublic 从真实数据库读取单例设置，缺失数据由调用方作为未就绪处理。
func ReadPublic(ctx context.Context, db *store.Store) (PublicSite, error) {
	var s PublicSite
	// 公开投影明确列名，禁止 SELECT * 将私有管理字段带入响应。
	err := db.Read.QueryRowContext(ctx, `SELECT title,description,timezone FROM site_settings WHERE id=1`).Scan(&s.Title, &s.Description, &s.Timezone)
	return s, err
}
