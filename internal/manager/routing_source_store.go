package manager

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

func (s *Store) loadRoutingSources(ctx context.Context, state *State) error {
	state.RoutingSources = []RoutingSource{}
	state.CategoryEdits = []CategoryEdit{}
	state.CategoryRules = []CategoryRule{}
	state.BlockedRules = map[string]string{}
	rows, err := s.db.QueryContext(ctx, `SELECT config,document FROM routing_sources ORDER BY id`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var raw, doc string
		if err = rows.Scan(&raw, &doc); err != nil {
			break
		}
		var source RoutingSource
		if err = json.Unmarshal([]byte(raw), &source); err != nil {
			break
		}
		if err = json.Unmarshal([]byte(doc), &source.Document); err != nil {
			break
		}
		state.RoutingSources = append(state.RoutingSources, source)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	if err = s.db.QueryRowContext(ctx, `SELECT source_id FROM routing_active WHERE id=1`).Scan(&state.ActiveRoutingSource); err != nil {
		return err
	}
	return s.loadRoutingScope(ctx, state)
}

func (s *Store) loadRoutingScope(ctx context.Context, state *State) error {
	state.CategoryEdits = []CategoryEdit{}
	state.CategoryRules = []CategoryRule{}
	state.BlockedRules = map[string]string{}
	for _, table := range []string{"category_edits", "category_rules"} {
		rows, err := s.db.QueryContext(ctx, `SELECT data FROM `+table+` WHERE scope=? ORDER BY rowid`, state.ActiveRoutingSource)
		if err != nil {
			return err
		}
		for rows.Next() {
			var raw string
			if err = rows.Scan(&raw); err != nil {
				break
			}
			if table == "category_edits" {
				var edit CategoryEdit
				err = json.Unmarshal([]byte(raw), &edit)
				state.CategoryEdits = append(state.CategoryEdits, edit)
			} else {
				var rule CategoryRule
				err = json.Unmarshal([]byte(raw), &rule)
				state.CategoryRules = append(state.CategoryRules, rule)
			}
			if err != nil {
				break
			}
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return err
		}
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,rule FROM routing_blocked_rules WHERE scope=?`, state.ActiveRoutingSource)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id, rule string
		if err = rows.Scan(&id, &rule); err != nil {
			break
		}
		state.BlockedRules[id] = rule
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	state.Selections = map[string]string{}
	query := `SELECT name,member FROM proxy_selections`
	args := []any{}
	if state.ActiveRoutingSource != "" {
		query = `SELECT name,member FROM routing_source_selections WHERE scope=?`
		args = append(args, state.ActiveRoutingSource)
	}
	rows, err = s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name, member string
		if err := rows.Scan(&name, &member); err != nil {
			return err
		}
		state.Selections[name] = member
	}
	return rows.Err()
}

func writeRoutingSource(ctx context.Context, tx *sql.Tx, source RoutingSource) error {
	raw, err := json.Marshal(source)
	if err != nil {
		return err
	}
	doc, err := json.Marshal(source.Document)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO routing_sources(id,config,document) VALUES(?,?,?) ON CONFLICT(id) DO UPDATE SET config=excluded.config,document=excluded.document`, source.ID, string(raw), string(doc))
	return err
}

func (s *Store) saveRoutingSource(ctx context.Context, source RoutingSource, bump bool) error {
	if bump {
		return s.mutate(ctx, func(tx *sql.Tx) error { return writeRoutingSource(ctx, tx, source) })
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = writeRoutingSource(ctx, tx, source); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) sourceStatus(ctx context.Context, id string, version int, message string) error {
	state, err := s.Snapshot(ctx)
	if err != nil {
		return err
	}
	for _, source := range state.RoutingSources {
		if source.ID == id && source.Version == version {
			source.CheckedAt = time.Now().UTC().Format(time.RFC3339)
			source.LastError = message
			return s.saveRoutingSource(ctx, source, false)
		}
	}
	return nil
}

func (s *Store) SaveCategoryEdit(ctx context.Context, scope string, edit CategoryEdit) error {
	state, err := s.Snapshot(ctx)
	if err != nil {
		return err
	}
	if scope != state.ActiveRoutingSource {
		return errors.New("当前方案已切换，请刷新后重试")
	}
	if edit.Deleted {
		for _, old := range state.CategoryEdits {
			if old.Name == edit.Name {
				old.Deleted = true
				edit = old
				break
			}
		}
	}
	if edit.Name == "" && !edit.Deleted {
		edit.Name = "category-" + newID()
	}
	edit.Name = strings.TrimSpace(edit.Name)
	edit.Label = strings.TrimSpace(edit.Label)
	if edit.Name == "" || len(edit.Name) > 256 || strings.ContainsAny(edit.Name, ",\r\n") || strings.HasPrefix(edit.Name, "node-") || edit.Name == ExitProbeGroup || edit.Name == "GLOBAL" || builtinOutbounds[edit.Name] {
		return errors.New("分组标识无效")
	}
	if edit.Label == "" {
		edit.Label = edit.Name
	}
	if len([]rune(edit.Label)) > 64 {
		return errors.New("显示名称最长 64 字符")
	}
	if !edit.Deleted && edit.Kind != "" && edit.Kind != "select" && edit.Kind != "url-test" && edit.Kind != "fallback" && edit.Kind != "load-balance" {
		return errors.New("不支持的分组类型")
	}
	if !edit.Deleted {
		if edit.Kind == "" && !allowedCategoryMember(state, edit.Name) {
			return errors.New("新增分组需要指定选择方式")
		}
		for _, member := range edit.Members {
			if !allowedCategoryMember(state, member) {
				return errors.New("候选出口已不存在，请刷新后重试")
			}
		}
	}
	state.CategoryEdits = replaceCategoryEdit(state.CategoryEdits, edit)
	state.Routing.Enabled = true
	state.Listeners = []Listener{{Mode: ListenerModeRule, Enabled: true}}
	if _, err := compileRouting(state); err != nil {
		return err
	}
	raw, err := json.Marshal(edit)
	if err != nil {
		return err
	}
	return s.mutate(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO category_edits(scope,name,data) VALUES(?,?,?) ON CONFLICT(scope,name) DO UPDATE SET data=excluded.data`, scope, edit.Name, string(raw))
		return err
	})
}

func replaceCategoryEdit(edits []CategoryEdit, edit CategoryEdit) []CategoryEdit {
	result := append([]CategoryEdit{}, edits...)
	for i := range result {
		if result[i].Name == edit.Name {
			result[i] = edit
			return result
		}
	}
	return append(result, edit)
}

func (s *Store) RestoreCategory(ctx context.Context, scope, name string) error {
	state, err := s.Snapshot(ctx)
	if err != nil {
		return err
	}
	if state.ActiveRoutingSource != scope {
		return errors.New("当前方案已切换，请刷新后重试")
	}
	for _, edit := range state.CategoryEdits {
		if edit.Name == name && edit.Deleted {
			edit.Deleted = false
			state.CategoryEdits = replaceCategoryEdit(state.CategoryEdits, edit)
			state.Routing.Enabled = true
			state.Listeners = []Listener{{Mode: ListenerModeRule, Enabled: true}}
			if _, err := compileRouting(state); err != nil {
				return err
			}
			raw, err := json.Marshal(edit)
			if err != nil {
				return err
			}
			return s.mutate(ctx, func(tx *sql.Tx) error {
				return changed(tx.ExecContext(ctx, `UPDATE category_edits SET data=? WHERE scope=? AND name=?`, string(raw), scope, name))
			})
		}
	}
	return s.mutate(ctx, func(tx *sql.Tx) error {
		return changed(tx.ExecContext(ctx, `DELETE FROM category_edits WHERE scope=? AND name=?`, scope, name))
	})
}

func (s *Store) SaveCategoryRule(ctx context.Context, scope string, rule CategoryRule) error {
	if err := normalizeCategoryRule(&rule); err != nil {
		return err
	}
	state, err := s.Snapshot(ctx)
	if err != nil {
		return err
	}
	if scope != state.ActiveRoutingSource {
		return errors.New("当前方案已切换，请刷新后重试")
	}
	known := false
	for _, policy := range policyOptions(state) {
		if policy == rule.Policy {
			known = true
		}
	}
	if !known {
		return errors.New("请选择有效的规则组")
	}
	newRule := rule.ID == ""
	if newRule && rule.ReplacesText != "" {
		found := false
		for _, entry := range routingEntries(state) {
			if !entry.Local && entry.Text == rule.ReplacesText {
				found = true
				break
			}
		}
		if !found {
			return errors.New("原规则已变化，请刷新后重试")
		}
	}
	if newRule {
		rule.ID = newID()
	}
	raw, err := json.Marshal(rule)
	if err != nil {
		return err
	}
	return s.mutate(ctx, func(tx *sql.Tx) error {
		if newRule {
			if rule.ReplacesText != "" {
				if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO routing_blocked_rules(scope,id,rule) VALUES(?,?,?)`, scope, routingEntryID(rule.ReplacesText), rule.ReplacesText); err != nil {
					return err
				}
			}
			_, err := tx.ExecContext(ctx, `INSERT INTO category_rules(scope,id,data) VALUES(?,?,?)`, scope, rule.ID, string(raw))
			return err
		}
		return changed(tx.ExecContext(ctx, `UPDATE category_rules SET data=? WHERE scope=? AND id=?`, string(raw), scope, rule.ID))
	})
}

