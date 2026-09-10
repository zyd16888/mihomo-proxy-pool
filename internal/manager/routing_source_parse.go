package manager

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

type RoutingSourcePreview struct {
	Token      string            `json:"token,omitempty"`
	Source     RoutingSource     `json:"source"`
	GroupNames []string          `json:"groupNames"`
	Mappings   map[string]string `json:"mappings"`
	Unresolved []string          `json:"unresolved"`
	Errors     []string          `json:"errors"`
	Ignored    []string          `json:"ignored"`
	Changed    bool              `json:"changed"`
}

func sourceURL(value string) error {
	u, err := url.Parse(value)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return errors.New("请输入 HTTP(S) 订阅地址")
	}
	return nil
}

func validateRoutingSource(source *RoutingSource) error {
	source.Name = strings.TrimSpace(source.Name)
	source.URL = strings.TrimSpace(source.URL)
	if source.Name == "" || len([]rune(source.Name)) > 120 {
		return errors.New("方案名称须为 1–120 字符")
	}
	if err := sourceURL(source.URL); err != nil {
		return err
	}
	if source.Interval == 0 {
		source.Interval = 86400
	}
	if source.Interval < 60 || source.Interval > 30*86400 {
		return errors.New("更新间隔须为 60 秒到 30 天")
	}
	if source.SourceIDs == nil {
		source.SourceIDs = []string{}
	}
	if source.Bindings == nil {
		source.Bindings = map[string]string{}
	}
	return nil
}

