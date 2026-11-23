// Package httpapi 将 HTTP 请求转换为应用服务调用，权限校验始终在 Go 完成。
package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"personal-website/backend/internal/auth"
	"personal-website/backend/internal/config"
	"personal-website/backend/internal/settings"
	"personal-website/backend/internal/store"
)

type fieldError struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}
type errorBody struct {
	Code        string       `json:"code"`
	Message     string       `json:"message"`
	FieldErrors []fieldError `json:"field_errors"`
	RequestID   string       `json:"request_id"`
}

// Server 持有实例级服务和限频状态；测试使用独立实例，不共享全局会话或数据库。
type Server struct {
	cfg     config.Config
	db      *store.Store
	auth    *auth.Service
	limiter *auth.Limiter
	log     *slog.Logger
}

// New 只构造 HTTP 处理器，不绑定端口；返回值同时适用于真实服务与 httptest。
func New(c config.Config, db *store.Store, log *slog.Logger) (*Server, error) {
	a, err := auth.New(db, c.CSRFKey)
	if err != nil {
		return nil, err
	}
	return &Server{cfg: c, db: db, auth: a, limiter: auth.NewLimiter(c.RateLimitKey), log: log}, nil
}

// ServeHTTP 对每个请求重新执行认证与来源检查，不信任前端路由守卫。
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	id, err := uuid.NewV7()
	requestID := "request-id-unavailable"
	if err == nil {
		requestID = id.String()
	}
	w.Header().Set("X-Request-ID", requestID)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	rw := &responseWriter{ResponseWriter: w, status: 200}
	defer func() {
		if recover() != nil {
			s.fail(rw, 503, "SERVICE_UNAVAILABLE", "服务暂不可用，请稍后重试。", nil)
		}
		if s.log != nil {
			s.log.Info("HTTP 请求完成", "request_id", requestID, "method", r.Method, "status", rw.status, "duration_ms", time.Since(start).Milliseconds())
		}
	}()
	if err != nil {
		s.fail(rw, 503, "SERVICE_UNAVAILABLE", "服务暂不可用。", nil)
		return
	}
	if r.URL.RawQuery != "" {
		s.fail(rw, 400, "INVALID_REQUEST", "此接口不接受查询参数。", nil)
		return
	}
	if r.Method == http.MethodGet && (r.URL.Path == "/healthz" || r.URL.Path == "/readyz") {
		if r.URL.Path == "/readyz" {
			if err := s.ready(r); err != nil {
				s.fail(rw, 503, "SERVICE_UNAVAILABLE", "服务尚未就绪。", nil)
				return
			}
		}
		writeJSON(rw, 200, map[string]string{"status": "ok"})
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/api/v1/public/site" {
		site, err := settings.ReadPublic(r.Context(), s.db)
		if err != nil {
			s.serviceError(rw, err)
			return
		}
		writeJSON(rw, 200, site)
		return
	}
	if r.URL.Path == "/api/v1/admin/session" && r.Method == http.MethodPost {
		s.login(rw, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/v1/admin/") {
		// 管理路由不存在也先鉴权，禁止借未实现路由绕过统一权限边界。
		token := cookieToken(r)
		session, err := s.auth.Current(r.Context(), token)
		if err != nil {
			s.serviceError(rw, err)
			return
		}
		// 安全方法只读取数据；所有其他管理请求都需同时验证来源和当前会话的 CSRF token。
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
			if !s.originOK(r) {
				s.fail(rw, 403, "ORIGIN_REJECTED", "请求来源不受信任。", nil)
				return
			}
			if err := s.auth.CheckCSRF(token, r.Header.Get("X-CSRF-Token")); err != nil {
				s.serviceError(rw, err)
				return
			}
		}
		if r.URL.Path == "/api/v1/admin/session" {
			switch r.Method {
			case http.MethodGet:
				writeJSON(rw, 200, session)
				return
			case http.MethodDelete:
				if err := s.auth.Logout(r.Context(), token); err != nil {
					s.serviceError(rw, err)
					return
				}
				http.SetCookie(rw, &http.Cookie{Name: auth.CookieName, Value: "", Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: -1, Expires: time.Unix(1, 0).UTC()})
				rw.WriteHeader(204)
				return
			}
		}
	}
	s.fail(rw, 404, "NOT_FOUND", "页面或资源不存在。", nil)
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if !s.originOK(r) {
		s.fail(w, 403, "ORIGIN_REJECTED", "请求来源不受信任。", nil)
		return
	}
	typ, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || typ != "application/json" || (params["charset"] != "" && !strings.EqualFold(params["charset"], "utf-8")) {
		s.fail(w, 415, "UNSUPPORTED_MEDIA_TYPE", "请使用 UTF-8 JSON。", nil)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 2*1024*1024)
	defer r.Body.Close()
	// 有界读取后显式验证 UTF-8，避免 encoding/json 默默替换非法字节改变凭据。
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var max *http.MaxBytesError
		if errors.As(err, &max) {
			s.fail(w, 413, "PAYLOAD_TOO_LARGE", "请求体超过 2 MiB。", nil)
		} else {
			s.fail(w, 400, "INVALID_REQUEST", "无法读取请求。", nil)
		}
		return
	}
	if !utf8.Valid(raw) {
		s.fail(w, 400, "INVALID_REQUEST", "JSON 必须使用有效 UTF-8。", nil)
		return
	}
	body, err := decodeLogin(raw)
	if err != nil {
		s.fail(w, 400, "INVALID_REQUEST", "请求 JSON 格式或字段不正确。", nil)
		return
	}
	if err := auth.ValidateLogin(body.Username, body.Password); err != nil {
		s.fail(w, 422, "VALIDATION_FAILED", err.Error(), []fieldError{{Path: "/", Message: err.Error()}})
		return
	}
	finish, retry := s.limiter.Reserve(body.Username, clientIP(r))
	if finish == nil {
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(retry.Seconds()))))
		s.fail(w, 429, "RATE_LIMITED", "登录尝试过于频繁，请稍后重试。", nil)
		return
	}
	success := false
	defer func() { finish(success) }()
	session, token, err := s.auth.Login(r.Context(), body.Username, body.Password, cookieToken(r))
	if err != nil {
		// 仅凭据错误累计失败；数据库故障、请求取消或资源繁忙不能把站主锁定十五分钟。
		success = !errors.Is(err, auth.ErrCredentials)
		s.serviceError(w, err)
		return
	}
	success = true
	expires, _ := time.Parse(time.RFC3339, session.ExpiresAt)
	http.SetCookie(w, &http.Cookie{Name: auth.CookieName, Value: token, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, Expires: expires, MaxAge: int(auth.AbsoluteTTL.Seconds())})
	writeJSON(w, 200, session)
}

