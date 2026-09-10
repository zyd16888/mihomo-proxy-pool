package manager

import (
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"sort"
	"strings"
)

var providerReference = regexp.MustCompile(`RULE-SET,([^,)]+)`)

type RoutingPlan struct {
	Groups    []map[string]any
	Rules     []string
	Providers map[string]map[string]any
	DNS       map[string]any
	Reports   []MergeReport
}

func copyMaps(values []map[string]any) []map[string]any {
	result := make([]map[string]any, 0, len(values))
	for _, value := range values {
		copy := map[string]any{}
		for k, v := range value {
			copy[k] = v
		}
		result = append(result, copy)
	}
	return result
}

func baseRoutingPlan(state State) (RoutingPlan, error) {
	var plan RoutingPlan
	if state.ActiveRoutingSource == "" {
		nodes := []string{}
		for _, n := range state.Nodes {
			if n.Enabled && n.Available {
				nodes = append(nodes, "node-"+n.ID)
			}
		}
		state.Selections = nil
		plan.Groups, plan.Rules, plan.Providers, plan.DNS, plan.Reports = buildLocalRouting(state, nodes)
		return plan, nil
	}
	source := state.activeRoutingSource()
	if source == nil || source.Digest == "" {
		return plan, errors.New("当前分流方案尚无可用快照")
	}
	plan.Groups = copyMaps(source.Document.Groups)
	plan.Rules = append([]string{}, source.Document.Rules...)
	plan.Providers = map[string]map[string]any{}
	for name, provider := range source.Document.Providers {
		copy := map[string]any{}
		for k, v := range provider {
			copy[k] = v
		}
		copy["path"] = "./ruleset/source-" + source.ID + "-" + digestBytes([]byte(name))[:16] + fileSuffix(stringOf(provider["format"]))
		if stringOf(copy["type"]) == "http" {
			copy["proxy"] = "DIRECT"
		}
		plan.Providers[name] = copy
	}
	if source.ImportDNS {
		plan.DNS = map[string]any{}
		for k, v := range source.Document.DNS {
			plan.DNS[k] = v
		}
	}
	return plan, nil
}