func (s *Store) DeleteCategoryRule(ctx context.Context, scope, id string) error {
	state, err := s.Snapshot(ctx)
	if err != nil {
		return err
	}
	if state.ActiveRoutingSource != scope {
		return errors.New("当前方案已切换，请刷新后重试")
	}
	return s.mutate(ctx, func(tx *sql.Tx) error {
		return changed(tx.ExecContext(ctx, `DELETE FROM category_rules WHERE scope=? AND id=?`, scope, id))
	})
}

func (s *Store) BlockRoutingRule(ctx context.Context, scope, id, text string, block bool) error {
	state, err := s.Snapshot(ctx)
	if err != nil {
		return err
	}
	if scope != state.ActiveRoutingSource {
		return errors.New("当前方案已切换，请刷新后重试")
	}
	if block {
		valid := false
		for _, entry := range routingEntries(state) {
			if entry.ID == id && entry.Text == text && !entry.Local {
				valid = true
				break
			}
		}
		if !valid {
			return errors.New("原规则已变化，请刷新后重试")
		}
	}
	return s.mutate(ctx, func(tx *sql.Tx) error {
		if !block {
			_, err := tx.ExecContext(ctx, `DELETE FROM routing_blocked_rules WHERE scope=? AND id=?`, scope, id)
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO routing_blocked_rules(scope,id,rule) VALUES(?,?,?) ON CONFLICT(scope,id) DO UPDATE SET rule=excluded.rule`, scope, id, text)
		return err
	})
}
