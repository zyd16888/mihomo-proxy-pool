package manager

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
)

func (s *Server) routingSourceRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/routing-sources/preview", func(w http.ResponseWriter, r *http.Request) {
		var source RoutingSource
		if err := readJSON(w, r, &source); err != nil {
			problem(w, 400, err)
			return
		}
		respond(w, 200, s.RoutingSources.Preview(r.Context(), source))
	})
	mux.HandleFunc("POST /api/routing-sources", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Token string `json:"token"`
		}
		if err := readJSON(w, r, &req); err != nil {
			problem(w, 400, err)
			return
		}
		changed, err := s.RoutingSources.SavePreview(r.Context(), req.Token)
		if err != nil {
			problem(w, 400, err)
			return
		}
		respond(w, 200, map[string]any{"saved": true, "changed": changed})
	})
	mux.HandleFunc("POST /api/routing-sources/{id}/refresh", func(w http.ResponseWriter, r *http.Request) {
		changed, err := s.RoutingSources.Refresh(r.Context(), r.PathValue("id"))
		if err != nil {
			problem(w, 400, err)
			return
		}
		respond(w, 200, map[string]any{"changed": changed})
	})
	mux.HandleFunc("POST /api/routing-sources/activate", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID string `json:"id"`
		}
		if err := readJSON(w, r, &req); err != nil {
			problem(w, 400, err)
			return
		}
		if err := s.RoutingSources.Activate(r.Context(), req.ID); err != nil {
			problem(w, 400, err)
			return
		}
		respond(w, 200, map[string]bool{"applied": true})
	})
	mux.HandleFunc("DELETE /api/routing-sources/{id}", func(w http.ResponseWriter, r *http.Request) {
		m := s.Manager
		m.mu.Lock()
		defer m.mu.Unlock()
		state, err := m.snapshotLocked(r.Context())
		if err != nil {
			problem(w, 500, err)
			return
		}
		id := r.PathValue("id")
		if state.ActiveRoutingSource == id {
			problem(w, 400, errors.New("请先切换到其他方案，再删除此订阅"))
			return
		}
		tx, err := m.Store.db.BeginTx(r.Context(), nil)
		if err != nil {
			problem(w, 500, err)
			return
		}
		defer tx.Rollback()
		for _, table := range []string{"category_edits", "category_rules", "routing_blocked_rules", "routing_source_selections"} {
			if _, err = tx.ExecContext(r.Context(), `DELETE FROM `+table+` WHERE scope=?`, id); err != nil {
				problem(w, 500, err)
				return
			}
		}
		if err = changed(tx.ExecContext(r.Context(), `DELETE FROM routing_sources WHERE id=?`, id)); err == nil {
			err = tx.Commit()
		}
		if err != nil {
			problem(w, 400, err)
			return
		}
		respond(w, 200, map[string]bool{"deleted": true})
	})
	mux.HandleFunc("PUT /api/categories", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Scope string       `json:"scope"`
			Edit  CategoryEdit `json:"edit"`
		}
		if err := readJSON(w, r, &req); err != nil {
			problem(w, 400, err)
			return
		}
		s.change(w, r, func() error { return s.Manager.Store.SaveCategoryEdit(r.Context(), req.Scope, req.Edit) })
	})
	mux.HandleFunc("POST /api/categories/restore", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Scope string `json:"scope"`
			Name  string `json:"name"`
		}
		if err := readJSON(w, r, &req); err != nil {
			problem(w, 400, err)
			return
		}
		s.change(w, r, func() error { return s.Manager.Store.RestoreCategory(r.Context(), req.Scope, req.Name) })
	})
	mux.HandleFunc("POST /api/category-rules", s.saveCategoryRule)
	mux.HandleFunc("PUT /api/category-rules/{id}", s.saveCategoryRule)
	mux.HandleFunc("DELETE /api/category-rules/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.change(w, r, func() error {
			return s.Manager.Store.DeleteCategoryRule(r.Context(), r.URL.Query().Get("scope"), r.PathValue("id"))
		})
	})
	mux.HandleFunc("POST /api/routing-entries/block", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Scope string `json:"scope"`
			ID    string `json:"id"`
			Text  string `json:"text"`
			Block bool   `json:"block"`
		}
		if err := readJSON(w, r, &req); err != nil {
			problem(w, 400, err)
			return
		}
		s.change(w, r, func() error {
			return s.Manager.Store.BlockRoutingRule(r.Context(), req.Scope, req.ID, req.Text, req.Block)
		})
	})
	mux.HandleFunc("GET /api/routing-entries", func(w http.ResponseWriter, r *http.Request) {
		state, err := s.Manager.Snapshot(r.Context())
		if err != nil {
			problem(w, 500, err)
			return
		}
		policy := r.URL.Query().Get("policy")
		term := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		if offset < 0 {
			offset = 0
		}
		entries := []RoutingEntry{}
		total := 0
		for _, entry := range routingEntries(state) {
			if policy != "" && entry.Policy != policy {
				continue
			}
			if term != "" && !strings.Contains(strings.ToLower(entry.Text), term) {
				continue
			}
			if total >= offset && len(entries) < 100 {
				entries = append(entries, entry)
			}
			total++
		}
		respond(w, 200, map[string]any{"entries": entries, "total": total, "offset": offset, "scope": state.ActiveRoutingSource})
	})
}

func (s *Server) saveCategoryRule(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Scope string       `json:"scope"`
		Rule  CategoryRule `json:"rule"`
	}
	if err := readJSON(w, r, &req); err != nil {
		problem(w, 400, err)
		return
	}
	req.Rule.ID = r.PathValue("id")
	s.change(w, r, func() error { return s.Manager.Store.SaveCategoryRule(r.Context(), req.Scope, req.Rule) })
}
