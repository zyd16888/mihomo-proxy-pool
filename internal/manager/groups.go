package manager

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Empty NodeIDs follows the active node pool, including future subscription updates.
// Explicit membership keeps its order, which also determines fallback priority.
type ProxyGroup struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Kind    string   `json:"kind"`
	NodeIDs []string `json:"nodeIds"`
}

func uniqueMembers(values []any) []any {
	result := []any{}
	seen := map[string]bool{}
	for _, value := range values {
		name := stringOf(value)
		if !seen[name] {
			seen[name] = true
			result = append(result, value)
		}
	}
	return result
}

func foreignPolicy(policy string, groups []map[string]any, visited map[string]bool) bool {
	if builtinOutbounds[policy] {
		return false
	}
	if strings.HasPrefix(policy, "node-") {
		return true
	}
	if visited[policy] {
		return false
	}
	visited[policy] = true
	for _, group := range groups {
		if stringOf(group["name"]) == policy {
			members := stringsOf(group["proxies"])
			if len(members) == 0 {
				return false
			}
			if stringOf(group["type"]) == "select" {
				return foreignPolicy(members[0], groups, visited)
			}
			for _, member := range members {
				if foreignPolicy(member, groups, visited) {
					return true
				}
			}
			return false
		}
	}
	return policy == GroupProxy || policy == GroupSelect || policy == GroupAuto
}

func (s *Store) proxyGroups(ctx context.Context) ([]ProxyGroup, map[string]string, error) {
	groups := []ProxyGroup{}
	selections := map[string]string{}
	rows, err := s.db.QueryContext(ctx, `SELECT id,name,kind,node_ids FROM proxy_groups ORDER BY name`)
	if err != nil {
		return groups, selections, err
	}
	for rows.Next() {
		var group ProxyGroup
		var raw string
		if err = rows.Scan(&group.ID, &group.Name, &group.Kind, &raw); err != nil {
			break
		}
		if err = json.Unmarshal([]byte(raw), &group.NodeIDs); err != nil {
			break
		}
		groups = append(groups, group)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return groups, selections, err
	}
	rows, err = s.db.QueryContext(ctx, `SELECT name,member FROM proxy_selections`)
	if err != nil {
		return groups, selections, err
	}
	defer rows.Close()
	for rows.Next() {
		var name, member string
		if err := rows.Scan(&name, &member); err != nil {
			return groups, selections, err
		}
		selections[name] = member
	}
	return groups, selections, rows.Err()
}

func routingPreview(state State) []map[string]any {
	state.Routing.Enabled = true
	state.Listeners = []Listener{{Mode: ListenerModeRule, Enabled: true}}
	nodes := []string{}
	for _, n := range state.Nodes {
		if n.Enabled && n.Available {
			nodes = append(nodes, "node-"+n.ID)
		}
	}
	groups, _, _, _, _ := BuildRouting(state, nodes)
	return groups
}

func policyOptions(state State) []string {
	options := []string{"DIRECT", "REJECT"}
	for _, group := range routingPreview(state) {
		name := stringOf(group["name"])
		if !slices.Contains(options, name) {
			options = append(options, name)
		}
	}
	return options
}

func (s *Store) SaveProxyGroup(ctx context.Context, group ProxyGroup) error {
	group.Name = strings.TrimSpace(group.Name)
	if group.Name == "" || len([]rune(group.Name)) > 64 || strings.ContainsAny(group.Name, ",\r\n") || strings.HasPrefix(group.Name, "node-") || group.Name == ExitProbeGroup || group.Name == "GLOBAL" || group.Name == GroupAuto || slices.Contains(PolicyTargets, group.Name) || builtinOutbounds[strings.ToUpper(group.Name)] {
		return errors.New("分组名称须为 1–64 字符，不能包含逗号、换行或使用保留名称")
	}
	if group.Kind != "select" && group.Kind != "url-test" && group.Kind != "fallback" {
		return errors.New("请选择手动、自动选择或故障转移")
	}
	state, err := s.Snapshot(ctx)
	if err != nil {
		return err
	}
	if state.ActiveRoutingSource != "" {
		return errors.New("当前为外部方案，请使用分类编辑或切回本地方案")
	}
	found := group.ID == ""
	for _, old := range state.ProxyGroups {
		if old.ID == group.ID {
			found = true
			if old.Name != group.Name {
				return errors.New("分组名称不可修改，请新增分组后迁移规则")
			}
		}
	}
	if !found {
		return errors.New("分组不存在")
	}
	if group.ID == "" && slices.Contains(policyOptions(state), group.Name) {
		return errors.New("分组名称已存在")
	}
	known := map[string]bool{}
	for _, node := range state.Nodes {
		known[node.ID] = true
	}
	seen := map[string]bool{}
	for _, id := range group.NodeIDs {
		if !known[id] || seen[id] {
			return errors.New("成员节点不存在或重复")
		}
		seen[id] = true
	}
	if group.NodeIDs == nil {
		group.NodeIDs = []string{}
	}
	raw, err := json.Marshal(group.NodeIDs)
	if err != nil {
		return err
	}
	return s.mutate(ctx, func(tx *sql.Tx) error {
		if group.ID == "" {
			_, err := tx.ExecContext(ctx, `INSERT INTO proxy_groups(id,name,kind,node_ids) VALUES(?,?,?,?)`, newID(), group.Name, group.Kind, string(raw))
			return err
		}
		return changed(tx.ExecContext(ctx, `UPDATE proxy_groups SET kind=?,node_ids=? WHERE id=?`, group.Kind, string(raw), group.ID))
	})
}

