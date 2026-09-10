package manager

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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
	return false
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
			_, err := tx.ExecContext(ctx, `INSERT INTO routing_source_selections(scope,name,member) VALUES(?,?,?) ON CONFLICT(scope,name) DO UPDATE SET member=excluded.member`, state.ActiveRoutingSource, name, member)
			return err
		})
	}
	return errors.New("分组不存在")
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