func compileRouting(state State) (RoutingPlan, error) {
	if !state.Routing.Enabled || !hasRuleListener(state) {
		return RoutingPlan{}, nil
	}
	plan, err := baseRoutingPlan(state)
	if err != nil {
		return plan, err
	}
	if state.ActiveRoutingSource != "" && !state.Routing.AllowGeoRules {
		for _, rule := range plan.Rules {
			if strings.Contains(rule, "GEOIP,") || strings.Contains(rule, "GEOSITE,") {
				return plan, errors.New("当前方案包含 GEO 规则，请保持允许 GEO 规则或切换方案")
			}
		}
		if source := state.activeRoutingSource(); source.ImportDNS && state.Routing.DNSEnabled {
			if policies, ok := plan.DNS["nameserver-policy"].(map[string]any); ok {
				for name := range policies {
					if strings.HasPrefix(name, "geosite:") {
						return plan, errors.New("当前方案 DNS 使用 GEOSITE，请允许 GEO 规则")
					}
				}
			}
		}
	}
	initialProviders := routingProviderRefs(plan.Rules)
	deleted := map[string]bool{}
	overridden := map[string]bool{}
	for _, edit := range state.CategoryEdits {
		if edit.Deleted {
			deleted[edit.Name] = true
			continue
		}
		if edit.Kind == "" {
			continue
		}
		overridden[edit.Name] = true
		group := map[string]any{"name": edit.Name, "type": edit.Kind, "proxies": toAny(edit.Members), "include-all-proxies": edit.AllNodes}
		if edit.Kind != "select" {
			group["url"] = "https://www.gstatic.com/generate_204"
			group["interval"] = 300
			group["lazy"] = true
		}
		if edit.Kind == "url-test" {
			group["tolerance"] = 50
		}
		found := false
		for i, old := range plan.Groups {
			if stringOf(old["name"]) == edit.Name {
				plan.Groups[i] = group
				found = true
				break
			}
		}
		if !found {
			plan.Groups = append(plan.Groups, group)
		}
	}
	active := map[string]bool{}
	pool := []Node{}
	for _, node := range state.routingNodes() {
		if node.Enabled && node.Available {
			active["node-"+node.ID] = true
			pool = append(pool, node)
		}
	}
	known := map[string]bool{}
	for name := range builtinOutbounds {
		known[name] = true
	}
	for _, group := range plan.Groups {
		if !deleted[stringOf(group["name"])] {
			known[stringOf(group["name"])] = true
		}
	}
	groups := []map[string]any{}
	for _, group := range plan.Groups {
		name := stringOf(group["name"])
		if deleted[name] {
			continue
		}
		members := []string{}
		for _, member := range stringsOf(group["proxies"]) {
			if deleted[member] {
				continue
			}
			if strings.HasPrefix(member, "node-") {
				if active[member] {
					members = append(members, member)
				}
				continue
			}
			if !known[member] {
				if overridden[name] {
					continue
				}
				return plan, fmt.Errorf("分组 %s 引用了不存在的出口 %s", name, member)
			}
			members = append(members, member)
		}
		if truthy(group["include-all-proxies"]) || truthy(group["include-all"]) {
			include, exclude := compilePattern(stringOf(group["filter"])), compilePattern(stringOf(group["exclude-filter"]))
			for _, node := range pool {
				if include != nil && !include.MatchString(node.Name) || exclude != nil && exclude.MatchString(node.Name) {
					continue
				}
				members = append(members, "node-"+node.ID)
			}
		}
		if len(members) == 0 {
			members = []string{"REJECT"}
		}
		group["proxies"] = uniqueMembers(toAny(members))
		for _, key := range []string{"include-all-proxies", "include-all", "filter", "exclude-filter", "use", "include-all-providers"} {
			delete(group, key)
		}
		groups = append(groups, group)
	}
	if err := validateGroupGraph(groups); err != nil {
		return plan, err
	}
	plan.Groups = groups
	rules := []string{}
	for _, rule := range plan.Rules {
		policy := rulePolicy(rule)
		if deleted[policy] || state.BlockedRules[routingEntryID(rule)] != "" {
			continue
		}
		if strings.HasPrefix(policy, "node-") {
			if !active[policy] {
				rule = replaceRulePolicy(rule, "REJECT")
			}
		} else if !known[policy] {
			return plan, fmt.Errorf("规则引用不存在的出口 %s", policy)
		}
		rules = append(rules, rule)
	}
	local := append([]CategoryRule{}, state.CategoryRules...)
	sort.SliceStable(local, func(i, j int) bool {
		if local[i].Position == local[j].Position {
			return local[i].ID < local[j].ID
		}
		return local[i].Position < local[j].Position
	})
	manual := []string{}
	for _, rule := range local {
		if !rule.Enabled || deleted[rule.Policy] {
			continue
		}
		policy := rule.Policy
		if !known[policy] && !active[policy] {
			policy = "REJECT"
		}
		text := rule.Kind + "," + rule.Value + "," + policy
		if rule.Kind == "IP-CIDR" || rule.Kind == "IP-CIDR6" {
			text += ",no-resolve"
		}
		manual = append(manual, text)
	}
	plan.Rules = append(manual, rules...)
	if len(plan.Rules) == 0 || !strings.HasPrefix(plan.Rules[len(plan.Rules)-1], "MATCH,") {
		plan.Rules = append(plan.Rules, "MATCH,REJECT")
	}
	usedProviders := routingProviderRefs(plan.Rules)
	for name := range plan.Providers {
		if initialProviders[name] && !usedProviders[name] {
			delete(plan.Providers, name)
		}
	}
	orderSelections(plan.Groups, state.Selections)
	sets := []RuleSet{}
	for _, rule := range plan.Rules {
		parts := splitRule(rule)
		if len(parts) >= 3 && parts[0] == "RULE-SET" {
			if p, ok := plan.Providers[parts[1]]; ok {
				sets = append(sets, RuleSet{Name: parts[1], Behavior: stringOf(p["behavior"]), Policy: parts[2], Enabled: true})
			}
		}
	}
	if plan.DNS == nil || state.ActiveRoutingSource == "" || !state.activeRoutingSource().ImportDNS {
		plan.DNS = buildDNSForGroups(state.Routing, sets, plan.Groups)
	}
	if !state.Routing.DNSEnabled {
		plan.DNS = nil
	}
	if plan.DNS != nil && state.Routing.DNSEnabled {
		policies := map[string]any{}
		if existing, ok := plan.DNS["nameserver-policy"].(map[string]any); ok {
			for k, v := range existing {
				if strings.HasPrefix(k, "rule-set:") {
					names := []string{}
					for _, name := range strings.Split(strings.TrimPrefix(k, "rule-set:"), ",") {
						if plan.Providers[name] != nil {
							names = append(names, name)
						}
					}
					if len(names) == 0 {
						continue
					}
					k = "rule-set:" + strings.Join(names, ",")
				}
				policies[k] = v
			}
		}
		for _, rule := range local {
			if !rule.Enabled || deleted[rule.Policy] || (rule.Kind != "DOMAIN" && rule.Kind != "DOMAIN-SUFFIX") {
				continue
			}
			resolvers := state.Routing.DNSDomestic
			if foreignPolicy(rule.Policy, plan.Groups, map[string]bool{}) {
				resolvers = state.Routing.DNSForeign
			}
			key := rule.Value
			if rule.Kind == "DOMAIN-SUFFIX" {
				key = "+." + key
			}
			if len(resolvers) > 0 {
				policies[key] = toAny(resolvers)
			}
		}
		if len(policies) > 0 {
			plan.DNS["nameserver-policy"] = policies
		}
	}
	return plan, nil
}

