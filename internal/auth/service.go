// Package auth 实现唯一站主和服务端会话；不会向客户端暴露密码或 token 摘要。
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"personal-website/backend/internal/store"
)

// CookieName 使用 __Host- 前缀，要求 Secure、Path=/ 且不设置 Domain。
const CookieName = "__Host-site_session"

// AbsoluteTTL 为不可被会话活动延长的绝对期限。
const AbsoluteTTL = 24 * time.Hour

// IdleTTL 为距最近服务端活动记录的空闲期限。
const IdleTTL = 2 * time.Hour
const touchInterval = 5 * time.Minute

// ErrCredentials 对不存在的账号和错误密码返回相同业务错误。
var ErrCredentials = errors.New("用户名或密码错误")

// ErrRequired 表示请求没有有效会话，不透露历史 token 是否存在。
var ErrRequired = errors.New("请先登录")

// ErrExpired 表示已知会话到期或服务密钥轮换导致会话失效。
var ErrExpired = errors.New("会话已失效，请重新登录")

// ErrCSRF 表示当前会话的写请求校验失败。
var ErrCSRF = errors.New("请求校验失败，请刷新会话")

// Identity 是会话允许返回的站主身份，不包含密码或内部维护字段。
type Identity struct {
	ID       string `json:"id"`
	Username string `json:"username"`
}

// Session 按合同返回可再次取回的 CSRF token 和绝对过期时间。
type Session struct {
	User      Identity `json:"user"`
	CSRFToken string   `json:"csrf_token"`
	ExpiresAt string   `json:"expires_at"`
}

// Service 接受可替换时钟，以在真实数据库测试中验证过期边界而不使用长时间等待。
type Service struct {
	DB            *store.Store
	CSRFKey       []byte
	Now           func() time.Time
	dummyHash     string
	passwordSlots chan struct{}
}

// New 创建认证服务；不存在的账号也执行一次 Argon2id，减少用户名存在性时序差异。
func New(db *store.Store, key []byte) (*Service, error) {
	dummy, err := HashPassword("dummy-password-never-valid")
	if err != nil {
		return nil, err
	}
	return &Service{DB: db, CSRFKey: append([]byte(nil), key...), Now: time.Now, dummyHash: dummy, passwordSlots: make(chan struct{}, 2)}, nil
}

// InitAdmin 只允许创建唯一站主；重复执行绝不覆盖已有账号和密码。
func InitAdmin(ctx context.Context, db *store.Store, username, password string) error {
	username = strings.TrimSpace(username)
	if !utf8.ValidString(username) {
		return errors.New("用户名必须使用有效 UTF-8")
	}
	if n := utf8.RuneCountInString(username); n < 1 || n > 80 {
		return errors.New("用户名必须包含 1–80 个字符")
	}
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return err
	}
	now := time.Now().UTC().UnixMilli()
	return db.WithTx(ctx, func(tx *sql.Tx) error {
		// singleton 唯一约束是并发初始化的最终防线；不能使用 REPLACE 擦除原站主。
		_, err := tx.ExecContext(ctx, `INSERT INTO admin_users(id,username,password_hash,created_at,password_changed_at) VALUES(?,?,?,?,?)`, id.String(), username, hash, now, now)
		return err
	})
}

