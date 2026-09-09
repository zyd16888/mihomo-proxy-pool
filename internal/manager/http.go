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
	"strconv"
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
	Checks   *NodeChecks
	Observer *Observer
	Exits    *ExitProbes
	syncing  map[string]bool
}

func NewServer(m *Manager, key string) *Server {
	return &Server{
		Manager: m, AdminKey: key, sessions: map[string]time.Time{},
		Checks: NewNodeChecks(m), Observer: NewObserver(m), Exits: NewExitProbes(m, m.ProbePort),
		syncing: map[string]bool{},
	}
}

func (s *Server) Close() {
	s.Checks.Close()
	s.Observer.Close()
	s.Exits.Close()
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
	mux.HandleFunc("GET /api/node-checks", s.nodeChecksState)
	mux.HandleFunc("POST /api/node-checks/batch", s.startNodeCheckBatch)
	mux.HandleFunc("POST /api/node-checks/batch/{id}/stop", s.stopNodeCheckBatch)
	mux.HandleFunc("POST /api/import", s.importNodes)
	mux.HandleFunc("POST /api/subscriptions", s.saveSubscription)
	mux.HandleFunc("PUT /api/subscriptions/{id}", s.saveSubscription)
	mux.HandleFunc("DELETE /api/subscriptions/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.change(w, r, func() error { return s.Manager.Store.DeleteSubscription(r.Context(), r.PathValue("id")) })
	})
	mux.HandleFunc("POST /api/subscriptions/{id}/sync", s.syncSubscription)
	mux.HandleFunc("POST /api/subscriptions/{id}/usage", s.refreshSubscriptionUsage)
	mux.HandleFunc("GET /api/logs", s.readLogs)
	mux.HandleFunc("POST /api/logs/level", s.setLogLevel)
	mux.HandleFunc("DELETE /api/logs", func(w http.ResponseWriter, r *http.Request) {
		s.Observer.Clear()
		respond(w, 200, s.Observer.Logs(0))
	})
	mux.HandleFunc("GET /api/connections", s.readConnections)
	mux.HandleFunc("DELETE /api/connections", func(w http.ResponseWriter, r *http.Request) {
		s.closeConnections(w, r, "")
	})
	mux.HandleFunc("DELETE /api/connections/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.closeConnections(w, r, r.PathValue("id"))
	})
	mux.HandleFunc("GET /api/routing", s.readRouting)
	mux.HandleFunc("PUT /api/routing", s.saveRouting)
	mux.HandleFunc("POST /api/rule-sets", s.saveRuleSet)
	mux.HandleFunc("PUT /api/rule-sets/{id}", s.saveRuleSet)
	mux.HandleFunc("DELETE /api/rule-sets/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.change(w, r, func() error { return s.Manager.Store.DeleteRuleSet(r.Context(), r.PathValue("id")) })
	})
	mux.HandleFunc("POST /api/rule-sets/refresh", s.refreshRuleSets)
	mux.HandleFunc("GET /api/exit-ip", s.exitProbeState)
	mux.HandleFunc("POST /api/exit-ip/nodes", s.startNodeExitProbe)
	mux.HandleFunc("POST /api/exit-ip/listeners/{id}", s.startListenerExitProbe)
	mux.HandleFunc("POST /api/exit-ip/batch/{id}/stop", func(w http.ResponseWriter, r *http.Request) {
		if err := s.Exits.Stop(r.PathValue("id")); err != nil {
			problem(w, 404, err)
			return
		}
		s.writeExitProbes(w, r, http.StatusOK)
	})
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
		CoreReady   bool              `json:"coreReady"`
		CoreVersion string            `json:"coreVersion"`
		Checks      NodeCheckSnapshot `json:"checks"`
	}{state, coreErr == nil, version, s.Checks.Snapshot(state)})
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
	if l.Port == s.Manager.ProbePort {
		problem(w, 400, fmt.Errorf("端口 %d 已保留给出口 IP 探测", l.Port))
		return
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
	s.refreshSubscription(w, r, true)
}
func (s *Server) refreshSubscriptionUsage(w http.ResponseWriter, r *http.Request) {
	s.refreshSubscription(w, r, false)
}

func (s *Server) recordUsage(ctx context.Context, sub Subscription, usage SubscriptionUsage, status string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	s.Manager.mu.Lock()
	defer s.Manager.mu.Unlock()
	return s.Manager.Store.RecordSubscriptionUsage(ctx, sub.ID, sub.URL, usage, status)
}

