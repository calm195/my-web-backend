package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"personal-website/backend/internal/auth"
	"personal-website/backend/internal/config"
	"personal-website/backend/internal/store"
	"strings"
	"testing"
	"time"
)

const password = "test-password-only-1234"

func serverFixture(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(dir, "site.db"), true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err = db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err = auth.InitAdmin(ctx, db, "owner", password); err != nil {
		t.Fatal(err)
	}
	assets := filepath.Join(dir, "assets")
	if err = os.Mkdir(assets, 0700); err != nil {
		t.Fatal(err)
	}
	s, err := New(config.Config{PublicOrigin: "https://localhost:8443", AssetDir: assets, CSRFKey: make([]byte, 32), RateLimitKey: make([]byte, 32)}, db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return s, db
}
func perform(s *Server, method, path, body string, cookie *http.Cookie, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "https://localhost:8443"+path, strings.NewReader(body))
	r.RemoteAddr = "192.0.2.1:12345"
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	if cookie != nil {
		r.AddCookie(cookie)
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}
func loginFixture(t *testing.T, s *Server) (auth.Session, *http.Cookie) {
	t.Helper()
	w := perform(s, "POST", "/api/v1/admin/session", `{"username":"owner","password":"`+password+`"}`, nil, map[string]string{"Origin": s.cfg.PublicOrigin})
	if w.Code != 200 {
		t.Fatalf("登录失败 %d %s", w.Code, w.Body.String())
	}
	var session auth.Session
	if err := json.Unmarshal(w.Body.Bytes(), &session); err != nil {
		t.Fatal(err)
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatal("没有唯一 Cookie")
	}
	return session, cookies[0]
}

func TestHTTPLoginMultiTabCSRFAndLogout(t *testing.T) {
	s, _ := serverFixture(t)
	session, cookie := loginFixture(t, s)
	if cookie.Name != auth.CookieName || cookie.Domain != "" || cookie.Path != "/" || !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatal("Cookie 安全属性错误")
	}
	for i := 0; i < 2; i++ {
		w := perform(s, "GET", "/api/v1/admin/session", "", cookie, nil)
		if w.Code != 200 {
			t.Fatal(w.Code)
		}
		var current auth.Session
		json.Unmarshal(w.Body.Bytes(), &current)
		if current.CSRFToken != session.CSRFToken {
			t.Fatal("刷新轮换了 CSRF")
		}
		if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("X-Request-ID") == "" {
			t.Fatal("响应缺少隐私/追踪头")
		}
	}
	for _, headers := range []map[string]string{{"Origin": s.cfg.PublicOrigin}, {"Origin": "https://evil.example", "X-CSRF-Token": session.CSRFToken}, {"Origin": s.cfg.PublicOrigin, "X-CSRF-Token": "fake"}} {
		w := perform(s, "DELETE", "/api/v1/admin/session", "", cookie, headers)
		if w.Code != 403 {
			t.Fatalf("错误校验应拒绝：%d", w.Code)
		}
	}
	w := perform(s, "DELETE", "/api/v1/admin/session", "", cookie, map[string]string{"Origin": s.cfg.PublicOrigin, "X-CSRF-Token": session.CSRFToken})
	if w.Code != 204 || w.Body.Len() != 0 || len(w.Result().Cookies()) != 1 || w.Result().Cookies()[0].MaxAge != -1 {
		t.Fatal("退出响应错误", w.Code)
	}
	if w = perform(s, "GET", "/api/v1/admin/session", "", cookie, nil); w.Code != 401 {
		t.Fatal("退出后的旧 Cookie 仍有效")
	}
}