// ResetPassword 原子更新密码并删除该站主全部会话，任何失败都保留原来的完整状态。
func ResetPassword(ctx context.Context, db *store.Store, username, password string) error {
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	return db.WithTx(ctx, func(tx *sql.Tx) error {
		var id string
		// NOCASE 与登录查询一致；不存在的账号不执行任何修改。
		if err := tx.QueryRowContext(ctx, `SELECT id FROM admin_users WHERE username=?`, strings.TrimSpace(username)).Scan(&id); err != nil {
			return err
		}
		// 密码变更与会话失效在同一写事务内；登录提交前还会复核摘要，避免旧密码竞争生成新会话。
		if _, err := tx.ExecContext(ctx, `UPDATE admin_users SET password_hash=?,password_changed_at=? WHERE id=?`, hash, time.Now().UTC().UnixMilli(), id); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE admin_id=?`, id)
		return err
	})
}

// Login 验证后生成新的随机会话；旧浏览器会话被原子替换，不复用攻击者预置 token。
func (s *Service) Login(ctx context.Context, username, password, oldToken string) (Session, string, error) {
	// Argon2id 每次使用 64 MiB：实例最多两个并行校验，避免分散 IP 令内存无界增长。
	// 忙时返回可重试的 503，不在 HTTP 生命周期外排队或启动后台 goroutine。
	select {
	case s.passwordSlots <- struct{}{}:
		defer func() { <-s.passwordSlots }()
	default:
		return Session{}, "", errors.New("密码校验资源繁忙")
	}
	if err := ctx.Err(); err != nil {
		return Session{}, "", err
	}
	var id Identity
	var hash string
	// 仅读取认证必需字段；原始用户名和密码不写日志。
	err := s.DB.Read.QueryRowContext(ctx, `SELECT id,username,password_hash FROM admin_users WHERE username=?`, username).Scan(&id.ID, &id.Username, &hash)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Session{}, "", err
	}
	if errors.Is(err, sql.ErrNoRows) {
		verifyPassword(s.dummyHash, password)
		return Session{}, "", ErrCredentials
	}
	if !verifyPassword(hash, password) {
		return Session{}, "", ErrCredentials
	}
	tokenBytes := make([]byte, 32)
	if _, err = rand.Read(tokenBytes); err != nil {
		return Session{}, "", err
	}
	token := base64.RawURLEncoding.EncodeToString(tokenBytes)
	now := s.Now().UTC()
	expires := now.Add(AbsoluteTTL)
	csrf := s.csrf(token)
	th := sha256.Sum256([]byte(token))
	ch := sha256.Sum256([]byte(csrf))
	err = s.DB.WithTx(ctx, func(tx *sql.Tx) error {
		var current string
		// Argon2id 在事务外执行；提交前重读密码摘要，阻止并发改密后旧密码产生会话。
		if err := tx.QueryRowContext(ctx, `SELECT password_hash FROM admin_users WHERE id=?`, id.ID).Scan(&current); err != nil {
			return err
		}
		if subtle.ConstantTimeCompare([]byte(hash), []byte(current)) != 1 {
			return ErrCredentials
		}
		if oldToken != "" {
			oldHash := sha256.Sum256([]byte(oldToken))
			if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash=?`, oldHash[:]); err != nil {
				return err
			}
		}
		// 数据库只保存两种摘要；浏览器原 token 仅通过 HttpOnly Cookie 返回。
		_, err := tx.ExecContext(ctx, `INSERT INTO sessions(token_hash,admin_id,csrf_hash,created_at,last_seen_at,expires_at) VALUES(?,?,?,?,?,?)`, th[:], id.ID, ch[:], now.UnixMilli(), now.UnixMilli(), expires.UnixMilli())
		return err
	})
	if err != nil {
		return Session{}, "", err
	}
	return Session{User: id, CSRFToken: csrf, ExpiresAt: expires.Format(time.RFC3339)}, token, nil
}

