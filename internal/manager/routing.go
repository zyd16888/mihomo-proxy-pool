package manager

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"mihomo-proxy/internal/importer"
)

// Built-in group names remain stable across database and configuration updates.
const (
	GroupSelect = "🚀 节点选择"
	GroupAuto   = "♻️ 自动选择"
	GroupDirect = "🎯 国内直连"
	GroupProxy  = "🌍 国外代理"
	GroupReject = "🛑 广告拦截"
	GroupFinal  = "🐟 漏网之鱼"
)

// PolicyTargets is the baseline; policyOptions adds custom and subscription groups.
var PolicyTargets = []string{GroupProxy, GroupDirect, GroupReject, GroupSelect, GroupFinal, "DIRECT", "REJECT"}

// builtinOutbounds are accepted verbatim as a rule policy without existing as
// a generated group.
var builtinOutbounds = map[string]bool{
	"DIRECT": true, "REJECT": true, "REJECT-DROP": true, "PASS": true, "COMPATIBLE": true,
}

// Private ranges are matched before any rule set so that LAN and loopback
// traffic never waits on a remote rule provider or a DNS lookup.
var privateRules = []string{
	"IP-CIDR,127.0.0.0/8,DIRECT,no-resolve",
	"IP-CIDR,10.0.0.0/8,DIRECT,no-resolve",
	"IP-CIDR,172.16.0.0/12,DIRECT,no-resolve",
	"IP-CIDR,192.168.0.0/16,DIRECT,no-resolve",
	"IP-CIDR,169.254.0.0/16,DIRECT,no-resolve",
	"IP-CIDR,100.64.0.0/10,DIRECT,no-resolve",
	"IP-CIDR6,::1/128,DIRECT,no-resolve",
	"IP-CIDR6,fc00::/7,DIRECT,no-resolve",
	"IP-CIDR6,fe80::/10,DIRECT,no-resolve",
}

const ruleSetBase = "https://cdn.jsdelivr.net/gh/MetaCubeX/meta-rules-dat@meta/geo"

// DefaultRuleSets implements 国内直连 / 国外代理 / 广告拦截. Ad blocking uses the
// Loyalsoldier list because the geosite advertising category carries only about
// nine hundred entries against its one hundred and eighty thousand.
func DefaultRuleSets() []RuleSet {
	return []RuleSet{
		{Name: "private-domain", Policy: GroupDirect, Behavior: "domain", Format: "mrs", URL: ruleSetBase + "/geosite/private.mrs", Position: 100},
		{Name: "private-ip", Policy: GroupDirect, Behavior: "ipcidr", Format: "mrs", URL: ruleSetBase + "/geoip/private.mrs", Position: 110, NoResolve: true},
		{Name: "reject-ads", Policy: GroupReject, Behavior: "domain", Format: "yaml", URL: "https://cdn.jsdelivr.net/gh/Loyalsoldier/clash-rules@release/reject.txt", Position: 200},
		{Name: "proxy-domain", Policy: GroupProxy, Behavior: "domain", Format: "mrs", URL: ruleSetBase + "/geosite/geolocation-!cn.mrs", Position: 600},
		{Name: "cn-domain", Policy: GroupDirect, Behavior: "domain", Format: "mrs", URL: ruleSetBase + "/geosite/cn.mrs", Position: 700},
		{Name: "cn-ip", Policy: GroupDirect, Behavior: "ipcidr", Format: "mrs", URL: ruleSetBase + "/geoip/cn.mrs", Position: 710, NoResolve: true},
	}
}

func DefaultRouting() Routing {
	return Routing{
		Enabled:         true,
		DefaultPolicy:   GroupProxy,
		MergeSubRules:   true,
		SubRulePosition: 500,
		AllowGeoRules:   false,
		RuleSetProxy:    "DIRECT",
		DNSEnabled:      true,
		DNSDomestic:     []string{"https://223.5.5.5/dns-query", "https://doh.pub/dns-query"},
		DNSForeign:      []string{"https://1.1.1.1/dns-query", "https://8.8.8.8/dns-query"},
	}
}

var ruleSetNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

