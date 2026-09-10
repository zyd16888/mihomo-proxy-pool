package manager

import (
	"context"
	"net/http"
	"slices"
	"time"
)

type CoreProxy struct {
	Type    string `json:"type"`
	Now     string `json:"now"`
	History []struct {
		Time  string `json:"time"`
		Delay int    `json:"delay"`
	} `json:"history"`
}

func (r *Runtime) Proxies(ctx context.Context) (map[string]CoreProxy, error) {
	var result struct {
		Proxies map[string]CoreProxy `json:"proxies"`
	}
	err := r.request(ctx, http.MethodGet, "/proxies", nil, &result)
	return result.Proxies, err
}

type ProxyGroupView struct {
	AllNodes          bool     `json:"allNodes"`
	ConfiguredMembers []string `json:"configuredMembers"`
	Label             string   `json:"label"`
	Edited            bool     `json:"edited"`
	RuleCount         int      `json:"ruleCount"`
	Name              string   `json:"name"`
	Kind              string   `json:"kind"`
	Members           []string `json:"members"`
	Selected          string   `json:"selected"`
	Now               string   `json:"now"`
	Chain             []string `json:"chain"`
	Live              bool     `json:"live"`
	Warning           string   `json:"warning,omitempty"`
	CustomID          string   `json:"customId,omitempty"`
	RuleSets          []string `json:"ruleSets"`
}

func (s *Server) groupViews(ctx context.Context, state State) ([]ProxyGroupView, map[string]CoreProxy, string) {
	groups := routingPreview(state)
	preview := state
	preview.Routing.Enabled = true
	preview.Listeners = []Listener{{Mode: ListenerModeRule, Enabled: true}}
	plan, _ := compileRouting(preview)
	counts := map[string]int{}
	for _, rule := range plan.Rules {
		counts[rulePolicy(rule)]++
	}
	policies := policyOptions(state)
	core := map[string]CoreProxy{}
	runtimeError := ""
	live := state.Routing.Enabled && hasRuleListener(state) && state.Revision == state.AppliedRevision && state.LastError == ""
	if reader, ok := s.Manager.Kernel.(interface {
		Proxies(context.Context) (map[string]CoreProxy, error)
	}); ok && live {
		probe, cancel := context.WithTimeout(ctx, 3*time.Second)
		var err error
		core, err = reader.Proxies(probe)
		cancel()
		if err != nil {
			runtimeError = "暂时无法读取实际出口"
			live = false
		}
	} else {
		live = false
	}
	views := []ProxyGroupView{}
	for _, group := range groups {
		name := stringOf(group["name"])
		members := stringsOf(group["proxies"])
		v := ProxyGroupView{Name: name, Kind: stringOf(group["type"]), Members: members, Selected: state.Selections[name], RuleSets: []string{}, Chain: []string{}}
		if len(members) > 0 && v.Selected == "" && v.Kind == "select" {
			v.Selected = members[0]
		}
		v.ConfiguredMembers = append([]string{}, members...)
		if source := state.activeRoutingSource(); source != nil {
			for _, raw := range source.Document.Groups {
				if stringOf(raw["name"]) == name {
					v.AllNodes = truthy(raw["include-all"]) || truthy(raw["include-all-proxies"])
					v.ConfiguredMembers = stringsOf(raw["proxies"])
				}
			}
		}
		v.Label = name
		v.RuleCount = counts[name]
		for _, edit := range state.CategoryEdits {
			if edit.Name == name {
				v.Label = edit.Label
				v.Edited = true
				if edit.Kind != "" {
					v.AllNodes = edit.AllNodes
					v.ConfiguredMembers = edit.Members
				}
			}
		}
		if state.ActiveRoutingSource == "" && name == GroupFinal && !slices.Contains(policies, state.Routing.DefaultPolicy) {
			v.Warning = "兜底策略已不存在，当前拦截流量，请重新设置"
		}
		if v.Selected != "" && !slices.Contains(members, v.Selected) {
			v.Warning = "原选择已不在分组中，当前使用默认出口，请重新选择"
		}
		for _, custom := range state.ProxyGroups {
			if custom.Name == name {
				v.CustomID = custom.ID
			}
		}
		for _, set := range effectiveRuleSets(state, plan) {
			if set.Policy == name {
				v.RuleSets = append(v.RuleSets, set.Name)
			}
		}
		if p, ok := core[name]; live && ok {
			v.Live = true
			v.Now = p.Now
			seen := map[string]bool{name: true}
			next := p.Now
			for next != "" && !seen[next] {
				seen[next] = true
				v.Chain = append(v.Chain, next)
				next = core[next].Now
			}
		}
		views = append(views, v)
	}
	return views, core, runtimeError
}

func (s *Server) saveProxyGroup(w http.ResponseWriter, r *http.Request) {
	var group ProxyGroup
	if err := readJSON(w, r, &group); err != nil {
		problem(w, 400, err)
		return
	}
	group.ID = r.PathValue("id")
	s.change(w, r, func() error { return s.Manager.Store.SaveProxyGroup(r.Context(), group) })
}

func (s *Server) selectGroup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name   string `json:"name"`
		Member string `json:"member"`
	}
	if err := readJSON(w, r, &req); err != nil {
		problem(w, 400, err)
		return
	}
	s.change(w, r, func() error { return s.Manager.Store.SaveSelection(r.Context(), req.Name, req.Member) })
}

func (s *Server) addTemplates(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs []string `json:"ids"`
	}
	if err := readJSON(w, r, &req); err != nil {
		problem(w, 400, err)
		return
	}
	s.change(w, r, func() error { return s.Manager.Store.AddRuleTemplates(r.Context(), req.IDs) })
}
