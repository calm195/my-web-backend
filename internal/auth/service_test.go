package auth

import (
	"context"
	"crypto/sha256"
	"errors"
	"path/filepath"
	"personal-website/backend/internal/store"
	"sync"
	"testing"
	"time"
)

const testPassword = "test-password-only-1234"

func fixture(t *testing.T) (*Service, *store.Store) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "site.db"), true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err = db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err = InitAdmin(ctx, db, "owner", testPassword); err != nil {
		t.Fatal(err)
	}
	s, err := New(db, make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	return s, db
}

func TestSessionHashRotationCSRFAndLogout(t *testing.T) {
	s, db := fixture(t)
	ctx := context.Background()
	first, token, err := s.Login(ctx, "OWNER", testPassword, "")
	if err != nil {
		t.Fatal(err)
	}
	if first.User.Username != "owner" || !validToken(token) {
		t.Fatal("登录身份或 token 不正确")
	}
	var stored []byte
	// 数据库保存的是摘要而不是原始 Cookie；对应生成 CSRF 也不能是明文存储。
	if err = db.Read.QueryRowContext(ctx, "SELECT token_hash FROM sessions").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256([]byte(token))
	if string(stored) != string(want[:]) || string(stored) == token {
		t.Fatal("会话摘要不正确")
	}
	for i := 0; i < 2; i++ {
		current, err := s.Current(ctx, token)
		if err != nil || current.CSRFToken != first.CSRFToken {
			t.Fatal("多标签读取不应轮换 CSRF", err)
		}
	}
	if err = s.CheckCSRF(token, "fake"); !errors.Is(err, ErrCSRF) {
		t.Fatal(err)
	}
	if err = s.CheckCSRF(token, first.CSRFToken); err != nil {
		t.Fatal(err)
	}
	_, newToken, err := s.Login(ctx, "owner", testPassword, token)
	if err != nil {
		t.Fatal(err)
	}
	if newToken == token {
		t.Fatal("登录应轮换会话")
	}
	if _, err = s.Current(ctx, token); !errors.Is(err, ErrRequired) {
		t.Fatal("旧会话仍有效", err)
	}
	if err = s.Logout(ctx, newToken); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Current(ctx, newToken); !errors.Is(err, ErrRequired) {
		t.Fatal("退出后会话仍有效", err)
	}
}

func TestSessionIdleAbsoluteAndTouchBoundaries(t *testing.T) {
	s, db := fixture(t)
	ctx := context.Background()
	start := time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)
	now := start
	s.Now = func() time.Time { return now }
	_, token, err := s.Login(ctx, "owner", testPassword, "")
	if err != nil {
		t.Fatal(err)
	}
	now = start.Add(4 * time.Minute)
	if _, err = s.Current(ctx, token); err != nil {
		t.Fatal(err)
	}
	var seen int64
	// 小于五分钟不写 last_seen，避免每个 GET 引发写事务。
	if err = db.Read.QueryRowContext(ctx, "SELECT last_seen_at FROM sessions").Scan(&seen); err != nil || seen != start.UnixMilli() {
		t.Fatal("会话被过早触碰", err)
	}
	now = start.Add(5 * time.Minute)
	if _, err = s.Current(ctx, token); err != nil {
		t.Fatal(err)
	}
	now = start.Add(5*time.Minute + IdleTTL)
	if _, err = s.Current(ctx, token); !errors.Is(err, ErrExpired) {
		t.Fatal("空闲精确边界未到期", err)
	}
	now = start
	_, token, err = s.Login(ctx, "owner", testPassword, "")
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < 24; i++ {
		now = start.Add(time.Duration(i) * time.Hour)
		if _, err = s.Current(ctx, token); err != nil {
			t.Fatal(err)
		}
	}
	now = start.Add(AbsoluteTTL)
	if _, err = s.Current(ctx, token); !errors.Is(err, ErrExpired) {
		t.Fatal("绝对到期不能被持续活动延长", err)
	}
}

func TestResetInvalidatesEverySessionAndInitializationIsSafe(t *testing.T) {
	s, db := fixture(t)
	ctx := context.Background()
	_, a, err := s.Login(ctx, "owner", testPassword, "")
	if err != nil {
		t.Fatal(err)
	}
	_, b, err := s.Login(ctx, "owner", testPassword, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = InitAdmin(ctx, db, "another", testPassword); err == nil {
		t.Fatal("不能初始化第二站主")
	}
	if err = ResetPassword(ctx, db, "owner", "replacement-password-1234"); err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{a, b} {
		if _, err = s.Current(ctx, token); !errors.Is(err, ErrRequired) {
			t.Fatal("改密后旧会话仍有效", err)
		}
	}
	if _, _, err = s.Login(ctx, "owner", testPassword, ""); !errors.Is(err, ErrCredentials) {
		t.Fatal("旧密码仍有效", err)
	}
	if _, _, err = s.Login(ctx, "owner", "replacement-password-1234", ""); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentResetCannotRetainOldPasswordSession(t *testing.T) {
	s, db := fixture(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	start := make(chan struct{})
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_, _, err := s.Login(ctx, "owner", testPassword, "")
		if err != nil && !errors.Is(err, ErrCredentials) {
			t.Error(err)
		}
	}()
	go func() {
		defer wg.Done()
		<-start
		if err := ResetPassword(ctx, db, "owner", "replacement-password-1234"); err != nil {
			t.Error(err)
		}
	}()
	close(start)
	wg.Wait()
	var n int
	// 旧密码登录要么先提交后被删，要么在改密后复核失败，均不能留下有效会话。
	if err := db.Read.QueryRowContext(ctx, "SELECT count(*) FROM sessions").Scan(&n); err != nil || n != 0 {
		t.Fatalf("旧密码竞争遗留会话 %d %v", n, err)
	}
}

func TestLimiterConcurrentReservationsAndExpiry(t *testing.T) {
	l := NewLimiter([]byte("test-hmac-key"))
	now := time.Now()
	l.Now = func() time.Time { return now }
	var finishes []func(bool)
	for i := 0; i < 5; i++ {
		f, retry := l.Reserve("Owner", "192.0.2.1")
		if f == nil || retry != 0 {
			t.Fatal("意外限频")
		}
		finishes = append(finishes, f)
	}
	if f, _ := l.Reserve("OWNER", "192.0.2.2"); f != nil {
		t.Fatal("并发预留必须计入账号限制")
	}
	for _, f := range finishes {
		f(false)
		f(false)
	}
	if f, _ := l.Reserve("other", "192.0.2.1"); f != nil {
		t.Fatal("IP 限制必须跨账号生效")
	}
	now = now.Add(15 * time.Minute)
	f, retry := l.Reserve("Owner", "192.0.2.1")
	if f == nil || retry != 0 {
		t.Fatal("窗口到期应恢复")
	}
	f(true)
}