func ValidateRuleSet(r RuleSet) error {
	if !ruleSetNamePattern.MatchString(r.Name) {
		return fmt.Errorf("规则集名称 %q 只能使用字母、数字、下划线、点和连字符，长度 1–64", r.Name)
	}
	switch r.Behavior {
	case "domain", "ipcidr", "classical":
	default:
		return fmt.Errorf("规则集 %s 的 behavior 必须是 domain、ipcidr 或 classical", r.Name)
	}
	switch r.Format {
	case "mrs", "yaml", "text":
	default:
		return fmt.Errorf("规则集 %s 的 format 必须是 mrs、yaml 或 text", r.Name)
	}
	if r.Format == "mrs" && r.Behavior == "classical" {
		return fmt.Errorf("规则集 %s：mrs 格式不支持 classical", r.Name)
	}
	if !strings.HasPrefix(r.URL, "http://") && !strings.HasPrefix(r.URL, "https://") {
		return fmt.Errorf("规则集 %s 的地址必须是 HTTP 或 HTTPS URL", r.Name)
	}
	if r.Interval < 60 || r.Interval > 30*24*3600 {
		return fmt.Errorf("规则集 %s 的更新间隔必须在 60 秒到 30 天之间", r.Name)
	}
	if strings.TrimSpace(r.Policy) == "" || strings.ContainsAny(r.Policy, ",\r\n") {
		return fmt.Errorf("规则集 %s 的策略名称 %q 为空或含有逗号、换行", r.Name, r.Policy)
	}
	return nil
}

// ruleTypes lists the rule keywords accepted from a subscription. Anything
// outside this set is dropped rather than passed through, because an unknown
// keyword fails kernel validation and would block the whole configuration.
var ruleTypes = map[string]bool{
	"DOMAIN": true, "DOMAIN-SUFFIX": true, "DOMAIN-KEYWORD": true, "DOMAIN-REGEX": true,
	"IP-CIDR": true, "IP-CIDR6": true, "IP-SUFFIX": true, "IP-ASN": true,
	"SRC-IP-CIDR": true, "SRC-IP-SUFFIX": true, "SRC-IP-ASN": true,
	"DST-PORT": true, "SRC-PORT": true, "IN-PORT": true, "IN-TYPE": true, "IN-USER": true, "IN-NAME": true,
	"PROCESS-NAME": true, "PROCESS-PATH": true, "PROCESS-NAME-REGEX": true, "PROCESS-PATH-REGEX": true,
	"NETWORK": true, "DSCP": true, "UID": true, "RULE-SET": true,
	"AND": true, "OR": true, "NOT": true,
	"GEOIP": true, "GEOSITE": true, "SRC-GEOIP": true,
}

// geoRules need the GeoIP database or GeoSite data. Keeping them out of the
// generated configuration is what lets kernel validation stay offline and fast.
var geoRules = map[string]bool{"GEOIP": true, "GEOSITE": true, "SRC-GEOIP": true}

// splitRule splits TYPE,VALUE,POLICY,OPTION while keeping the parenthesised
// payload of AND / OR / NOT intact, since that payload contains commas.
func splitRule(rule string) []string {
	rule = strings.TrimSpace(rule)
	comma := strings.Index(rule, ",")
	if comma < 0 {
		return []string{rule}
	}
	parts := []string{strings.TrimSpace(rule[:comma])}
	rest := strings.TrimSpace(rule[comma+1:])
	if strings.HasPrefix(rest, "(") {
		depth, end := 0, -1
		for i := 0; i < len(rest) && end < 0; i++ {
			switch rest[i] {
			case '(':
				depth++
			case ')':
				depth--
				if depth == 0 {
					end = i
				}
			}
		}
		if end < 0 {
			return nil
		}
		parts = append(parts, rest[:end+1])
		rest = strings.TrimSpace(rest[end+1:])
		rest = strings.TrimSpace(strings.TrimPrefix(rest, ","))
	}
	if rest != "" {
		for _, field := range strings.Split(rest, ",") {
			parts = append(parts, strings.TrimSpace(field))
		}
	}
	return parts
}

// MergeReport explains what a subscription contributed and, more usefully,
// what was left out and why.
type MergeReport struct {
	Subscription string   `json:"subscription"`
	Groups       int      `json:"groups"`
	Rules        int      `json:"rules"`
	Providers    int      `json:"providers"`
	Renamed      []string `json:"renamed,omitempty"`
	DroppedGeo   int      `json:"droppedGeo"`
	DroppedRules int      `json:"droppedRules"`
	DroppedGroup int      `json:"droppedGroups"`
	Duplicates   int      `json:"duplicates"`
}