// Current 校验绝对和空闲到期；CSRF 由固定会话 token 派生，多个标签页互不使其失效。
func (s *Service) Current(ctx context.Context, token string) (Session, error) {
	if !validToken(token) {
		return Session{}, ErrRequired
	}
	th := sha256.Sum256([]byte(token))
	var id Identity
	var seen, expiry int64
	var storedCSRF []byte
	// 会话与现存账号关联读取；改密和退出删除会话后不可继续通过该查询。
	err := s.DB.Read.QueryRowContext(ctx, `SELECT a.id,a.username,s.last_seen_at,s.expires_at,s.csrf_hash FROM sessions s JOIN admin_users a ON a.id=s.admin_id WHERE s.token_hash=?`, th[:]).Scan(&id.ID, &id.Username, &seen, &expiry, &storedCSRF)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrRequired
	}
	if err != nil {
		return Session{}, err
	}
	now := s.Now().UTC()
	if now.UnixMilli() >= expiry || now.UnixMilli()-seen >= IdleTTL.Milliseconds() {
		// 到期删除只针对该 token；失败不能反过来放行已过期会话。
		_, _ = s.DB.Write.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash=?`, th[:])
		return Session{}, ErrExpired
	}
	csrf := s.csrf(token)
	ch := sha256.Sum256([]byte(csrf))
	if subtle.ConstantTimeCompare(ch[:], storedCSRF) != 1 {
		return Session{}, ErrExpired
	}
	if now.UnixMilli()-seen >= touchInterval.Milliseconds() {
		// 每五分钟至多触碰一次；条件同时限制到期，防止读后等待锁时延长已失效会话。
		result, err := s.DB.Write.ExecContext(ctx, `UPDATE sessions SET last_seen_at=? WHERE token_hash=? AND last_seen_at<=? AND last_seen_at>? AND expires_at>?`, now.UnixMilli(), th[:], now.Add(-touchInterval).UnixMilli(), now.Add(-IdleTTL).UnixMilli(), now.UnixMilli())
		if err != nil {
			return Session{}, err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return Session{}, err
		}
		if n == 0 {
			// 同时读取可能由另一请求更新，也可能被退出/改密删除，必须重新确认而不是沿用旧身份。
			return s.Current(ctx, token)
		}
	}
	return Session{User: id, CSRFToken: csrf, ExpiresAt: time.UnixMilli(expiry).UTC().Format(time.RFC3339)}, nil
}

// CheckCSRF 只比较同一会话派生 token，错误不能通过刷新多标签状态绕过。
func (s *Service) CheckCSRF(token, csrf string) error {
	want := s.csrf(token)
	if !validToken(token) || subtle.ConstantTimeCompare([]byte(want), []byte(csrf)) != 1 {
		return ErrCSRF
	}
	return nil
}

// Logout 删除指定服务端会话；只有实际删除一行才表示退出成功。
func (s *Service) Logout(ctx context.Context, token string) error {
	h := sha256.Sum256([]byte(token))
	result, err := s.DB.Write.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash=?`, h[:])
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrRequired
	}
	return nil
}
func (s *Service) csrf(token string) string {
	h := hmac.New(sha256.New, s.CSRFKey)
	h.Write([]byte(token))
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}
func validToken(v string) bool {
	b, err := base64.RawURLEncoding.DecodeString(v)
	return err == nil && len(b) == 32 && len(v) == 43
}

// ErrorCode 统一业务错误，内部数据库错误由 HTTP 层再分类为繁忙或不可用。
func ErrorCode(err error) (int, string, string) {
	switch {
	case errors.Is(err, ErrCredentials):
		return 401, "INVALID_CREDENTIALS", ErrCredentials.Error()
	case errors.Is(err, ErrRequired):
		return 401, "AUTH_REQUIRED", ErrRequired.Error()
	case errors.Is(err, ErrExpired):
		return 401, "SESSION_EXPIRED", ErrExpired.Error()
	case errors.Is(err, ErrCSRF):
		return 403, "CSRF_REJECTED", ErrCSRF.Error()
	}
	if store.IsBusy(err) {
		return 503, "DATABASE_BUSY", "数据库繁忙，请稍后重试。"
	}
	return 503, "SERVICE_UNAVAILABLE", "服务暂不可用，请稍后重试。"
}

// ValidateLogin 按 OpenAPI 的 Unicode 字符上限检查凭据，登录不强制新密码策略以兼容已有账号。
func ValidateLogin(username, password string) error {
	if n := utf8.RuneCountInString(username); n < 1 || n > 80 {
		return fmt.Errorf("用户名长度必须为 1–80 字符")
	}
	if n := utf8.RuneCountInString(password); n < 1 || n > 256 {
		return fmt.Errorf("密码长度必须为 1–256 字符")
	}
	return nil
}