type loginBody struct {
	Username string
	Password string
}

// decodeLogin 按合同精确匹配字段，拒绝大小写别名和重复键，避免解码器静默覆盖凭据。
// encoding/json 的 struct 匹配默认忽略大小写，即使 DisallowUnknownFields 也不能表达该约束。
func decodeLogin(raw []byte) (loginBody, error) {
	var body loginBody
	dec := json.NewDecoder(bytes.NewReader(raw))
	start, err := dec.Token()
	if err != nil || start != json.Delim('{') {
		return body, errors.New("登录请求必须是 JSON 对象")
	}
	seen := make(map[string]bool, 2)
	for dec.More() {
		token, err := dec.Token()
		if err != nil {
			return body, err
		}
		key, ok := token.(string)
		if !ok || (key != "username" && key != "password") || seen[key] {
			return body, errors.New("登录请求含未知或重复字段")
		}
		seen[key] = true
		// 指针同时区分合法空字符串和 JSON null；空字符串继续交给业务长度校验返回 422。
		var value *string
		if err := dec.Decode(&value); err != nil {
			return body, err
		}
		if value == nil {
			return body, errors.New("登录字段必须是字符串")
		}
		if key == "username" {
			body.Username = *value
		} else {
			body.Password = *value
		}
	}
	end, err := dec.Token()
	if err != nil || end != json.Delim('}') || len(seen) != 2 {
		return body, errors.New("登录请求缺少字段或对象未闭合")
	}
	var extra json.RawMessage
	if dec.Decode(&extra) != io.EOF {
		return body, errors.New("登录请求包含尾随 JSON")
	}
	return body, nil
}

func (s *Server) originOK(r *http.Request) bool {
	// Origin 一旦存在就必须精确匹配，不允许错误 Origin 被正确 Referer 覆盖。
	if origins := r.Header.Values("Origin"); len(origins) > 0 {
		return len(origins) == 1 && origins[0] == s.cfg.PublicOrigin
	}
	u, err := url.Parse(r.Referer())
	return err == nil && u.User == nil && u.Scheme+"://"+u.Host == s.cfg.PublicOrigin
}
func (s *Server) ready(r *http.Request) error {
	if err := s.db.CheckSchema(r.Context()); err != nil {
		return err
	}
	info, err := os.Stat(s.cfg.AssetDir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("资源目录不可用")
	}
	_, err = settings.ReadPublic(r.Context(), s.db)
	return err
}
func (s *Server) serviceError(w http.ResponseWriter, err error) {
	status, code, message := auth.ErrorCode(err)
	s.fail(w, status, code, message, nil)
}
func (s *Server) fail(w http.ResponseWriter, status int, code, message string, fields []fieldError) {
	if fields == nil {
		fields = []fieldError{}
	}
	writeJSON(w, status, errorBody{Code: code, Message: message, FieldErrors: fields, RequestID: w.Header().Get("X-Request-ID")})
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func cookieToken(r *http.Request) string {
	c, err := r.Cookie(auth.CookieName)
	if err != nil {
		return ""
	}
	return c.Value
}
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return "unknown"
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return "unknown"
	}
	// 只有回环反代可提供转发地址；代理必须覆盖访客自报 X-Forwarded-For。
	if addr.IsLoopback() {
		forward := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-For"), ",")[0])
		if ip, err := netip.ParseAddr(forward); err == nil {
			return ip.Unmap().String()
		}
	}
	return addr.Unmap().String()
}

type responseWriter struct {
	http.ResponseWriter
	status  int
	written bool
}

func (w *responseWriter) WriteHeader(status int) {
	if w.written {
		return
	}
	w.status = status
	w.written = true
	w.ResponseWriter.WriteHeader(status)
}
func (w *responseWriter) Write(p []byte) (int, error) {
	if !w.written {
		w.WriteHeader(200)
	}
	return w.ResponseWriter.Write(p)
}