type routingBuild struct {
	groups    []map[string]any
	rules     []string
	providers map[string]map[string]any
	reports   []MergeReport
}

type nameRegistry struct {
	groups    map[string]bool
	providers map[string]bool
	seenRule  map[string]bool
}

// claim returns a free name, namespacing with the subscription name on a
// collision and then numbering if the namespaced form collides too.
func claim(used map[string]bool, source, name string) string {
	if !used[name] {
		used[name] = true
		return name
	}
	candidate := fmt.Sprintf("[%s] %s", source, name)
	for n := 2; used[candidate]; n++ {
		candidate = fmt.Sprintf("[%s] %s #%d", source, name, n)
	}
	used[candidate] = true
	return candidate
}

func groupNames(groups []map[string]any) []string {
	names := make([]string, 0, len(groups))
	for _, group := range groups {
		names = append(names, stringOf(group["name"]))
	}
	return names
}

func stringOf(value any) string {
	if value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return text
	}
	return fmt.Sprint(value)
}

func stringsOf(value any) []string {
	list, ok := value.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		if text := strings.TrimSpace(stringOf(item)); text != "" {
			out = append(out, text)
		}
	}
	return out
}

// mergeSubscription rewrites one subscription's groups, providers and rules
// into the generated namespace, dropping anything that cannot be resolved.
func mergeSubscription(build *routingBuild, reg *nameRegistry, routing Routing, sub Subscription, profile importer.Profile, nodes []Node) {
	report := MergeReport{Subscription: sub.Name}
	proxyToNode := map[string]string{}
	nodeNames := []string{}
	for _, n := range nodes {
		if n.SourceID == sub.ID && n.Enabled && n.Available {
			proxyToNode[n.Name] = "node-" + n.ID
			nodeNames = append(nodeNames, n.Name)
		}
	}

	groupRename := map[string]string{}
	for _, name := range groupNames(profile.Groups) {
		if name == "" {
			continue
		}
		claimed := claim(reg.groups, sub.Name, name)
		groupRename[name] = claimed
		if claimed != name {
			report.Renamed = append(report.Renamed, name+" → "+claimed)
		}
	}

	providerRename := map[string]string{}
	providerOrder := make([]string, 0, len(profile.Providers))
	for name := range profile.Providers {
		providerOrder = append(providerOrder, name)
	}
	sort.Strings(providerOrder)
	for _, name := range providerOrder {
		provider := profile.Providers[name]
		if !usableProvider(provider) {
			continue
		}
		claimed := claim(reg.providers, sub.Name, name)
		providerRename[name] = claimed
		copied := map[string]any{}
		for key, value := range provider {
			switch key {
			case "path", "proxy":
				continue
			default:
				copied[key] = value
			}
		}
		copied["path"] = "./ruleset/sub-" + sub.ID + "-" + sanitizePath(name) + fileSuffix(stringOf(provider["format"]))
		copied["proxy"] = routing.RuleSetProxy
		if _, ok := copied["interval"]; !ok {
			copied["interval"] = 86400
		}
		build.providers[claimed] = copied
		report.Providers++
	}

	resolved := resolveGroups(profile.Groups, groupRename, proxyToNode, nodeNames)
	for _, name := range groupNames(profile.Groups) {
		if _, ok := resolved[groupRename[name]]; !ok && name != "" {
			delete(groupRename, name)
			report.DroppedGroup++
		}
	}
	for _, group := range profile.Groups {
		claimed := groupRename[stringOf(group["name"])]
		if built, ok := resolved[claimed]; ok {
			build.groups = append(build.groups, built)
			report.Groups++
		}
	}

	for _, rule := range profile.Rules {
		converted, skipped := convertRule(rule, routing, groupRename, providerRename, reg.seenRule)
		if converted == "" {
			switch skipped {
			case "geo":
				report.DroppedGeo++
			case "duplicate":
				report.Duplicates++
			default:
				report.DroppedRules++
			}
			continue
		}
		build.rules = append(build.rules, converted)
		report.Rules++
	}
	build.reports = append(build.reports, report)
}

func usableProvider(provider map[string]any) bool {
	if stringOf(provider["type"]) != "http" {
		return false
	}
	url := stringOf(provider["url"])
	return strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://")
}

var unsafePath = regexp.MustCompile(`[^A-Za-z0-9_.-]`)