func (s *Server) refreshSubscription(w http.ResponseWriter, r *http.Request, importNodes bool) {
	id := r.PathValue("id")
	s.mu.Lock()
	if s.syncing[id] {
		s.mu.Unlock()
		problem(w, http.StatusConflict, errors.New("该订阅正在刷新，请等待完成"))
		return
	}
	s.syncing[id] = true
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.syncing, id); s.mu.Unlock() }()
	state, err := s.Manager.Snapshot(r.Context())
	if err != nil {
		problem(w, 500, err)
		return
	}
	var sub Subscription
	for _, item := range state.Subscriptions {
		if item.ID == id {
			sub = item
			break
		}
	}
	if sub.ID == "" {
		problem(w, 404, errors.New("订阅不存在"))
		return
	}
	fail := func(status int, message string) {
		if err := s.recordUsage(r.Context(), sub, SubscriptionUsage{}, "fetch_failed"); err != nil {
			problem(w, 500, err)
			return
		}
		problem(w, status, errors.New(message))
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sub.URL, nil)
	if err != nil {
		fail(400, "订阅地址无效")
		return
	}
	req.Header.Set("User-Agent", "Clash.Meta/Mihomo-Manager")
	client := &http.Client{Timeout: 25 * time.Second, Transport: &http.Transport{Proxy: nil}}
	defer client.CloseIdleConnections()
	res, err := client.Do(req)
	if err != nil {
		fail(502, "订阅下载失败，请检查地址和网络")
		return
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		fail(502, fmt.Sprintf("订阅返回 HTTP %d", res.StatusCode))
		return
	}
	header := res.Header.Get("subscription-userinfo")
	usage, parseErr := ParseSubscriptionUsage(header)
	status := "current"
	if strings.TrimSpace(header) == "" {
		status = "missing"
	} else if parseErr != nil {
		status = "invalid"
	}
	if err = s.recordUsage(r.Context(), sub, usage, status); err != nil {
		problem(w, 400, err)
		return
	}
	if !importNodes {
		current, err := s.Manager.Snapshot(r.Context())
		if err != nil {
			problem(w, 500, err)
			return
		}
		for _, latest := range current.Subscriptions {
			if latest.ID == sub.ID {
				respond(w, 200, map[string]any{"usage": latest.Usage})
				return
			}
		}
		problem(w, 404, errors.New("订阅已移除"))
		return
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, (10<<20)+1))
	if err != nil || len(raw) > 10<<20 {
		problem(w, 400, errors.New("订阅内容下载不完整或超过 10 MiB，节点未更新"))
		return
	}
	items, _, err := importer.Parse(importer.ImportRequest{Raw: string(raw)})
	if err != nil {
		problem(w, 400, err)
		return
	}
	profile := importer.ParseProfile(string(raw))
	s.change(w, r, func() error {
		current, err := s.Manager.Store.Snapshot(r.Context())
		if err != nil {
			return err
		}
		for _, latest := range current.Subscriptions {
			if latest.ID == sub.ID && latest.URL == sub.URL {
				return s.Manager.Store.ImportSubscription(r.Context(), sub.ID, items, profile)
			}
		}
		return errors.New("下载期间订阅已修改，请重新同步")
	})
}

func (s *Server) readLogs(w http.ResponseWriter, r *http.Request) {
	since, err := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	if err != nil || since < 0 {
		since = 0
	}
	respond(w, 200, s.Observer.Logs(since))
}

func (s *Server) setLogLevel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Level string `json:"level"`
	}
	if err := readJSON(w, r, &req); err != nil {
		problem(w, 400, err)
		return
	}
	if err := s.Observer.SetLevel(strings.ToLower(strings.TrimSpace(req.Level))); err != nil {
		problem(w, 400, err)
		return
	}
	respond(w, 200, s.Observer.Logs(0))
}

