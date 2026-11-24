package main

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 面板登录：只有一个密码（`vastmon passwd` 或设置页修改），哈希存在 settings 里；
// 除了登录 / 退出 / 查登录状态，所有接口都要登录（guardAPI）。
// 会话令牌只存它的 sha256，cookie 是 HttpOnly + SameSite=Strict。
// 所有写请求还要带 X-Requested-With: vastmon 头，挡住跨站表单提交。

const (
	sessionCookie = "vastmon_session"
	sessionTTL    = 7 * 24 * time.Hour
	pbkdf2Iter    = 600000
	failWindow    = 15 * time.Minute
	failPerIP     = 5
	failGlobal    = 30
	csrfHeader    = "X-Requested-With"
	csrfValue     = "vastmon"
)

type Auth struct {
	st     *Store
	mu     sync.Mutex
	fails  map[string][]time.Time
	global []time.Time
}

func newAuth(st *Store) *Auth {
	return &Auth{st: st, fails: map[string][]time.Time{}}
}

func hashPassword(pw string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, pw, salt, pbkdf2Iter, 32)
	if err != nil {
		return "", err
	}
	enc := base64.RawStdEncoding
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", pbkdf2Iter, enc.EncodeToString(salt), enc.EncodeToString(key)), nil
}

func checkPassword(stored, pw string) bool {
	parts := strings.Split(stored, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter < 1 {
		return false
	}
	enc := base64.RawStdEncoding
	salt, err1 := enc.DecodeString(parts[2])
	want, err2 := enc.DecodeString(parts[3])
	if err1 != nil || err2 != nil {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, pw, salt, iter, len(want))
	return err == nil && subtle.ConstantTimeCompare(got, want) == 1
}

func (a *Auth) passwordHash() (string, error) {
	var h string
	_, err := a.st.Get("password_hash", &h)
	return h, err
}

// SetPassword 换密码并让所有已登录的会话失效。
func (a *Auth) SetPassword(pw string) error {
	h, err := hashPassword(pw)
	if err != nil {
		return err
	}
	if err := a.st.Set("password_hash", h); err != nil {
		return err
	}
	_, err = a.st.db.Exec(`DELETE FROM sessions`)
	return err
}

func tokenHash(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

func (a *Auth) LoggedIn(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return false
	}
	var created int64
	err = a.st.db.QueryRow(`SELECT created FROM sessions WHERE token_hash = ?`, tokenHash(c.Value)).Scan(&created)
	return err == nil && time.Since(time.Unix(created, 0)) < sessionTTL
}

// cloudflareNets 是 Cloudflare 公布的回源 IP 段（https://www.cloudflare.com/ips-v4、ips-v6，2026-11-24）。
var cloudflareNets = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range strings.Fields(`173.245.48.0/20 103.21.244.0/22 103.22.200.0/22 103.31.4.0/22 141.101.64.0/18
		108.162.192.0/18 190.93.240.0/20 188.114.96.0/20 197.234.240.0/22 198.41.128.0/17 162.158.0.0/15
		104.16.0.0/13 104.24.0.0/14 172.64.0.0/13 131.0.72.0/22
		2400:cb00::/32 2606:4700::/32 2803:f800::/32 2405:b500::/32 2405:8100::/32 2a06:98c0::/29 2c0f:f248::/32`) {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

// clientIP 是访问者的 IP。面板套在 Cloudflare 后面时，只有请求确实来自 Cloudflare
// 的 IP 段才信 CF-Connecting-IP（直连 8090 时伪造这个头没用）。
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		addr = addr.Unmap()
		for _, p := range cloudflareNets {
			if p.Contains(addr) {
				if cf, err := netip.ParseAddr(strings.TrimSpace(r.Header.Get("CF-Connecting-IP"))); err == nil {
					return cf.String()
				}
				break
			}
		}
	}
	return host
}

func prune(ts []time.Time, now time.Time) []time.Time {
	out := ts[:0]
	for _, t := range ts {
		if now.Sub(t) < failWindow {
			out = append(out, t)
		}
	}
	return out
}

// limited 判断这个 IP 现在是否还能尝试登录。
func (a *Auth) limited(ip string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	a.fails[ip] = prune(a.fails[ip], now)
	a.global = prune(a.global, now)
	return len(a.fails[ip]) >= failPerIP || len(a.global) >= failGlobal
}