func (s *Store) DeleteProxyGroup(ctx context.Context, id string) error {
	state, err := s.Snapshot(ctx)
	if err != nil {
		return err
	}
	if state.ActiveRoutingSource != "" {
		return errors.New("请切回本地方案删除本地代理组")
	}
	return s.mutate(ctx, func(tx *sql.Tx) error {
		var name string
		if err := tx.QueryRowContext(ctx, `SELECT name FROM proxy_groups WHERE id=?`, id).Scan(&name); err != nil {
			return err
		}
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM rule_sets WHERE policy=?) + (SELECT count(*) FROM routing WHERE default_policy=?)`, name, name).Scan(&count); err != nil {
			return err
		}
		if count > 0 {
			return errors.New("分组仍被规则集或兜底策略引用，请先更换出口")
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM proxy_selections WHERE name=? OR member=?`, name, name); err != nil {
			return err
		}
		return changed(tx.ExecContext(ctx, `DELETE FROM proxy_groups WHERE id=?`, id))
	})
}

func (s *Store) SaveSelection(ctx context.Context, name, member string) error {
	state, err := s.Snapshot(ctx)
	if err != nil {
		return err
	}
	for _, group := range routingPreview(state) {
		if stringOf(group["name"]) != name {
			continue
		}
		if stringOf(group["type"]) != "select" {
			return errors.New("自动选择和故障转移由内核决定出口；固定节点请使用手动分组")
		}
		if !slices.Contains(stringsOf(group["proxies"]), member) {
			return errors.New("节点不在当前分组中，请刷新后重试")
		}
		return s.mutate(ctx, func(tx *sql.Tx) error {
			if state.ActiveRoutingSource != "" {
				_, err := tx.ExecContext(ctx, `INSERT INTO routing_source_selections(scope,name,member) VALUES(?,?,?) ON CONFLICT(scope,name) DO UPDATE SET member=excluded.member`, state.ActiveRoutingSource, name, member)
				return err
			}
			_, err := tx.ExecContext(ctx, `INSERT INTO proxy_selections(name,member) VALUES(?,?) ON CONFLICT(name) DO UPDATE SET member=excluded.member`, name, member)
			return err
		})
	}
	return errors.New("分组不存在")
}

func customGroups(state State, nodes []string) []map[string]any {
	groups := []map[string]any{}
	for _, group := range state.ProxyGroups {
		members := []string{}
		if len(group.NodeIDs) == 0 {
			members = append(members, nodes...)
		} else {
			for _, id := range group.NodeIDs {
				if slices.Contains(nodes, "node-"+id) {
					members = append(members, "node-"+id)
				}
			}
		}
		if group.Kind == "select" {
			members = append([]string{GroupSelect}, members...)
			members = append(members, "DIRECT", "REJECT")
		}
		// An empty automatic group blocks instead of accidentally bypassing the proxy.
		if len(members) == 0 {
			members = []string{"REJECT"}
		}
		entry := map[string]any{"name": group.Name, "type": group.Kind, "proxies": toAny(members)}
		if group.Kind != "select" {
			entry["url"] = "https://www.gstatic.com/generate_204"
			entry["interval"] = 300
			entry["lazy"] = true
		}
		if group.Kind == "url-test" {
			entry["tolerance"] = 50
		}
		groups = append(groups, entry)
	}
	return groups
}