func sanitizePath(name string) string {
	cleaned := unsafePath.ReplaceAllString(name, "_")
	if len(cleaned) > 40 {
		cleaned = cleaned[:40]
	}
	if cleaned == "" {
		cleaned = "set"
	}
	return cleaned
}

func fileSuffix(format string) string {
	switch format {
	case "mrs":
		return ".mrs"
	case "text":
		return ".txt"
	default:
		return ".yaml"
	}
}

// candidateMembers lists a group's members under their original names.
// A filter or include-all group draws on the subscription's own nodes, which
// must be expanded here: the generated configuration renames every proxy to
// node-<id>, so a name filter evaluated by the kernel would match nothing.
func candidateMembers(group map[string]any, nodeNames []string) []string {
	members := stringsOf(group["proxies"])
	pullsNodes := truthy(group["include-all"]) || truthy(group["include-all-proxies"]) ||
		group["use"] != nil || group["filter"] != nil
	if !pullsNodes {
		return members
	}
	include := compilePattern(stringOf(group["filter"]))
	exclude := compilePattern(stringOf(group["exclude-filter"]))
	for _, name := range nodeNames {
		if include != nil && !include.MatchString(name) {
			continue
		}
		if exclude != nil && exclude.MatchString(name) {
			continue
		}
		members = append(members, name)
	}
	return members
}

func compilePattern(pattern string) *regexp.Regexp {
	if strings.TrimSpace(pattern) == "" {
		return nil
	}
	compiled, err := regexp.Compile(pattern)
	if err != nil {
		return nil
	}
	return compiled
}

// resolveGroups maps each group's members into the generated namespace and
// drops groups left with nothing to select. Dropping one group can empty
// another that referenced it, so removal repeats until the set is stable.
func resolveGroups(groups []map[string]any, groupRename map[string]string, proxyToNode map[string]string, nodeNames []string) map[string]map[string]any {
	type pending struct {
		attrs   map[string]any
		members []string
	}
	staged := map[string]*pending{}
	order := []string{}
	for _, group := range groups {
		claimed := groupRename[stringOf(group["name"])]
		if claimed == "" || staged[claimed] != nil {
			continue
		}
		attrs := map[string]any{}
		for key, value := range group {
			switch key {
			case "name", "proxies", "use", "filter", "exclude-filter", "exclude-type",
				"include-all", "include-all-proxies", "include-all-providers":
				continue
			default:
				attrs[key] = value
			}
		}
		attrs["name"] = claimed
		if stringOf(attrs["type"]) == "" {
			attrs["type"] = "select"
		}
		staged[claimed] = &pending{attrs: attrs, members: candidateMembers(group, nodeNames)}
		order = append(order, claimed)
	}

	for {
		dropped := false
		for _, claimed := range order {
			entry := staged[claimed]
			if entry == nil {
				continue
			}
			resolved := []string{}
			seen := map[string]bool{}
			for _, member := range entry.members {
				target := ""
				switch {
				case builtinOutbounds[strings.ToUpper(member)]:
					target = strings.ToUpper(member)
				case groupRename[member] != "" && staged[groupRename[member]] != nil:
					target = groupRename[member]
				case proxyToNode[member] != "":
					target = proxyToNode[member]
				}
				if target == "" || target == claimed || seen[target] {
					continue
				}
				seen[target] = true
				resolved = append(resolved, target)
			}
			if len(resolved) == 0 {
				delete(staged, claimed)
				dropped = true
				continue
			}
			entry.attrs["proxies"] = resolved
		}
		if !dropped {
			break
		}
	}

	built := map[string]map[string]any{}
	for claimed, entry := range staged {
		members, _ := entry.attrs["proxies"].([]string)
		list := make([]any, 0, len(members))
		for _, member := range members {
			list = append(list, member)
		}
		entry.attrs["proxies"] = list
		built[claimed] = entry.attrs
	}
	return built
}

func truthy(value any) bool {
	flag, ok := value.(bool)
	return ok && flag
}