func validateGroupGraph(groups []map[string]any) error {
	if len(groups) > 512 {
		return errors.New("代理组超过 512 个")
	}
	byName := map[string]map[string]any{}
	for _, g := range groups {
		byName[stringOf(g["name"])] = g
	}
	color := map[string]int{}
	var visit func(string) error
	visit = func(name string) error {
		if color[name] == 1 {
			return fmt.Errorf("代理组 %s 存在循环引用", name)
		}
		if color[name] == 2 {
			return nil
		}
		color[name] = 1
		for _, member := range stringsOf(byName[name]["proxies"]) {
			if _, ok := byName[member]; ok {
				if err := visit(member); err != nil {
					return err
				}
			}
		}
		color[name] = 2
		return nil
	}
	for name := range byName {
		if err := visit(name); err != nil {
			return err
		}
	}
	return nil
}

func normalizeCategoryRule(rule *CategoryRule) error {
	rule.Value = strings.TrimSpace(rule.Value)
	if rule.Position < 0 || rule.Position > 100000 {
		return errors.New("优先级须在 0–100000 之间")
	}
	switch rule.Kind {
	case "DOMAIN", "DOMAIN-SUFFIX":
		rule.Value = strings.ToLower(strings.TrimSuffix(rule.Value, "."))
		if rule.Kind == "DOMAIN-SUFFIX" {
			rule.Value = strings.TrimPrefix(strings.TrimPrefix(rule.Value, "*."), "+.")
		}
		if rule.Value == "" || len(rule.Value) > 253 || strings.ContainsAny(rule.Value, " /\\,:;\r\n\t*+()") || strings.Contains(rule.Value, "..") || strings.HasPrefix(rule.Value, ".") {
			return errors.New("请输入域名，例如 example.com，不要包含协议、端口或路径")
		}
		if _, err := netip.ParseAddr(rule.Value); err == nil {
			return errors.New("IP 地址请选择 IP / CIDR 类型")
		}
	case "IP-CIDR", "IP-CIDR6", "IP":
		prefix, err := netip.ParsePrefix(rule.Value)
		if err != nil {
			addr, e := netip.ParseAddr(rule.Value)
			if e != nil {
				return errors.New("请输入有效 IPv4、IPv6 地址或 CIDR 网段")
			}
			if addr.Zone() != "" {
				return errors.New("IP 规则不能包含网卡区域标识")
			}
			prefix = netip.PrefixFrom(addr, addr.BitLen())
		}
		rule.Value = prefix.Masked().String()
		rule.Kind = "IP-CIDR"
		if prefix.Addr().Is6() {
			rule.Kind = "IP-CIDR6"
		}
	default:
		return errors.New("请选择域名、域名后缀或 IP / CIDR")
	}
	return nil
}