func (s *Server) readConnections(w http.ResponseWriter, r *http.Request) {
	state, err := s.Manager.Snapshot(r.Context())
	if err != nil {
		problem(w, 500, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	snapshot, err := s.Observer.Connections(ctx, state)
	if err != nil {
		problem(w, 503, errors.New("内核未就绪，暂时无法读取连接"))
		return
	}
	respond(w, 200, snapshot)
}

func (s *Server) closeConnections(w http.ResponseWriter, r *http.Request, id string) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	var err error
	if id == "" {
		err = s.Manager.Kernel.CloseConnections(ctx)
	} else {
		err = s.Manager.Kernel.CloseConnection(ctx, id)
	}
	if err != nil {
		problem(w, 502, errors.New("关闭连接失败，请确认内核状态"))
		return
	}
	respond(w, 200, map[string]bool{"closed": true})
}

// routingView reports the settings together with what the current
// subscriptions would actually contribute, so merge results are visible
// before the configuration is applied.
func (s *Server) readRouting(w http.ResponseWriter, r *http.Request) {
	state, err := s.Manager.Snapshot(r.Context())
	if err != nil {
		problem(w, 500, err)
		return
	}
	preview := state
	// Merge reporting must not depend on a rule listener already existing.
	preview.Listeners = append([]Listener{}, state.Listeners...)
	if !hasRuleListener(preview) {
		preview.Listeners = append(preview.Listeners, Listener{ID: "preview", Port: 0, Mode: ListenerModeRule, Enabled: true})
	}
	active := []string{}
	for _, n := range state.Nodes {
		if n.Enabled && n.Available {
			active = append(active, "node-"+n.ID)
		}
	}
	groups, rules, providers, _, reports := BuildRouting(preview, active)
	respond(w, 200, map[string]any{
		"routing":   state.Routing,
		"ruleSets":  state.RuleSets,
		"policies":  PolicyTargets,
		"reports":   reports,
		"groups":    len(groups),
		"rules":     len(rules),
		"providers": len(providers),
	})
}

func (s *Server) saveRouting(w http.ResponseWriter, r *http.Request) {
	var routing Routing
	if err := readJSON(w, r, &routing); err != nil {
		problem(w, 400, err)
		return
	}
	s.change(w, r, func() error { return s.Manager.Store.SaveRouting(r.Context(), routing) })
}

func (s *Server) saveRuleSet(w http.ResponseWriter, r *http.Request) {
	var set RuleSet
	if err := readJSON(w, r, &set); err != nil {
		problem(w, 400, err)
		return
	}
	set.ID = r.PathValue("id")
	s.change(w, r, func() error { return s.Manager.Store.SaveRuleSet(r.Context(), set) })
}

// refreshRuleSets asks the kernel to pull every enabled provider now instead
// of waiting for its interval. It changes no stored configuration.
func (s *Server) refreshRuleSets(w http.ResponseWriter, r *http.Request) {
	state, err := s.Manager.Snapshot(r.Context())
	if err != nil {
		problem(w, 500, err)
		return
	}
	if state.Revision != state.AppliedRevision || state.LastError != "" {
		problem(w, 409, errors.New("请先成功应用当前配置，再更新规则集"))
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 60*time.Second)
	defer cancel()
	refreshed, failed := 0, []string{}
	for _, set := range state.RuleSets {
		if !set.Enabled {
			continue
		}
		if err := s.Manager.Kernel.RefreshRuleProvider(ctx, set.Name); err != nil {
			failed = append(failed, set.Name)
			continue
		}
		refreshed++
	}
	respond(w, 200, map[string]any{"refreshed": refreshed, "failed": failed})
}

func (s *Server) writeExitProbes(w http.ResponseWriter, r *http.Request, status int) {
	state, err := s.Manager.Snapshot(r.Context())
	if err != nil {
		problem(w, 500, err)
		return
	}
	respond(w, status, s.Exits.Snapshot(state))
}

func (s *Server) exitProbeState(w http.ResponseWriter, r *http.Request) {
	s.writeExitProbes(w, r, http.StatusOK)
}

func (s *Server) startNodeExitProbe(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs []string `json:"ids"`
	}
	if err := readJSON(w, r, &req); err != nil {
		problem(w, 400, err)
		return
	}
	if len(req.IDs) == 0 {
		problem(w, 400, errors.New("请选择要探测的节点"))
		return
	}
	if s.Manager.ProbePort == 0 {
		problem(w, 409, errors.New("未配置出口探测端口，无法按节点探测"))
		return
	}
	if err := s.Exits.StartNodes(r.Context(), req.IDs); err != nil {
		problem(w, 400, err)
		return
	}
	s.writeExitProbes(w, r, http.StatusAccepted)
}

func (s *Server) startListenerExitProbe(w http.ResponseWriter, r *http.Request) {
	if err := s.Exits.StartListener(r.Context(), r.PathValue("id")); err != nil {
		problem(w, 400, err)
		return
	}
	s.writeExitProbes(w, r, http.StatusAccepted)
}

func (s *Server) checkNode(w http.ResponseWriter, r *http.Request) {
	if err := s.Checks.StartSingle(r.Context(), r.PathValue("id")); err != nil {
		problem(w, 400, err)
		return
	}
	s.writeNodeChecks(w, r, http.StatusAccepted)
}

func (s *Server) writeNodeChecks(w http.ResponseWriter, r *http.Request, status int) {
	state, err := s.Manager.Snapshot(r.Context())
	if err != nil {
		problem(w, 500, err)
		return
	}
	respond(w, status, s.Checks.Snapshot(state))
}
func (s *Server) nodeChecksState(w http.ResponseWriter, r *http.Request) {
	s.writeNodeChecks(w, r, http.StatusOK)
}
func (s *Server) startNodeCheckBatch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs []string `json:"ids"`
	}
	if err := readJSON(w, r, &req); err != nil {
		problem(w, 400, err)
		return
	}
	if err := s.Checks.StartBatch(r.Context(), req.IDs); err != nil {
		problem(w, 400, err)
		return
	}
	s.writeNodeChecks(w, r, http.StatusAccepted)
}
func (s *Server) stopNodeCheckBatch(w http.ResponseWriter, r *http.Request) {
	if err := s.Checks.StopBatch(r.PathValue("id")); err != nil {
		problem(w, 404, err)
		return
	}
	s.writeNodeChecks(w, r, http.StatusOK)
}
