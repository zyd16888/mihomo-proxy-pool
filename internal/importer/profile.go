package importer

import (
	"strings"

	"gopkg.in/yaml.v3"
)

// Profile carries the routing half of a Clash subscription: the parts that
// decide where traffic goes, as opposed to the proxies that carry it.
// Airport subscriptions frequently ship only proxies, so an empty profile is a
// normal result rather than an error.
type Profile struct {
	Groups    []map[string]any          `json:"groups"`
	Rules     []string                  `json:"rules"`
	Providers map[string]map[string]any `json:"providers"`
}

func (p Profile) Empty() bool {
	return len(p.Groups) == 0 && len(p.Rules) == 0 && len(p.Providers) == 0
}

// ParseProfile reads groups, rules and rule providers out of Clash YAML.
// Content that is not Clash YAML yields an empty profile without an error:
// the caller already reports unusable subscriptions through proxy parsing.
func ParseProfile(raw string) Profile {
	text := strings.TrimSpace(raw)
	if text == "" {
		return Profile{}
	}
	profile, ok := parseProfileYAML(text)
	if ok {
		return profile
	}
	decoded, err := decodeBase64Any(strings.Join(strings.Fields(text), ""))
	if err != nil {
		return Profile{}
	}
	profile, _ = parseProfileYAML(strings.TrimSpace(decoded))
	return profile
}

func parseProfileYAML(raw string) (Profile, bool) {
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(raw), &doc); err != nil || doc == nil {
		return Profile{}, false
	}
	normalized, ok := normalizeGenericValue(doc).(map[string]any)
	if !ok {
		return Profile{}, false
	}
	if _, isClash := normalized["proxies"]; !isClash {
		if _, hasRules := normalized["rules"]; !hasRules {
			return Profile{}, false
		}
	}
	profile := Profile{Providers: map[string]map[string]any{}}
	if list, ok := normalized["proxy-groups"].([]any); ok {
		for _, entry := range list {
			group, ok := entry.(map[string]any)
			if !ok {
				continue
			}
			if name := stringValue(group["name"]); name != "" {
				profile.Groups = append(profile.Groups, group)
			}
		}
	}
	if list, ok := normalized["rules"].([]any); ok {
		for _, entry := range list {
			if rule := strings.TrimSpace(stringValue(entry)); rule != "" {
				profile.Rules = append(profile.Rules, rule)
			}
		}
	}
	if providers, ok := normalized["rule-providers"].(map[string]any); ok {
		for name, entry := range providers {
			provider, ok := entry.(map[string]any)
			if !ok || strings.TrimSpace(name) == "" {
				continue
			}
			profile.Providers[strings.TrimSpace(name)] = provider
		}
	}
	return profile, true
}