func routingEntries(state State) []RoutingEntry {
	blocked := state.BlockedRules
	state.BlockedRules = nil
	state.Routing.Enabled = true
	state.Listeners = []Listener{{Mode: ListenerModeRule, Enabled: true}}
	local := state.CategoryRules
	state.CategoryRules = nil
	plan, err := compileRouting(state)
	if err != nil {
		return nil
	}
	result := []RoutingEntry{}
	deleted := map[string]bool{}
	for _, e := range state.CategoryEdits {
		if e.Deleted {
			deleted[e.Name] = true
		}
	}
	for _, rule := range local {
		if deleted[rule.Policy] {
			continue
		}
		text := rule.Kind + "," + rule.Value + "," + rule.Policy
		result = append(result, RoutingEntry{ID: rule.ID, Policy: rule.Policy, Kind: rule.Kind, Value: rule.Value, Text: text, Local: true, Enabled: rule.Enabled, Position: rule.Position})
	}
	for i, rule := range plan.Rules {
		parts := splitRule(rule)
		id := routingEntryID(rule)
		value := ""
		if len(parts) > 2 {
			value = parts[1]
		}
		result = append(result, RoutingEntry{ID: id, Text: rule, Policy: rulePolicy(rule), Kind: parts[0], Value: value, Enabled: blocked[id] == "", Position: i})
	}
	// Deleted upstream entries can still be restored after the next source update.
	seen := map[string]bool{}
	for _, e := range result {
		seen[e.ID] = true
	}
	for id, text := range blocked {
		if !seen[id] {
			parts := splitRule(text)
			if len(parts) > 2 && !deleted[rulePolicy(text)] {
				result = append(result, RoutingEntry{ID: id, Text: text, Policy: rulePolicy(text), Kind: parts[0], Value: parts[1]})
			}
		}
	}
	return result
}

func BuildRouting(state State, _ []string) ([]map[string]any, []string, map[string]map[string]any, map[string]any, []MergeReport) {
	plan, err := compileRouting(state)
	if err != nil {
		return nil, nil, nil, nil, nil
	}
	return plan.Groups, plan.Rules, plan.Providers, plan.DNS, plan.Reports
}

func allowedCategoryMember(state State, member string) bool {
	if builtinOutbounds[member] {
		return true
	}
	for _, node := range state.routingNodes() {
		if "node-"+node.ID == member {
			return true
		}
	}
	return slices.Contains(policyOptions(state), member)
}

func effectiveRuleSets(state State, plan RoutingPlan) []RuleSet {
	deleted := map[string]bool{}
	for _, edit := range state.CategoryEdits {
		if edit.Deleted {
			deleted[edit.Name] = true
		}
	}
	if state.ActiveRoutingSource == "" {
		sets := []RuleSet{}
		for _, set := range state.RuleSets {
			if !deleted[set.Policy] {
				sets = append(sets, set)
			}
		}
		return sets
	}
	sets := []RuleSet{}
	seen := map[string]bool{}
	for i, rule := range plan.Rules {
		parts := splitRule(rule)
		if len(parts) < 3 || parts[0] != "RULE-SET" {
			continue
		}
		key := parts[1] + ":" + parts[2]
		if seen[key] {
			continue
		}
		seen[key] = true
		p := plan.Providers[parts[1]]
		sets = append(sets, RuleSet{ID: "external-" + digestBytes([]byte(key))[:16], Name: parts[1], Policy: parts[2], Behavior: stringOf(p["behavior"]), Format: stringOf(p["format"]), URL: stringOf(p["url"]), Interval: 86400, Enabled: true, Position: i})
	}
	return sets
}

func routingProviderRefs(rules []string) map[string]bool {
	refs := map[string]bool{}
	for _, rule := range rules {
		if !strings.Contains(rule, "RULE-SET,") {
			continue
		}
		for _, match := range providerReference.FindAllStringSubmatch(rule, -1) {
			refs[match[1]] = true
		}
	}
	return refs
}
