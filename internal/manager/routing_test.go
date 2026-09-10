package manager

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func routingStore(t *testing.T) (*Store, string) {
	t.Helper()
	s := storeForTest(t)
	requireOK(t, s.SaveSubscription(background, Subscription{Name: "airport", URL: "https://example.test/sub"}))
	source := snapshot(t, s).Subscriptions[0].ID
	return s, source
}

func ruleListener(t *testing.T, s *Store, port int) {
	t.Helper()
	requireOK(t, s.SaveListener(background, Listener{Name: "rule", Port: port, Mode: ListenerModeRule, Enabled: true}))
}

// builtConfig is the generated configuration read back as data. Group names
// carry emoji, which YAML escapes, so assertions parse rather than match text.
type builtConfig struct {
	Listeners []map[string]any          `yaml:"listeners"`
	Proxies   []map[string]any          `yaml:"proxies"`
	Groups    []map[string]any          `yaml:"proxy-groups"`
	Providers map[string]map[string]any `yaml:"rule-providers"`
	Rules     []string                  `yaml:"rules"`
	DNS       map[string]any            `yaml:"dns"`
}

func buildFor(t *testing.T, s *Store) builtConfig {
	t.Helper()
	raw, _, err := BuildConfig(snapshot(t, s), "127.0.0.1:9090", "secret")
	requireOK(t, err)
	var cfg builtConfig
	requireOK(t, yaml.Unmarshal(raw, &cfg))
	return cfg
}

func (c builtConfig) ruleAt(rule string) int {
	for i, item := range c.Rules {
		if item == rule {
			return i
		}
	}
	return -1
}

func (c builtConfig) ruleWithPrefix(prefix string) (string, int) {
	for i, item := range c.Rules {
		if strings.HasPrefix(item, prefix) {
			return item, i
		}
	}
	return "", -1
}

func (c builtConfig) group(name string) map[string]any {
	for _, group := range c.Groups {
		if stringOf(group["name"]) == name {
			return group
		}
	}
	return nil
}

func (c builtConfig) groupNames() []string {
	names := []string{}
	for _, group := range c.Groups {
		names = append(names, stringOf(group["name"]))
	}
	return names
}

func (c builtConfig) members(t *testing.T, name string) []string {
	t.Helper()
	group := c.group(name)
	if group == nil {
		t.Fatalf("group %q missing", name)
	}
	return stringsOf(group["proxies"])
}

func intOfAny(value any) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	}
	return 0
}

func TestRoutingStaysOffWithoutRuleListener(t *testing.T) {
	s, _ := routingStore(t)
	requireOK(t, s.Import(background, "", parsed(t, nodeJSON("A", "8881"))))
	node := snapshot(t, s).Nodes[0]
	requireOK(t, s.SaveListener(background, Listener{Name: "fixed", Port: 17891, Mode: ListenerModeNode, NodeID: node.ID, Enabled: true}))

	cfg := buildFor(t, s)
	// An unused feature must not pull rule sets or publish an open fallthrough.
	if len(cfg.Providers) != 0 || len(cfg.Groups) != 0 || cfg.DNS != nil {
		t.Fatal("routing emitted groups, providers or DNS without a rule listener")
	}
	if len(cfg.Rules) != 1 || cfg.Rules[0] != "MATCH,REJECT" {
		t.Fatalf("fallthrough is not a rejection: %v", cfg.Rules)
	}
}