func (a *Auth) recordFail(ip string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	a.fails[ip] = append(a.fails[ip], now)
	a.global = append(a.global, now)
}

func (a *Auth) session(w http.ResponseWriter, r *http.Request) {
	h, err := a.passwordHash()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}
	writeJSON(w, map[string]bool{"logged_in": a.LoggedIn(r), "password_set": h != ""})
}

func (a *Auth) login(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if a.limited(ip) {
		writeError(w, http.StatusTooManyRequests, "RATE_LIMITED", "登录失败次数太多，请 15 分钟后再试")
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "请求格式不对")
		return
	}
	stored, err := a.passwordHash()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}
	if stored == "" {
		writeError(w, http.StatusForbidden, "NO_PASSWORD", "还没有设置密码：在服务器上运行 ./vastmon passwd")
		return
	}
	if !checkPassword(stored, body.Password) {
		a.recordFail(ip)
		writeError(w, http.StatusUnauthorized, "BAD_PASSWORD", "密码不对")
		return
	}
	if err := a.newSession(w); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}
	writeJSON(w, map[string]bool{"logged_in": true, "password_set": true})
}

// newSession 建一个会话并发 cookie。
func (a *Auth) newSession(w http.ResponseWriter) error {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return err
	}
	tok := hex.EncodeToString(raw)
	now := time.Now()
	if _, err := a.st.db.Exec(`DELETE FROM sessions WHERE created < ?`, now.Add(-sessionTTL).Unix()); err != nil {
		return err
	}
	if _, err := a.st.db.Exec(`INSERT INTO sessions (token_hash, created) VALUES (?, ?)`, tokenHash(tok), now.Unix()); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: tok, Path: "/", MaxAge: int(sessionTTL.Seconds()),
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
	return nil
}

// changePassword 在设置页改密码：要输对当前密码（错了和登录一样计失败次数）。
// 改完其它地方的登录全部失效，当前浏览器换一个新会话继续登录着。
func (a *Auth) changePassword(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if a.limited(ip) {
		writeError(w, http.StatusTooManyRequests, "RATE_LIMITED", "密码输错次数太多，请 15 分钟后再试")
		return
	}
	var body struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "请求格式不对")
		return
	}
	stored, err := a.passwordHash()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}
	if !checkPassword(stored, body.Current) {
		a.recordFail(ip)
		writeError(w, http.StatusBadRequest, "BAD_PASSWORD", "当前密码不对")
		return
	}
	if n := len([]rune(body.New)); n < 8 || n > 128 {
		writeError(w, http.StatusBadRequest, "WEAK_PASSWORD", "新密码要 8–128 个字符")
		return
	}
	if body.New == body.Current {
		writeError(w, http.StatusBadRequest, "SAME_PASSWORD", "新密码和当前密码一样")
		return
	}
	if err := a.SetPassword(body.New); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}
	if err := a.newSession(w); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}
	writeJSON(w, map[string]bool{"logged_in": true, "password_set": true})
}

func (a *Auth) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		if _, err := a.st.db.Exec(`DELETE FROM sessions WHERE token_hash = ?`, tokenHash(c.Value)); err != nil {
			writeError(w, http.StatusInternalServerError, "INTERNAL", err.Error())
			return
		}
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	writeJSON(w, map[string]bool{"logged_in": false, "password_set": true})
}

// require 包装只有登录后才能用的接口。
func (a *Auth) require(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.LoggedIn(r) {
			writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "需要先登录")
			return
		}
		next(w, r)
	}
}

// publicAPI 是不登录也能用的接口，其它 /api/ 接口都要先登录。
var publicAPI = map[string]bool{"/api/session": true, "/api/login": true, "/api/logout": true}

// guardAPI 让面板的所有数据都要登录才能看（新加的接口默认也受保护）；
// 静态文件（登录页要用）不拦。
func (a *Auth) guardAPI(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") && !publicAPI[r.URL.Path] && !a.LoggedIn(r) {
			writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "需要先登录")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// csrf 拒绝没有带 X-Requested-With: vastmon 的写请求。
func csrf(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead && strings.HasPrefix(r.URL.Path, "/api/") &&
			r.Header.Get(csrfHeader) != csrfValue {
			writeError(w, http.StatusForbidden, "FORBIDDEN", "请求缺少 "+csrfHeader+" 头")
			return
		}
		next.ServeHTTP(w, r)
	})
}
