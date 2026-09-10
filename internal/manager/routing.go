package manager

import (
	"fmt"
	"regexp"
	"strings"
)

// builtinOutbounds are accepted verbatim as a rule policy without existing as
// a generated group.
var builtinOutbounds = map[string]bool{
	"DIRECT": true, "REJECT": true, "REJECT-DROP": true, "PASS": true, "COMPATIBLE": true,
}

func DefaultRouting() Routing {
	return Routing{
		Enabled:         true,
		DefaultPolicy:   "",
		MergeSubRules:   false,
		SubRulePosition: 0,
		AllowGeoRules:   false,
		RuleSetProxy:    "DIRECT",
		DNSEnabled:      true,
		DNSDomestic:     []string{"https://223.5.5.5/dns-query", "https://doh.pub/dns-query"},
		DNSForeign:      []string{"https://1.1.1.1/dns-query", "https://8.8.8.8/dns-query"},
	}
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

func truthy(value any) bool {
	flag, ok := value.(bool)
	return ok && flag
}

func hasRuleListener(state State) bool {
	for _, l := range state.Listeners {
		if l.Enabled && l.RuleMode() {
			return true
		}
	}
	return false
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