// convertRule rewrites one subscription rule into the generated namespace and
// reports why it was dropped when it cannot be represented.
func convertRule(rule string, routing Routing, groupRename, providerRename map[string]string, seen map[string]bool) (string, string) {
	parts := splitRule(rule)
	if len(parts) < 3 {
		return "", "malformed"
	}
	kind := strings.ToUpper(parts[0])
	if !ruleTypes[kind] {
		return "", "unsupported"
	}
	if geoRules[kind] && !routing.AllowGeoRules {
		return "", "geo"
	}
	value := parts[1]
	if kind == "RULE-SET" {
		renamed := providerRename[value]
		if renamed == "" {
			return "", "unknown-provider"
		}
		value = renamed
	}
	policy := parts[2]
	switch {
	case builtinOutbounds[strings.ToUpper(policy)]:
		policy = strings.ToUpper(policy)
	case groupRename[policy] != "":
		policy = groupRename[policy]
	default:
		return "", "unknown-policy"
	}
	key := kind + "," + value
	if seen[key] {
		return "", "duplicate"
	}
	seen[key] = true
	out := kind + "," + value + "," + policy
	if len(parts) > 3 && parts[3] != "" {
		out += "," + parts[3]
	}
	return out, ""
}

// BuildRouting produces the proxy groups, rules, rule providers and DNS block
// for rule listeners. It returns nothing when routing is off or no rule
// listener is enabled, so an unused feature never downloads a rule set.
func BuildRouting(state State, activeNodes []string) (groups []map[string]any, rules []string, providers map[string]map[string]any, dns map[string]any, reports []MergeReport) {
	if !state.Routing.Enabled || !hasRuleListener(state) {
		return nil, nil, nil, nil, nil
	}
	routing := state.Routing
	build := &routingBuild{providers: map[string]map[string]any{}}
	reg := &nameRegistry{
		groups:    map[string]bool{},
		providers: map[string]bool{},
		seenRule:  map[string]bool{},
	}

	base := append(baseGroups(routing, activeNodes), customGroups(state, activeNodes)...)
	for _, group := range base {
		reg.groups[stringOf(group["name"])] = true
	}
	// The exit probe drives its own selector. A subscription group must not be
	// able to claim that name, or probing would redirect live traffic.
	reg.groups[ExitProbeGroup] = true

	sets := append([]RuleSet{}, state.RuleSets...)
	sort.SliceStable(sets, func(i, j int) bool {
		if sets[i].Position != sets[j].Position {
			return sets[i].Position < sets[j].Position
		}
		return sets[i].Name < sets[j].Name
	})
	// Disabled sets reserve their name too. Otherwise a subscription provider
	// could take it, and enabling the set later would silently repoint that
	// subscription's rules at different content.
	for _, set := range sets {
		reg.providers[set.Name] = true
	}

	if routing.MergeSubRules {
		profiles := map[string]importer.Profile{}
		for _, item := range state.Profiles {
			profiles[item.SubscriptionID] = item.Profile
		}
		for _, sub := range state.Subscriptions {
			profile, ok := profiles[sub.ID]
			if !ok || profile.Empty() {
				continue
			}
			mergeSubscription(build, reg, routing, sub, profile, state.Nodes)
		}
	}

	availablePolicies := map[string]bool{"DIRECT": true, "REJECT": true}
	for _, group := range append(append([]map[string]any{}, base...), build.groups...) {
		availablePolicies[stringOf(group["name"])] = true
	}
	// Removed subscription groups must not make the entire configuration invalid.
	for i := range sets {
		if !availablePolicies[sets[i].Policy] {
			sets[i].Policy = "REJECT"
		}
	}
	rules = append(rules, privateRules...)
	emitted := map[string]bool{}
	subRulesWritten := false
	for _, set := range sets {
		if !set.Enabled {
			continue
		}
		if !subRulesWritten && set.Position >= routing.SubRulePosition {
			rules = append(rules, build.rules...)
			subRulesWritten = true
		}
		if emitted[set.Name] {
			continue
		}
		emitted[set.Name] = true
		providersEntry := map[string]any{
			"type":     "http",
			"behavior": set.Behavior,
			"format":   set.Format,
			"url":      set.URL,
			"path":     "./ruleset/" + set.Name + fileSuffix(set.Format),
			"interval": set.Interval,
			"proxy":    routing.RuleSetProxy,
		}
		if build.providers == nil {
			build.providers = map[string]map[string]any{}
		}
		build.providers[set.Name] = providersEntry
		rule := "RULE-SET," + set.Name + "," + set.Policy
		if set.NoResolve && set.Behavior == "ipcidr" {
			rule += ",no-resolve"
		}
		rules = append(rules, rule)
	}
	if !subRulesWritten {
		rules = append(rules, build.rules...)
	}
	rules = append(rules, "MATCH,"+GroupFinal)

	groups = append(base, build.groups...)
	// Filter an unavailable saved default (for example a removed subscription group).
	for _, group := range groups {
		if stringOf(group["name"]) == GroupFinal {
			members := []any{}
			if !availablePolicies[state.Routing.DefaultPolicy] && state.Routing.DefaultPolicy != "" {
				members = append(members, "REJECT")
			}
			for _, member := range stringsOf(group["proxies"]) {
				if availablePolicies[member] && member != GroupFinal || strings.HasPrefix(member, "node-") {
					members = append(members, member)
				}
			}
			group["proxies"] = members
		}
	}
	orderSelections(groups, state.Selections)
	return groups, rules, build.providers, buildDNSForGroups(routing, sets, groups), build.reports
}