func TestHTTPStrictInputAndOrigin(t *testing.T) {
	s, _ := serverFixture(t)
	valid := `{"username":"owner","password":"` + password + `"}`
	cases := []struct {
		name, body string
		headers    map[string]string
		status     int
	}{
		{"missing-origin", valid, nil, 403}, {"evil-origin", valid, map[string]string{"Origin": "https://evil.example", "Referer": s.cfg.PublicOrigin + "/admin/"}, 403},
		{"unknown-field", `{"username":"owner","password":"x","owner":true}`, map[string]string{"Origin": s.cfg.PublicOrigin}, 400},
		{"uppercase-fields", `{"Username":"owner","Password":"` + password + `"}`, map[string]string{"Origin": s.cfg.PublicOrigin}, 400},
		{"uppercase-alias", `{"username":"owner","PASSWORD":"` + password + `"}`, map[string]string{"Origin": s.cfg.PublicOrigin}, 400},
		{"duplicate-username", `{"username":"other","username":"owner","password":"` + password + `"}`, map[string]string{"Origin": s.cfg.PublicOrigin}, 400},
		{"duplicate-password", `{"username":"owner","password":"wrong","password":"` + password + `"}`, map[string]string{"Origin": s.cfg.PublicOrigin}, 400},
		{"escaped-duplicate", `{"username":"other","user\u006eame":"owner","password":"` + password + `"}`, map[string]string{"Origin": s.cfg.PublicOrigin}, 400},
		{"trailing-json", valid + ` {}`, map[string]string{"Origin": s.cfg.PublicOrigin}, 400},
		{"non-object", `[]`, map[string]string{"Origin": s.cfg.PublicOrigin}, 400},
		{"null-object", `null`, map[string]string{"Origin": s.cfg.PublicOrigin}, 400},
		{"missing-field", `{"username":"owner"}`, map[string]string{"Origin": s.cfg.PublicOrigin}, 400},
		{"null-field", `{"username":null,"password":"x"}`, map[string]string{"Origin": s.cfg.PublicOrigin}, 400},
		{"wrong-type", `{"username":"owner","password":123}`, map[string]string{"Origin": s.cfg.PublicOrigin}, 400},
		{"empty", `{"username":"","password":"x"}`, map[string]string{"Origin": s.cfg.PublicOrigin}, 422},
		{"unicode-limit", `{"username":"` + strings.Repeat("站", 81) + `","password":"x"}`, map[string]string{"Origin": s.cfg.PublicOrigin}, 422},
		{"bad-media", valid, map[string]string{"Origin": s.cfg.PublicOrigin, "Content-Type": "text/plain"}, 415},
		{"too-large", `{"username":"owner","password":"` + strings.Repeat("x", 2*1024*1024) + `"}`, map[string]string{"Origin": s.cfg.PublicOrigin}, 413},
		{"bad-utf8", string([]byte{123, 34, 120, 34, 58, 34, 255, 34, 125}), map[string]string{"Origin": s.cfg.PublicOrigin}, 400},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := perform(s, "POST", "/api/v1/admin/session", c.body, nil, c.headers)
			if w.Code != c.status {
				t.Fatalf("want %d got %d", c.status, w.Code)
			}
			var e errorBody
			if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil || e.Code == "" || e.RequestID == "" || e.FieldErrors == nil {
				t.Fatal("错误 DTO 不符合合同", err)
			}
		})
	}
	w := perform(s, "POST", "/api/v1/admin/session", valid, nil, map[string]string{"Referer": s.cfg.PublicOrigin + "/admin/login"})
	if w.Code != 200 {
		t.Fatal("合法 Referer fallback 不可用", w.Code)
	}
}

func TestHTTPLoginFailuresRateLimitedWithoutAccountDisclosure(t *testing.T) {
	s, _ := serverFixture(t)
	for i := 0; i < 5; i++ {
		w := perform(s, "POST", "/api/v1/admin/session", `{"username":"unknown","password":"wrong"}`, nil, map[string]string{"Origin": s.cfg.PublicOrigin})
		if w.Code != 401 {
			t.Fatal(w.Code)
		}
	}
	w := perform(s, "POST", "/api/v1/admin/session", `{"username":"owner","password":"`+password+`"}`, nil, map[string]string{"Origin": s.cfg.PublicOrigin})
	if w.Code != 429 || w.Header().Get("Retry-After") == "" {
		t.Fatal("失败限频应跨账号限制 IP", w.Code)
	}
}

func TestHTTPPrivateRoutesAndPublicProjection(t *testing.T) {
	s, _ := serverFixture(t)
	if w := perform(s, "GET", "/api/v1/admin/not-implemented", "", nil, nil); w.Code != 401 {
		t.Fatal("管理未知路由未鉴权")
	}
	_, cookie := loginFixture(t, s)
	if w := perform(s, "GET", "/api/v1/admin/not-implemented", "", cookie, nil); w.Code != 404 {
		t.Fatal("已认证未知路由状态错误")
	}
	w := perform(s, "GET", "/api/v1/public/site", "", cookie, nil)
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	var obj map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &obj); err != nil || len(obj) != 3 || obj["title"] != "一隅" || obj["timezone"] != "Asia/Shanghai" {
		t.Fatal("公开投影不正确", err)
	}
	if w := perform(s, "GET", "/api/v1/public/site?unexpected=1", "", nil, nil); w.Code != 400 {
		t.Fatal("未知查询参数未拒绝")
	}
	if w := perform(s, "GET", "/readyz", "", nil, nil); w.Code != 200 {
		t.Fatal("准备完成却未就绪")
	}
	if err := os.Remove(s.cfg.AssetDir); err != nil {
		t.Fatal(err)
	}
	if w := perform(s, "GET", "/readyz", "", nil, nil); w.Code != 503 {
		t.Fatal("资源目录丢失仍宣告就绪")
	}
	if w := perform(s, "GET", "/healthz", "", nil, nil); w.Code != 200 {
		t.Fatal("存活和就绪必须分开")
	}
}

func TestHTTPExpiredSession(t *testing.T) {
	s, _ := serverFixture(t)
	now := time.Now()
	s.auth.Now = func() time.Time { return now }
	_, cookie := loginFixture(t, s)
	now = now.Add(auth.IdleTTL)
	w := perform(s, "GET", "/api/v1/admin/session", "", cookie, nil)
	if w.Code != 401 || !strings.Contains(w.Body.String(), "SESSION_EXPIRED") {
		t.Fatal("过期会话错误状态", w.Code)
	}
}