func containsAny(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

func providerNames(cfg builtConfig) []string {
	names := []string{}
	for name := range cfg.Providers {
		names = append(names, name)
	}
	return names
}

func TestRoutingCannotBeDisabledUnderAnActiveRuleListener(t *testing.T) {
	s, _ := routingStore(t)
	ruleListener(t, s, 17892)
	routing := snapshot(t, s).Routing
	routing.Enabled = false
	if err := s.SaveRouting(background, routing); err == nil {
		t.Fatal("disabled routing while a rule listener was still serving")
	}
	listener := snapshot(t, s).Listeners[0]
	listener.Enabled = false
	requireOK(t, s.SaveListener(background, listener))
	requireOK(t, s.SaveRouting(background, routing))
}

func TestRuleListenerNeedsNoNodeAndKeepsPortOnRebind(t *testing.T) {
	s, _ := routingStore(t)
	requireOK(t, s.Import(background, "", parsed(t, nodeJSON("A", "8881"))))
	ruleListener(t, s, 17892)
	listener := snapshot(t, s).Listeners[0]
	if listener.NodeID != "" {
		t.Fatal("rule listener bound a node")
	}
	// Switching a port between modes must not need a delete and recreate.
	listener.Mode = ListenerModeNode
	listener.NodeID = snapshot(t, s).Nodes[0].ID
	requireOK(t, s.SaveListener(background, listener))
	updated := snapshot(t, s).Listeners[0]
	if updated.Port != 17892 || updated.Mode != ListenerModeNode || updated.NodeID == "" {
		t.Fatal("mode change lost the port or the binding")
	}
}

func TestSplitRuleKeepsLogicalPayloadIntact(t *testing.T) {
	cases := map[string][]string{
		"DOMAIN,example.com,DIRECT":                 {"DOMAIN", "example.com", "DIRECT"},
		"IP-CIDR,1.2.3.0/24,DIRECT,no-resolve":      {"IP-CIDR", "1.2.3.0/24", "DIRECT", "no-resolve"},
		"AND,((DOMAIN,a.com),(NETWORK,tcp)),REJECT": {"AND", "((DOMAIN,a.com),(NETWORK,tcp))", "REJECT"},
		"NOT,((DOMAIN-SUFFIX,b.com)),DIRECT":        {"NOT", "((DOMAIN-SUFFIX,b.com))", "DIRECT"},
	}
	for rule, want := range cases {
		got := splitRule(rule)
		if len(got) != len(want) {
			t.Fatalf("%s split into %v", rule, got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%s field %d is %q, want %q", rule, i, got[i], want[i])
			}
		}
	}
}

// An existing installation has a listeners table with a NOT NULL node_id and
// no mode column, neither of which SQLite can alter in place.
func TestUpgradeFromAPreRuleListenerDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manager.db")
	legacy, err := sql.Open("sqlite", path)
	requireOK(t, err)
	for _, stmt := range []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE nodes(id TEXT PRIMARY KEY,name TEXT NOT NULL,source_id TEXT NOT NULL,identity TEXT NOT NULL,protocol TEXT NOT NULL,server TEXT NOT NULL,port INTEGER NOT NULL,enabled INTEGER NOT NULL DEFAULT 1,available INTEGER NOT NULL DEFAULT 1,config TEXT NOT NULL,UNIQUE(source_id,name))`,
		`CREATE TABLE listeners(id TEXT PRIMARY KEY,name TEXT NOT NULL,port INTEGER NOT NULL UNIQUE CHECK(port BETWEEN 1 AND 65535),node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE RESTRICT,enabled INTEGER NOT NULL DEFAULT 1)`,
		`INSERT INTO nodes VALUES('n1','A','','identity','http','127.0.0.1',8881,1,1,'{"type":"http","server":"127.0.0.1","port":8881}')`,
		`INSERT INTO listeners VALUES('l1','existing',17891,'n1',1)`,
	} {
		if _, err := legacy.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	requireOK(t, legacy.Close())

	store, err := OpenStore(path)
	requireOK(t, err)
	defer store.Close()
	state := snapshot(t, store)

	// The existing binding must survive, and must keep behaving as before.
	if len(state.Listeners) != 1 {
		t.Fatalf("migration lost listeners: %+v", state.Listeners)
	}
	existing := state.Listeners[0]
	if existing.Port != 17891 || existing.NodeID != "n1" || existing.Mode != ListenerModeNode {
		t.Fatalf("migrated listener changed meaning: %+v", existing)
	}
	// Upgrades initialize settings without installing routing rules.
	if !state.Routing.Enabled || len(state.RuleSets) != 0 {
		t.Fatal("upgrade unexpectedly installed routing rules")
	}
	requireOK(t, store.SaveListener(background, Listener{Name: "rule", Port: 17892, Mode: ListenerModeRule, Enabled: true}))
	if _, _, err := BuildConfig(snapshot(t, store), "127.0.0.1:9090", "secret"); err != nil {
		t.Fatal(err)
	}
	// The foreign key must still be enforced after the table rebuild.
	if err := store.DeleteNode(background, "n1"); err == nil {
		t.Fatal("migrated table lost its reference protection")
	}
}

// The generated routing configuration is only useful if the kernel accepts it.
// Opt in with MIHOMO_TEST_BIN; no external target is contacted by -t.

func TestNoSubscriptionParksRuleListenersAndKeepsFixedListeners(t *testing.T) {
	s := storeForTest(t)
	requireOK(t, s.Import(background, "", parsed(t, nodeJSON("A", "8881"))))
	node := snapshot(t, s).Nodes[0]
	ruleListener(t, s, 17892)
	requireOK(t, s.SaveListener(background, Listener{Name: "fixed", Port: 17893, NodeID: node.ID, Enabled: true}))
	state := snapshot(t, s)
	if len(state.RuleSets) != 0 || len(routingPreview(state)) != 0 {
		t.Fatal("fresh database contains defaults")
	}
	cfg := buildFor(t, s)
	if len(cfg.Listeners) != 1 || intOfAny(cfg.Listeners[0]["port"]) != 17893 || stringOf(cfg.Listeners[0]["proxy"]) != "node-"+node.ID {
		t.Fatalf("fixed listener changed or rule listener opened: %+v", cfg.Listeners)
	}
	if (len(cfg.Rules) != 1 || cfg.Rules[0] != "MATCH,REJECT") || len(cfg.Providers) != 0 {
		t.Fatal("unconfigured scheme generated rules")
	}
	if err := s.SaveCategoryEdit(background, "", CategoryEdit{Label: "Forbidden", Kind: "select", AllNodes: true}); err == nil {
		t.Fatal("allowed category without a subscription")
	}
	if err := s.SaveCategoryRule(background, "", CategoryRule{Kind: "DOMAIN", Value: "example.com", Policy: "DIRECT"}); err == nil {
		t.Fatal("allowed rule without a subscription")
	}
}

func TestUpgradeRetiresOnlyMarkedBuiltins(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manager.db")
	s, err := OpenStore(path)
	requireOK(t, err)
	for _, item := range []struct {
		id      string
		builtin int
	}{{"old-default", 1}, {"user-rule", 0}} {
		_, err = s.db.Exec(`INSERT INTO rule_sets(id,name,policy,behavior,format,url,interval,no_resolve,enabled,position,builtin) VALUES(?,?,'DIRECT','domain','yaml','https://example.test/rules',86400,0,1,100,?)`, item.id, item.id, item.builtin)
		requireOK(t, err)
	}
	requireOK(t, s.Close())
	s, err = OpenStore(path)
	requireOK(t, err)
	defer s.Close()
	state := snapshot(t, s)
	if len(state.RuleSets) != 1 || state.RuleSets[0].ID != "user-rule" {
		t.Fatalf("upgrade removed user data or kept defaults: %+v", state.RuleSets)
	}
	ruleListener(t, s, 17892)
	if cfg := buildFor(t, s); len(cfg.Providers) != 0 || len(cfg.Listeners) != 0 {
		t.Fatal("archived local data silently became active")
	}
}

func TestBootstrapRetiresLegacyRulePortsWithoutChangingFixedConfig(t *testing.T) {
	m, _, _ := sourceManager(t)
	legacy := `listeners:
 - {name: old-rule, type: mixed, listen: 0.0.0.0, port: 17892}
 - {name: fixed, type: mixed, listen: 0.0.0.0, port: 17893, proxy: node-A}
proxies:
 - {name: node-A, type: http, server: 127.0.0.1, port: 8881}
proxy-groups:
 - {name: old-default, type: select, proxies: [node-A]}
rule-providers:
 old: {type: http, url: 'https://example.test/rules'}
rules: [MATCH,old-default]
dns: {enable: true}
`
	requireOK(t, os.WriteFile(filepath.Join(m.Dir, "last-good.yaml"), []byte(legacy), 0600))
	requireOK(t, m.Bootstrap(background))
	for _, name := range []string{"config.yaml", "last-good.yaml"} {
		raw, err := os.ReadFile(filepath.Join(m.Dir, name))
		requireOK(t, err)
		var cfg builtConfig
		requireOK(t, yaml.Unmarshal(raw, &cfg))
		if len(cfg.Listeners) != 1 || stringOf(cfg.Listeners[0]["proxy"]) != "node-A" || len(cfg.Proxies) != 1 || len(cfg.Groups) != 0 || len(cfg.Providers) != 0 || len(cfg.DNS) != 0 {
			t.Fatalf("%s: bad migration: %+v", name, cfg)
		}
	}
}