func hasRuleListener(state State) bool {
	for _, l := range state.Listeners {
		if l.Enabled && l.RuleMode() {
			return true
		}
	}
	return false
}

// baseGroups builds the six generated policy groups. Every group keeps DIRECT
// as a member so that a configuration with no usable node still validates.
func baseGroups(routing Routing, nodes []string) []map[string]any {
	selectMembers := []any{}
	if len(nodes) > 0 {
		selectMembers = append(selectMembers, GroupAuto)
	}
	for _, node := range nodes {
		selectMembers = append(selectMembers, node)
	}
	selectMembers = append(selectMembers, "DIRECT")

	groups := []map[string]any{
		{"name": GroupSelect, "type": "select", "proxies": selectMembers},
	}
	if len(nodes) > 0 {
		autoMembers := make([]any, 0, len(nodes))
		for _, node := range nodes {
			autoMembers = append(autoMembers, node)
		}
		groups = append(groups, map[string]any{
			"name": GroupAuto, "type": "url-test", "url": "https://www.gstatic.com/generate_204",
			"interval": 300, "tolerance": 50, "lazy": true, "proxies": autoMembers,
		})
	}
	finalFirst := routing.DefaultPolicy
	if finalFirst == "" || finalFirst == GroupFinal {
		finalFirst = GroupSelect
	}
	finalMembers := []any{finalFirst}
	if finalFirst == "DIRECT" {
		finalMembers = append(finalMembers, GroupSelect)
	} else {
		finalMembers = append(finalMembers, "DIRECT")
	}
	groups = append(groups,
		map[string]any{"name": GroupProxy, "type": "select", "proxies": append([]any{GroupSelect, "DIRECT"}, toAny(nodes)...)},
		map[string]any{"name": GroupDirect, "type": "select", "proxies": append([]any{"DIRECT", GroupSelect}, toAny(nodes)...)},
		map[string]any{"name": GroupReject, "type": "select", "proxies": []any{"REJECT", "DIRECT"}},
		map[string]any{"name": GroupFinal, "type": "select", "proxies": uniqueMembers(append(finalMembers, append([]any{GroupProxy, GroupDirect, GroupReject, "REJECT"}, toAny(nodes)...)...))},
	)
	return groups
}

// buildDNS pairs name resolution with routing: domestic resolvers by default,
// and the foreign resolvers for exactly the domain sets that route abroad.
// rule-set keys are used instead of geosite so that kernel validation never
// has to download the GeoSite database.
func buildDNSForGroups(routing Routing, sets []RuleSet, groups []map[string]any) map[string]any {
	if !routing.DNSEnabled {
		return nil
	}
	domestic := toAny(routing.DNSDomestic)
	if len(domestic) == 0 {
		domestic = toAny([]string{"https://223.5.5.5/dns-query"})
	}
	dns := map[string]any{
		"enable":                  true,
		"ipv6":                    false,
		"prefer-h3":               false,
		"enhanced-mode":           "redir-host",
		"respect-rules":           false,
		"default-nameserver":      []any{"223.5.5.5", "119.29.29.29"},
		"nameserver":              domestic,
		"proxy-server-nameserver": domestic,
	}
	foreignSets := []string{}
	for _, set := range sets {
		if set.Enabled && set.Behavior == "domain" && foreignPolicy(set.Policy, groups, map[string]bool{}) {
			foreignSets = append(foreignSets, set.Name)
		}
	}
	if len(foreignSets) > 0 && len(routing.DNSForeign) > 0 {
		dns["nameserver-policy"] = map[string]any{
			"rule-set:" + strings.Join(foreignSets, ","): toAny(routing.DNSForeign),
		}
	}
	return dns
}

func toAny(values []string) []any {
	out := make([]any, 0, len(values))
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
