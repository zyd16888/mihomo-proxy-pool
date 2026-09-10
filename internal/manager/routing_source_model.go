package manager

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

type RoutingSource struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	URL        string            `json:"url"`
	Interval   int               `json:"interval"`
	AutoUpdate bool              `json:"autoUpdate"`
	SourceIDs  []string          `json:"sourceIds"`
	Bindings   map[string]string `json:"bindings"`
	ImportDNS  bool              `json:"importDns"`
	Version    int               `json:"version"`
	Digest     string            `json:"digest"`
	CheckedAt  string            `json:"checkedAt"`
	UpdatedAt  string            `json:"updatedAt"`
	LastError  string            `json:"lastError"`
	Groups     int               `json:"groups"`
	Rules      int               `json:"rules"`
	Providers  int               `json:"providers"`
	Ignored    []string          `json:"ignored"`
	Document   RoutingDocument   `json:"-"`
}

// The saved document contains only routing data and bindings to local node IDs.
// Credentials and listeners from a complete Clash subscription are never saved.
type RoutingDocument struct {
	Groups    []map[string]any          `json:"groups"`
	Rules     []string                  `json:"rules"`
	Providers map[string]map[string]any `json:"providers"`
	DNS       map[string]any            `json:"dns,omitempty"`
}

type CategoryEdit struct {
	Name     string   `json:"name"`
	Label    string   `json:"label"`
	Kind     string   `json:"kind"`
	Members  []string `json:"members"`
	AllNodes bool     `json:"allNodes"`
	Deleted  bool     `json:"deleted"`
}

type CategoryRule struct {
	ReplacesText string `json:"replacesText,omitempty"`
	ID           string `json:"id"`
	Policy       string `json:"policy"`
	Kind         string `json:"kind"`
	Value        string `json:"value"`
	Enabled      bool   `json:"enabled"`
	Position     int    `json:"position"`
}

type RoutingEntry struct {
	ID       string `json:"id"`
	Text     string `json:"text"`
	Policy   string `json:"policy"`
	Kind     string `json:"kind"`
	Value    string `json:"value"`
	Local    bool   `json:"local"`
	Enabled  bool   `json:"enabled"`
	Position int    `json:"position"`
}

func digestBytes(data []byte) string    { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
func routingEntryID(rule string) string { return "upstream-" + digestBytes([]byte(rule))[:24] }
func rulePolicy(rule string) string {
	parts := splitRule(rule)
	if len(parts) == 2 && (parts[0] == "MATCH" || parts[0] == "FINAL") {
		return parts[1]
	}
	if len(parts) >= 3 {
		return parts[2]
	}
	return ""
}
func replaceRulePolicy(rule, policy string) string {
	parts := splitRule(rule)
	if len(parts) == 2 {
		parts[1] = policy
	} else if len(parts) >= 3 {
		parts[2] = policy
	}
	return strings.Join(parts, ",")
}
func (state State) activeRoutingSource() *RoutingSource {
	for i := range state.RoutingSources {
		if state.RoutingSources[i].ID == state.ActiveRoutingSource {
			return &state.RoutingSources[i]
		}
	}
	return nil
}
func (state State) routingNodes() []Node {
	source := state.activeRoutingSource()
	if source == nil || len(source.SourceIDs) == 0 {
		return state.Nodes
	}
	allowed := map[string]bool{}
	for _, id := range source.SourceIDs {
		allowed[id] = true
	}
	nodes := []Node{}
	for _, node := range state.Nodes {
		if allowed[node.SourceID] {
			nodes = append(nodes, node)
		}
	}
	return nodes
}
