package manager

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"mihomo-proxy/internal/importer"
)

type Server struct {
	Manager  *Manager
	AdminKey string
	mu       sync.Mutex
	sessions map[string]time.Time
}

func NewServer(m *Manager, key string) *Server {
	return &Server{Manager: m, AdminKey: key, sessions: map[string]time.Time{}}
}

func respond(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(value)
}
func problem(w http.ResponseWriter, status int, err error) {
	respond(w, status, map[string]string{"error": err.Error()})
}
func readJSON(w http.ResponseWriter, r *http.Request, value any) error {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		return errors.New("请求必须使用 application/json")
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<20)
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(value); err != nil {
		return errors.New("请求 JSON 无效或超过 4 MiB")
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("请求只能包含一个 JSON 值")
	}
	return nil
}

func (s *Server) authenticated(r *http.Request) bool {
	cookie, err := r.Cookie("manager_session")
	if err != nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	expires, ok := s.sessions[cookie.Value]
	return ok && time.Now().Before(expires)
}

func (s *Server) Handler(assets http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		_, err := s.Manager.Kernel.Version(ctx)
		if err != nil {
			problem(w, 503, errors.New("内核未就绪"))
			return
		}
		respond(w, 200, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /api/auth", func(w http.ResponseWriter, r *http.Request) {
		respond(w, 200, map[string]bool{"authenticated": s.authenticated(r)})
	})
	mux.HandleFunc("POST /api/login", s.login)
	mux.HandleFunc("POST /api/logout", func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie("manager_session"); err == nil {
			s.mu.Lock()
			delete(s.sessions, c.Value)
			s.mu.Unlock()
		}
		http.SetCookie(w, &http.Cookie{Name: "manager_session", Value: "", Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil, MaxAge: -1})
		respond(w, 200, map[string]bool{"ok": true})
	})
	mux.HandleFunc("GET /api/state", s.state)
	mux.HandleFunc("POST /api/apply", func(w http.ResponseWriter, r *http.Request) { respond(w, 200, s.Manager.Apply(r.Context())) })
	mux.HandleFunc("POST /api/listeners", s.saveListener)
	mux.HandleFunc("PUT /api/listeners/{id}", s.saveListener)
	mux.HandleFunc("DELETE /api/listeners/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.change(w, r, func() error { return s.Manager.Store.DeleteListener(r.Context(), r.PathValue("id")) })
	})
	mux.HandleFunc("PATCH /api/nodes/{id}", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Enabled bool `json:"enabled"`
		}
		if err := readJSON(w, r, &req); err != nil {
			problem(w, 400, err)
			return
		}
		s.change(w, r, func() error { return s.Manager.Store.SetNodeEnabled(r.Context(), r.PathValue("id"), req.Enabled) })
	})
	mux.HandleFunc("DELETE /api/nodes/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.change(w, r, func() error { return s.Manager.Store.DeleteNode(r.Context(), r.PathValue("id")) })
	})
	mux.HandleFunc("POST /api/nodes/{id}/check", s.checkNode)
	mux.HandleFunc("POST /api/import", s.importNodes)
	mux.HandleFunc("POST /api/subscriptions", s.saveSubscription)
	mux.HandleFunc("PUT /api/subscriptions/{id}", s.saveSubscription)
	mux.HandleFunc("DELETE /api/subscriptions/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.change(w, r, func() error { return s.Manager.Store.DeleteSubscription(r.Context(), r.PathValue("id")) })
	})
	mux.HandleFunc("POST /api/subscriptions/{id}/sync", s.syncSubscription)
	mux.Handle("/", assets)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'")
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if origin := r.Header.Get("Origin"); origin != "" {
				u, err := url.Parse(origin)
				if err != nil || u.Host != r.Host {
					problem(w, 403, errors.New("不允许跨站操作"))
					return
				}
			}
		}
		if strings.HasPrefix(r.URL.Path, "/api/") && r.URL.Path != "/api/auth" && r.URL.Path != "/api/login" && !s.authenticated(r) {
			problem(w, 401, errors.New("请先登录"))
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key string `json:"key"`
	}
	if err := readJSON(w, r, &req); err != nil {
		problem(w, 400, err)
		return
	}
	expected := sha256.Sum256([]byte(s.AdminKey))
	actual := sha256.Sum256([]byte(req.Key))
	if subtle.ConstantTimeCompare(expected[:], actual[:]) != 1 {
		problem(w, 401, errors.New("管理口令不正确"))
		return
	}
	token := newID() + newID()
	s.mu.Lock()
	for k, v := range s.sessions {
		if time.Now().After(v) {
			delete(s.sessions, k)
		}
	}
	if len(s.sessions) >= 128 {
		s.mu.Unlock()
		problem(w, 429, errors.New("登录会话过多，请稍后重试"))
		return
	}
	s.sessions[token] = time.Now().Add(24 * time.Hour)
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "manager_session", Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil, MaxAge: 86400})
	respond(w, 200, map[string]bool{"authenticated": true})
}