// Persist the chosen member as the first member, so last-good.yaml is itself
// sufficient to recover choices. Runtime selection is restored after reload too.
func orderSelections(groups []map[string]any, selections map[string]string) {
	for _, group := range groups {
		if stringOf(group["type"]) != "select" {
			continue
		}
		members := stringsOf(group["proxies"])
		selected := selections[stringOf(group["name"])]
		if i := slices.Index(members, selected); i > 0 {
			members = append([]string{selected}, append(members[:i], members[i+1:]...)...)
			group["proxies"] = toAny(members)
		}
	}
}

type RuleTemplate struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Sets        []RuleSet `json:"sets"`
}

func RuleTemplates() []RuleTemplate {
	definitions := []struct {
		id, name, description string
		ip                    bool
	}{
		{"category-ai-!cn", "AI 服务", "海外 AI 服务，包括 OpenAI、Claude 等", false},
		{"youtube", "YouTube", "视频播放与相关服务", false},
		{"netflix", "Netflix", "流媒体域名与 IP 段", true},
		{"telegram", "Telegram", "消息服务域名与 IP 段", true},
		{"google", "Google", "搜索及相关服务", false},
		{"microsoft", "Microsoft", "微软在线服务", false},
		{"apple", "Apple", "苹果在线服务", false},
		{"steam", "Steam", "游戏商店与下载", false},
	}
	result := []RuleTemplate{}
	for _, d := range definitions {
		name := strings.ReplaceAll(d.id, "!", "not-")
		set := RuleSet{Name: name + "-domain", Policy: d.name, Behavior: "domain", Format: "mrs", URL: ruleSetBase + "/geosite/" + d.id + ".mrs", Interval: 86400, Enabled: true, Position: 300}
		t := RuleTemplate{ID: d.id, Name: d.name, Description: d.description, Sets: []RuleSet{set}}
		if d.ip {
			set.Name = name + "-ip"
			set.Behavior = "ipcidr"
			set.URL = ruleSetBase + "/geoip/" + d.id + ".mrs"
			set.NoResolve = true
			set.Position = 310
			t.Sets = append(t.Sets, set)
		}
		result = append(result, t)
	}
	return result
}

// One transaction for a whole preset import. Conflicts never overwrite user rules.
func (s *Store) AddRuleTemplates(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return errors.New("请至少选择一个分类")
	}
	catalog := map[string]RuleTemplate{}
	for _, t := range RuleTemplates() {
		catalog[t.ID] = t
	}
	state, err := s.Snapshot(ctx)
	if err != nil {
		return err
	}
	if state.ActiveRoutingSource != "" {
		return errors.New("请切回本地方案添加内置模板")
	}
	policies := policyOptions(state)
	return s.mutate(ctx, func(tx *sql.Tx) error {
		seen := map[string]bool{}
		for _, id := range ids {
			t, ok := catalog[id]
			if !ok || seen[id] {
				return errors.New("分类不存在或重复")
			}
			seen[id] = true
			if slices.Contains(policies, t.Name) {
				return fmt.Errorf("分组 %s 已存在，请在规则集中为其添加规则", t.Name)
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO proxy_groups(id,name,kind,node_ids) VALUES(?,?,'select','[]')`, newID(), t.Name); err != nil {
				return err
			}
			for _, set := range t.Sets {
				var count int
				if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM rule_sets WHERE name=?`, set.Name).Scan(&count); err != nil {
					return err
				}
				if count > 0 {
					return fmt.Errorf("规则集 %s 已存在，未导入任何分类", set.Name)
				}
				if _, err := tx.ExecContext(ctx, `INSERT INTO rule_sets(id,name,policy,behavior,format,url,interval,no_resolve,enabled,position,builtin) VALUES(?,?,?,?,?,?,?,?,1,?,0)`, newID(), set.Name, set.Policy, set.Behavior, set.Format, set.URL, set.Interval, set.NoResolve, set.Position); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// Reading the applied configuration keeps recovery independent of a newer,
// unacknowledged database revision. The probe selector is deliberately excluded.
func (m *Manager) restoreSelections(ctx context.Context, groups []map[string]any) error {
	for _, group := range groups {
		if stringOf(group["type"]) != "select" || stringOf(group["name"]) == ExitProbeGroup {
			continue
		}
		members := stringsOf(group["proxies"])
		if len(members) == 0 {
			return errors.New("代理组没有成员")
		}
		if err := m.Kernel.SelectProxy(ctx, stringOf(group["name"]), members[0]); err != nil {
			return err
		}
	}
	return nil
}