func parseRoutingDocument(raw []byte, source RoutingSource, state State) (RoutingDocument, RoutingSourcePreview, error) {
	doc := RoutingDocument{Groups: []map[string]any{}, Rules: []string{}, Providers: map[string]map[string]any{}}
	report := RoutingSourcePreview{Source: source, GroupNames: []string{}, Mappings: map[string]string{}, Unresolved: []string{}, Errors: []string{}, Ignored: []string{}}
	var root map[string]any
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&root); err != nil || root == nil {
		return doc, report, errors.New("订阅不是有效的 YAML 对象")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return doc, report, errors.New("订阅只能包含一个 YAML 文档")
	}
	for key := range root {
		if !slices.Contains([]string{"proxy-groups", "rules", "rule-providers", "proxies", "proxy-providers", "dns"}, key) {
			report.Ignored = append(report.Ignored, key)
		}
	}
	if _, ok := root["dns"]; ok && !source.ImportDNS {
		report.Ignored = append(report.Ignored, "dns（使用管理器 DNS）")
	}
	if root["proxy-providers"] != nil {
		report.Ignored = append(report.Ignored, "proxy-providers（使用关联的本地节点）")
	}
	sort.Strings(report.Ignored)
	state.ActiveRoutingSource = source.ID
	state.RoutingSources = []RoutingSource{source}
	pool := state.routingNodes()
	byID := map[string]Node{}
	for _, node := range pool {
		byID[node.ID] = node
	}
	for _, id := range source.SourceIDs {
		found := id == ""
		for _, sub := range state.Subscriptions {
			if sub.ID == id {
				found = true
			}
		}
		if !found {
			return doc, report, errors.New("关联的节点订阅已不存在，请重新选择")
		}
	}
	remoteFingerprints := map[string]string{}
	informationNodes := map[string]bool{}
	if list, ok := root["proxies"].([]any); ok {
		for _, entry := range list {
			proxy, ok := entry.(map[string]any)
			if !ok {
				return doc, report, errors.New("proxies 必须是节点对象数组")
			}
			name := stringOf(proxy["name"])
			copy := map[string]any{}
			for k, v := range proxy {
				if k != "name" {
					copy[k] = v
				}
			}
			data, _ := json.Marshal(copy)
			remoteFingerprints[name] = digestBytes(data)
			if stringOf(proxy["server"]) == "127.0.0.1" && stringOf(proxy["password"]) == "dummy" {
				informationNodes[name] = true
			}
		}
	}
	resolveNode := func(name string) (string, bool) {
		if id := source.Bindings[name]; id != "" {
			if _, ok := byID[id]; ok {
				return "node-" + id, true
			}
			return "", false
		}
		identity := []string{}
		names := []string{}
		for _, n := range pool {
			if remoteFingerprints[name] != "" && remoteFingerprints[name] == digestBytes(n.Config) {
				identity = append(identity, n.ID)
			}
			match := n.Name == name || name == "node-"+n.ID
			for _, sub := range state.Subscriptions {
				if sub.ID == n.SourceID && name == "["+sub.Name+"] "+n.Name {
					match = true
				}
			}
			if !match && strings.HasPrefix(name, "[") {
				if end := strings.Index(name, "] "); end > 0 && name[end+2:] == n.Name {
					match = true
				}
			}
			if match {
				names = append(names, n.ID)
			}
		}
		if len(identity) == 1 {
			return "node-" + identity[0], true
		}
		if len(names) == 1 {
			return "node-" + names[0], true
		}
		return "", false
	}
	groups, ok := root["proxy-groups"].([]any)
	if !ok || len(groups) == 0 {
		return doc, report, errors.New("方案必须包含非空 proxy-groups")
	}
	if len(groups) > 512 {
		return doc, report, errors.New("代理组超过 512 个")
	}
	rawRules, ok := root["rules"].([]any)
	if !ok || len(rawRules) == 0 {
		return doc, report, errors.New("方案必须包含非空 rules")
	}
	groupNames := map[string]bool{}
	ignoredGroups := map[string]bool{}
	for _, entry := range groups {
		group, ok := entry.(map[string]any)
		if !ok {
			return doc, report, errors.New("proxy-groups 必须是对象数组")
		}
		name := stringOf(group["name"])
		if name == "" || strings.ContainsAny(name, ",\r\n") || name == ExitProbeGroup || name == "GLOBAL" || strings.HasPrefix(name, "node-") || builtinOutbounds[name] || groupNames[name] {
			return doc, report, fmt.Errorf("代理组名称无效、重复或保留：%s", name)
		}
		members := stringsOf(group["proxies"])
		infoOnly := len(members) > 0
		for _, member := range members {
			if !informationNodes[member] {
				infoOnly = false
			}
		}
		if infoOnly {
			ignoredGroups[name] = true
			report.Ignored = append(report.Ignored, "订阅信息组："+name)
			continue
		}
		groupNames[name] = true
	}
	for _, entry := range groups {
		group := entry.(map[string]any)
		name := stringOf(group["name"])
		if ignoredGroups[name] {
			continue
		}
		kind := stringOf(group["type"])
		if kind == "" {
			kind = "select"
		}
		if !slices.Contains([]string{"select", "url-test", "fallback", "load-balance"}, kind) {
			return doc, report, fmt.Errorf("代理组 %s 的类型 %s 暂不支持", name, kind)
		}
		copy := map[string]any{"name": name, "type": kind}
		for _, key := range []string{"url", "interval", "tolerance", "lazy", "timeout", "max-failed-times", "expected-status", "disable-udp", "strategy", "filter", "exclude-filter", "include-all", "include-all-proxies"} {
			if v, ok := group[key]; ok {
				copy[key] = v
			}
		}
		for _, key := range []string{"filter", "exclude-filter"} {
			if pattern := stringOf(copy[key]); pattern != "" {
				if _, err := regexp.Compile(pattern); err != nil {
					return doc, report, fmt.Errorf("分组 %s 的 %s 正则无效", name, key)
				}
			}
		}
		if group["use"] != nil || truthy(group["include-all-providers"]) {
			copy["include-all-proxies"] = true
		}
		if kind != "select" {
			if copy["url"] == nil {
				copy["url"] = "https://www.gstatic.com/generate_204"
			}
			if copy["interval"] == nil {
				copy["interval"] = 300
			}
		}
		members := []string{}
		for _, member := range stringsOf(group["proxies"]) {
			if ignoredGroups[member] {
				continue
			}
			if groupNames[member] || builtinOutbounds[member] {
				members = append(members, member)
				continue
			}
			resolved, ok := resolveNode(member)
			if !ok {
				report.Unresolved = append(report.Unresolved, member)
				continue
			}
			members = append(members, resolved)
			report.Mappings[member] = strings.TrimPrefix(resolved, "node-")
		}
		copy["proxies"] = toAny(members)
		doc.Groups = append(doc.Groups, copy)
		report.GroupNames = append(report.GroupNames, name)
	}
	if providers, ok := root["rule-providers"].(map[string]any); ok {
		for name, entry := range providers {
			provider, ok := entry.(map[string]any)
			if !ok || name == "" || strings.ContainsAny(name, ",\r\n") {
				return doc, report, errors.New("rule-providers 必须是有效的命名对象")
			}
			kind := stringOf(provider["type"])
			if kind != "http" && kind != "inline" {
				return doc, report, fmt.Errorf("规则集 %s 仅支持 http 或 inline 来源", name)
			}
			behavior := stringOf(provider["behavior"])
			if !slices.Contains([]string{"domain", "ipcidr", "classical"}, behavior) {
				return doc, report, fmt.Errorf("规则集 %s 的 behavior 无效", name)
			}
			format := stringOf(provider["format"])
			if format == "" {
				format = "yaml"
			}
			if !slices.Contains([]string{"yaml", "text", "mrs"}, format) || format == "mrs" && behavior == "classical" {
				return doc, report, fmt.Errorf("规则集 %s 的 format 与 behavior 不兼容", name)
			}
			copy := map[string]any{"type": kind, "behavior": behavior, "format": format}
			if kind == "http" {
				address := stringOf(provider["url"])
				if err := sourceURL(address); err != nil {
					return doc, report, fmt.Errorf("规则集 %s 地址无效", name)
				}
				copy["url"] = address
				copy["interval"] = 86400
				if v, ok := provider["interval"]; ok {
					copy["interval"] = v
				}
				if header, ok := provider["header"]; ok {
					copy["header"] = header
				}
			} else {
				payload, ok := provider["payload"].([]any)
				if !ok {
					return doc, report, fmt.Errorf("inline 规则集 %s 缺少 payload", name)
				}
				copy["payload"] = payload
			}
			doc.Providers[name] = copy
		}
	}
	for i, entry := range rawRules {
		rule, ok := entry.(string)
		if !ok {
			return doc, report, fmt.Errorf("第 %d 条规则不是字符串", i+1)
		}
		rule = strings.TrimSpace(rule)
		parts := splitRule(rule)
		if len(parts) == 2 && (parts[0] == "MATCH" || parts[0] == "FINAL") {
			parts[0] = "MATCH"
			if i != len(rawRules)-1 {
				return doc, report, errors.New("MATCH/FINAL 必须位于规则末尾")
			}
		} else if len(parts) < 3 || !ruleTypes[parts[0]] {
			return doc, report, fmt.Errorf("第 %d 条规则类型或格式不支持", i+1)
		}
		if geoRules[parts[0]] && !state.Routing.AllowGeoRules {
			return doc, report, errors.New("方案包含 GEOIP/GEOSITE，请先在分流设置中允许 GEO 规则，或使用 rule-providers 版本")
		}
		rule = strings.Join(parts, ",")
		policy := rulePolicy(rule)
		if ignoredGroups[policy] {
			return doc, report, errors.New("流量规则指向订阅信息组，无法导入")
		}
		if !groupNames[policy] && !builtinOutbounds[policy] {
			resolved, ok := resolveNode(policy)
			if !ok {
				report.Unresolved = append(report.Unresolved, policy)
			} else {
				report.Mappings[policy] = strings.TrimPrefix(resolved, "node-")
				rule = replaceRulePolicy(rule, resolved)
			}
		}
		if parts[0] == "RULE-SET" {
			if _, ok := doc.Providers[parts[1]]; !ok {
				return doc, report, fmt.Errorf("第 %d 条规则引用不存在的规则集 %s", i+1, parts[1])
			}
		}
		doc.Rules = append(doc.Rules, rule)
	}
	sort.Strings(report.Unresolved)
	report.Unresolved = slices.Compact(report.Unresolved)
	if len(report.Unresolved) > 0 {
		return doc, report, errors.New("部分节点名称无法唯一匹配，请指定节点映射后重新预览")
	}
	if source.ImportDNS {
		dns, ok := root["dns"].(map[string]any)
		if !ok {
			return doc, report, errors.New("方案没有有效 DNS 配置")
		}
		doc.DNS = map[string]any{}
		for _, key := range []string{"enable", "ipv6", "prefer-h3", "default-nameserver", "nameserver", "fallback", "proxy-server-nameserver", "direct-nameserver", "nameserver-policy", "fallback-filter"} {
			if value, ok := dns[key]; ok {
				doc.DNS[key] = value
			}
		}
		for key := range dns {
			if _, copied := doc.DNS[key]; !copied {
				report.Ignored = append(report.Ignored, "dns."+key)
			}
		}
		doc.DNS["enhanced-mode"] = "redir-host"
		doc.DNS["respect-rules"] = false
		if policies, ok := dns["nameserver-policy"].(map[string]any); ok {
			for key := range policies {
				if strings.HasPrefix(key, "geosite:") && !state.Routing.AllowGeoRules {
					return doc, report, errors.New("方案 DNS 引用 GEOSITE，请允许 GEO 规则或取消导入 DNS")
				}
			}
		}
		if filter, ok := doc.DNS["fallback-filter"].(map[string]any); ok && truthy(filter["geoip"]) && !state.Routing.AllowGeoRules {
			return doc, report, errors.New("方案 DNS 的 fallback-filter 使用 GEOIP，请允许 GEO 规则或取消导入 DNS")
		}
		if doc.DNS["proxy-server-nameserver"] == nil {
			doc.DNS["proxy-server-nameserver"] = toAny(state.Routing.DNSDomestic)
		}
	}
	return doc, report, nil
}