func (s *Server) state(w http.ResponseWriter, r *http.Request) {
	state, err := s.Manager.Snapshot(r.Context())
	if err != nil {
		problem(w, 500, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	version, coreErr := s.Manager.Kernel.Version(ctx)
	respond(w, 200, struct {
		State
		CoreReady   bool   `json:"coreReady"`
		CoreVersion string `json:"coreVersion"`
	}{state, coreErr == nil, version})
}
func (s *Server) change(w http.ResponseWriter, r *http.Request, fn func() error) {
	result, err := s.Manager.Change(r.Context(), fn)
	if err != nil {
		problem(w, 400, err)
		return
	}
	respond(w, 200, map[string]any{"saved": true, "apply": result})
}
func (s *Server) saveListener(w http.ResponseWriter, r *http.Request) {
	var l Listener
	if err := readJSON(w, r, &l); err != nil {
		problem(w, 400, err)
		return
	}
	l.ID = r.PathValue("id")
	for _, p := range s.Manager.ReservedPorts {
		if l.Port == p {
			problem(w, 400, fmt.Errorf("端口 %d 已保留给管理服务", p))
			return
		}
	}
	s.change(w, r, func() error { return s.Manager.Store.SaveListener(r.Context(), l) })
}
func (s *Server) importNodes(w http.ResponseWriter, r *http.Request) {
	var req importer.ImportRequest
	if err := readJSON(w, r, &req); err != nil {
		problem(w, 400, err)
		return
	}
	items, _, err := importer.Parse(req)
	if err != nil {
		problem(w, 400, err)
		return
	}
	s.change(w, r, func() error { return s.Manager.Store.Import(r.Context(), "", items) })
}

func validSubscriptionURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return errors.New("订阅地址必须是 HTTP 或 HTTPS URL")
	}
	return nil
}
func (s *Server) saveSubscription(w http.ResponseWriter, r *http.Request) {
	var sub Subscription
	if err := readJSON(w, r, &sub); err != nil {
		problem(w, 400, err)
		return
	}
	sub.ID = r.PathValue("id")
	sub.URL = strings.TrimSpace(sub.URL)
	if err := validSubscriptionURL(sub.URL); err != nil {
		problem(w, 400, err)
		return
	}
	s.change(w, r, func() error { return s.Manager.Store.SaveSubscription(r.Context(), sub) })
}

func (s *Server) syncSubscription(w http.ResponseWriter, r *http.Request) {
	state, err := s.Manager.Snapshot(r.Context())
	if err != nil {
		problem(w, 500, err)
		return
	}
	var sub Subscription
	for _, item := range state.Subscriptions {
		if item.ID == r.PathValue("id") {
			sub = item
			break
		}
	}
	if sub.ID == "" {
		problem(w, 404, errors.New("订阅不存在"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sub.URL, nil)
	if err != nil {
		problem(w, 400, errors.New("订阅地址无效"))
		return
	}
	req.Header.Set("User-Agent", "Clash.Meta/Mihomo-Manager")
	client := &http.Client{Timeout: 25 * time.Second, Transport: &http.Transport{Proxy: nil}}
	defer client.CloseIdleConnections()
	res, err := client.Do(req)
	if err != nil {
		problem(w, 502, errors.New("订阅下载失败，请检查地址和网络"))
		return
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		problem(w, 502, fmt.Errorf("订阅返回 HTTP %d", res.StatusCode))
		return
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, (10<<20)+1))
	if err != nil || len(raw) > 10<<20 {
		problem(w, 400, errors.New("订阅下载不完整或超过 10 MiB"))
		return
	}
	items, _, err := importer.Parse(importer.ImportRequest{Raw: string(raw)})
	if err != nil {
		problem(w, 400, err)
		return
	}
	s.change(w, r, func() error {
		current, err := s.Manager.Store.Snapshot(r.Context())
		if err != nil {
			return err
		}
		for _, latest := range current.Subscriptions {
			if latest.ID == sub.ID && latest.URL == sub.URL {
				return s.Manager.Store.Import(r.Context(), sub.ID, items)
			}
		}
		return errors.New("下载期间订阅已修改，请重新同步")
	})
}

func (s *Server) checkNode(w http.ResponseWriter, r *http.Request) {
	state, err := s.Manager.Snapshot(r.Context())
	if err != nil {
		problem(w, 500, err)
		return
	}
	for _, n := range state.Nodes {
		if n.ID == r.PathValue("id") {
			if !n.Enabled || !n.Available {
				problem(w, 400, errors.New("节点未启用或已失效"))
				return
			}
			delay, err := s.Manager.Kernel.Delay(r.Context(), n.ID)
			if err != nil {
				problem(w, 502, err)
				return
			}
			respond(w, 200, map[string]int{"delay": delay})
			return
		}
	}
	problem(w, 404, errors.New("节点不存在"))
}
