package manager

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"mihomo-proxy/internal/importer"
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

func TestRuleListenerOmitsProxyAndFixedListenerKeepsIt(t *testing.T) {
	s, _ := routingStore(t)
	requireOK(t, s.Import(background, "", parsed(t, nodeJSON("A", "8881"))))
	node := snapshot(t, s).Nodes[0]
	requireOK(t, s.SaveListener(background, Listener{Name: "fixed", Port: 17891, Mode: ListenerModeNode, NodeID: node.ID, Enabled: true}))
	ruleListener(t, s, 17892)

	state := snapshot(t, s)
	_, ports, err := BuildConfig(state, "127.0.0.1:9090", "secret")
	requireOK(t, err)
	if len(ports) != 2 {
		t.Fatalf("expected both listeners exposed, got %v", ports)
	}
	cfg := buildFor(t, s)
	byPort := map[int]map[string]any{}
	for _, listener := range cfg.Listeners {
		byPort[intOfAny(listener["port"])] = listener
	}
	// The pinned listener names its outbound; the rule listener must not, or
	// its traffic would bypass the rule engine.
	if stringOf(byPort[17891]["proxy"]) != "node-"+node.ID {
		t.Fatal("fixed listener lost its node binding")
	}
	if _, pinned := byPort[17892]["proxy"]; pinned {
		t.Fatal("rule listener pinned an outbound")
	}
	if cfg.ruleAt("MATCH,"+GroupFinal) < 0 {
		t.Fatal("rule listener without a fallthrough rule")
	}
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

func TestGeneratedRulesAreOrderedByPriority(t *testing.T) {
	s, _ := routingStore(t)
	ruleListener(t, s, 17892)
	cfg := buildFor(t, s)
	order := []string{
		"IP-CIDR,127.0.0.0/8,DIRECT,no-resolve",
		"RULE-SET,private-domain," + GroupDirect,
		"RULE-SET,reject-ads," + GroupReject,
		"RULE-SET,proxy-domain," + GroupProxy,
		"RULE-SET,cn-domain," + GroupDirect,
		"MATCH," + GroupFinal,
	}
	previous := -1
	for _, rule := range order {
		at := cfg.ruleAt(rule)
		if at < 0 {
			t.Fatalf("missing rule %q in %v", rule, cfg.Rules)
		}
		if at <= previous {
			t.Fatalf("rule %q is out of priority order", rule)
		}
		previous = at
	}
	// The fallthrough must be last, or the rules after it are unreachable.
	if cfg.ruleAt("MATCH,"+GroupFinal) != len(cfg.Rules)-1 {
		t.Fatal("fallthrough is not the last rule")
	}
}

func TestGeneratedConfigAvoidsGeoRules(t *testing.T) {
	s, source := routingStore(t)
	ruleListener(t, s, 17892)
	requireOK(t, s.ImportSubscription(background, source, parsed(t, nodeJSON("A", "8881")), importer.Profile{
		Rules: []string{"GEOIP,CN,DIRECT", "GEOSITE,youtube,PROXY", "DOMAIN-SUFFIX,example.com,DIRECT"},
	}))
	cfg := buildFor(t, s)
	// GEOIP and GEOSITE force the kernel to download the geo databases during
	// validation, which the apply path cannot wait for.
	for _, rule := range cfg.Rules {
		if strings.HasPrefix(rule, "GEOIP,") || strings.HasPrefix(rule, "GEOSITE,") {
			t.Fatalf("configuration kept the geo rule %q", rule)
		}
	}
	for key := range cfg.DNS["nameserver-policy"].(map[string]any) {
		if strings.Contains(key, "geosite:") {
			t.Fatalf("DNS policy kept the geo key %q", key)
		}
	}
	if cfg.ruleAt("DOMAIN-SUFFIX,example.com,DIRECT") < 0 {
		t.Fatal("dropped a rule that needs no geo data")
	}
}

func TestSubscriptionGroupCollisionIsNamespaced(t *testing.T) {
	s := storeForTest(t)
	for _, name := range []string{"alpha", "beta"} {
		requireOK(t, s.SaveSubscription(background, Subscription{Name: name, URL: "https://example.test/" + name}))
	}
	subs := snapshot(t, s).Subscriptions
	for _, sub := range subs {
		requireOK(t, s.ImportSubscription(background, sub.ID, parsed(t, nodeJSON(sub.Name+"-A", "8881")), importer.Profile{
			// Both subscriptions ship a group under the same name, and one of
			// them collides with a generated group as well.
			Groups: []map[string]any{
				{"name": "Streaming", "type": "select", "proxies": []any{sub.Name + "-A"}},
				{"name": GroupSelect, "type": "select", "proxies": []any{sub.Name + "-A"}},
			},
			Rules: []string{"DOMAIN-SUFFIX," + sub.Name + ".test,Streaming"},
		}))
	}
	ruleListener(t, s, 17892)
	cfg := buildFor(t, s)

	seen := map[string]int{}
	for _, name := range cfg.groupNames() {
		seen[name]++
	}
	for name, count := range seen {
		if count > 1 {
			t.Fatalf("duplicate group name %q survived the merge", name)
		}
	}
	// The first subscription keeps the plain name; the second is namespaced.
	if cfg.group("Streaming") == nil || cfg.group("[beta] Streaming") == nil {
		t.Fatalf("colliding group was not namespaced by subscription: %v", cfg.groupNames())
	}
	// A generated group always wins its own name.
	if members := cfg.members(t, GroupSelect); !containsAny(members, "DIRECT") {
		t.Fatal("generated group was overwritten by a subscription group")
	}
	if cfg.group("[alpha] "+GroupSelect) == nil || cfg.group("[beta] "+GroupSelect) == nil {
		t.Fatalf("collision with a generated group was not namespaced: %v", cfg.groupNames())
	}
	if cfg.ruleAt("DOMAIN-SUFFIX,alpha.test,Streaming") < 0 {
		t.Fatal("rule lost its policy after renaming")
	}
	if cfg.ruleAt("DOMAIN-SUFFIX,beta.test,[beta] Streaming") < 0 {
		t.Fatalf("rule was not repointed at the renamed group: %v", cfg.Rules)
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

func TestGroupMembersResolveToNodesAndDropWhenEmpty(t *testing.T) {
	s, source := routingStore(t)
	ruleListener(t, s, 17892)
	requireOK(t, s.ImportSubscription(background, source, parsed(t, `[{"name":"HK 01","type":"http","server":"127.0.0.1","port":8881},{"name":"JP 01","type":"http","server":"127.0.0.1","port":8882}]`), importer.Profile{
		Groups: []map[string]any{
			{"name": "HK", "type": "select", "filter": "HK", "include-all": true},
			{"name": "Gone", "type": "select", "proxies": []any{"missing node"}},
			{"name": "Nested", "type": "select", "proxies": []any{"Gone"}},
		},
		Rules: []string{"DOMAIN,a.test,HK", "DOMAIN,b.test,Gone", "DOMAIN,c.test,Nested"},
	}))
	byName := map[string]string{}
	for _, n := range snapshot(t, s).Nodes {
		byName[n.Name] = "node-" + n.ID
	}
	cfg := buildFor(t, s)

	// A name filter cannot survive into the kernel: every proxy is renamed to
	// node-<id>, so the filter is resolved here instead.
	hk := cfg.group("HK")
	if hk == nil {
		t.Fatal("filtered group was dropped")
	}
	for _, key := range []string{"filter", "include-all", "use", "exclude-filter"} {
		if _, present := hk[key]; present {
			t.Fatalf("group kept %q instead of resolving it", key)
		}
	}
	if members := cfg.members(t, "HK"); len(members) != 1 || members[0] != byName["HK 01"] {
		t.Fatalf("filter selected %v, want only the HK node", members)
	}
	if cfg.group("Gone") != nil || cfg.group("Nested") != nil {
		t.Fatal("group with no reachable member survived")
	}
	if cfg.ruleAt("DOMAIN,a.test,HK") < 0 {
		t.Fatal("rule for a resolvable group was dropped")
	}
	for _, dropped := range []string{"DOMAIN,b.test", "DOMAIN,c.test"} {
		if rule, at := cfg.ruleWithPrefix(dropped); at >= 0 {
			t.Fatalf("rule %q kept a policy that no longer exists", rule)
		}
	}
}

func TestMergedRulesLandBetweenAdBlockingAndTheDomesticSet(t *testing.T) {
	s, source := routingStore(t)
	ruleListener(t, s, 17892)
	requireOK(t, s.ImportSubscription(background, source, parsed(t, nodeJSON("A", "8881")), importer.Profile{
		Rules: []string{"DOMAIN-SUFFIX,netflix.com,DIRECT", "MATCH,DIRECT", "DOMAIN-SUFFIX,netflix.com,REJECT"},
	}))
	cfg := buildFor(t, s)
	merged := cfg.ruleAt("DOMAIN-SUFFIX,netflix.com,DIRECT")
	if merged < 0 {
		t.Fatalf("subscription rule was not merged: %v", cfg.Rules)
	}
	// Service rules from a subscription must beat the broad domestic and
	// foreign sets, but never the local ad-blocking decision.
	if merged < cfg.ruleAt("RULE-SET,reject-ads,"+GroupReject) {
		t.Fatal("merged rules outrank ad blocking")
	}
	if merged > cfg.ruleAt("RULE-SET,cn-domain,"+GroupDirect) {
		t.Fatal("merged rules fall after the domestic set")
	}
	// A subscription MATCH would swallow every later rule.
	if cfg.ruleAt("MATCH,DIRECT") >= 0 {
		t.Fatal("subscription MATCH rule was merged")
	}
	if cfg.ruleAt("DOMAIN-SUFFIX,netflix.com,REJECT") >= 0 {
		t.Fatal("duplicate rule value was merged twice")
	}
}

func TestSubscriptionRuleProvidersAreRenamedAndGivenUniquePaths(t *testing.T) {
	s := storeForTest(t)
	for _, name := range []string{"alpha", "beta"} {
		requireOK(t, s.SaveSubscription(background, Subscription{Name: name, URL: "https://example.test/" + name}))
	}
	for _, sub := range snapshot(t, s).Subscriptions {
		requireOK(t, s.ImportSubscription(background, sub.ID, parsed(t, nodeJSON(sub.Name+"-A", "8881")), importer.Profile{
			Providers: map[string]map[string]any{
				// Both use the name of a generated set and of each other.
				"cn-domain": {"type": "http", "behavior": "domain", "format": "yaml", "url": "https://example.test/cn.yaml", "path": "./shared.yaml"},
			},
			Rules: []string{"RULE-SET,cn-domain,DIRECT"},
		}))
	}
	ruleListener(t, s, 17892)
	cfg := buildFor(t, s)

	// Both subscriptions名 their set after a generated one, so both must move.
	for _, name := range []string{"[alpha] cn-domain", "[beta] cn-domain"} {
		if cfg.Providers[name] == nil {
			t.Fatalf("provider %q was dropped instead of renamed: %v", name, providerNames(cfg))
		}
	}
	if stringOf(cfg.Providers["cn-domain"]["url"]) != ruleSetBase+"/geosite/cn.mrs" {
		t.Fatal("a subscription provider overwrote the generated set")
	}
	paths := map[string]bool{}
	for name, provider := range cfg.Providers {
		path := stringOf(provider["path"])
		if paths[path] {
			t.Fatalf("provider %q shares an on-disk path with another set", name)
		}
		paths[path] = true
	}
	if cfg.ruleAt("RULE-SET,[alpha] cn-domain,DIRECT") < 0 {
		t.Fatalf("rule was not repointed at the renamed provider: %v", cfg.Rules)
	}
}

func providerNames(cfg builtConfig) []string {
	names := []string{}
	for name := range cfg.Providers {
		names = append(names, name)
	}
	return names
}

func TestDisabledRuleSetStillReservesItsName(t *testing.T) {
	s, source := routingStore(t)
	ruleListener(t, s, 17892)
	for _, set := range snapshot(t, s).RuleSets {
		if set.Name == "cn-domain" {
			set.Enabled = false
			requireOK(t, s.SaveRuleSet(background, set))
		}
	}
	requireOK(t, s.ImportSubscription(background, source, parsed(t, nodeJSON("A", "8881")), importer.Profile{
		Providers: map[string]map[string]any{
			"cn-domain": {"type": "http", "behavior": "domain", "format": "yaml", "url": "https://example.test/theirs.yaml"},
		},
		Rules: []string{"RULE-SET,cn-domain,DIRECT"},
	}))
	cfg := buildFor(t, s)

	// Taking a disabled set's name would silently repoint this subscription's
	// rules at different content the moment that set is enabled again.
	if cfg.Providers["cn-domain"] != nil {
		t.Fatal("subscription provider claimed a disabled rule set's name")
	}
	if cfg.Providers["[airport] cn-domain"] == nil {
		t.Fatalf("subscription provider was dropped instead of renamed: %v", providerNames(cfg))
	}
	if cfg.ruleAt("RULE-SET,[airport] cn-domain,DIRECT") < 0 {
		t.Fatalf("rule was not repointed: %v", cfg.Rules)
	}
}

func TestRuleSetsDownloadDirectlyByDefault(t *testing.T) {
	s, _ := routingStore(t)
	ruleListener(t, s, 17892)
	cfg := buildFor(t, s)
	// Rule provider downloads run through the rule engine themselves. Sending
	// them through a node the rules cannot pick yet never resolves.
	for _, set := range DefaultRuleSets() {
		provider := cfg.Providers[set.Name]
		if provider == nil {
			t.Fatalf("default rule set %s missing", set.Name)
		}
		if stringOf(provider["url"]) != set.URL {
			t.Fatalf("rule set %s points at %q", set.Name, provider["url"])
		}
		if stringOf(provider["proxy"]) != "DIRECT" {
			t.Fatalf("rule set %s downloads through the proxy it is meant to configure", set.Name)
		}
	}
}

func TestDNSPairsForeignResolversWithTheProxyRuleSets(t *testing.T) {
	s, _ := routingStore(t)
	ruleListener(t, s, 17892)
	cfg := buildFor(t, s)
	policy, ok := cfg.DNS["nameserver-policy"].(map[string]any)
	if !ok || policy["rule-set:proxy-domain"] == nil {
		t.Fatalf("foreign resolvers are not keyed on the sets that route abroad: %v", cfg.DNS)
	}
	if cfg.DNS["proxy-server-nameserver"] == nil {
		t.Fatal("node hostnames have no resolver, which deadlocks on first connect")
	}
	// Resolving through the rules would need the proxy that DNS is resolving.
	if respect, _ := cfg.DNS["respect-rules"].(bool); respect {
		t.Fatal("DNS resolves through the rules it is meant to feed")
	}
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

func TestRuleSetValidationRejectsUnusableCombinations(t *testing.T) {
	s, _ := routingStore(t)
	base := RuleSet{Name: "custom", Policy: GroupProxy, Behavior: "domain", Format: "mrs", URL: "https://example.test/a.mrs", Interval: 86400, Enabled: true, Position: 400}
	requireOK(t, s.SaveRuleSet(background, base))

	broken := map[string]RuleSet{
		"name collision":     {Name: "cn-domain", Policy: GroupProxy, Behavior: "domain", Format: "mrs", URL: "https://example.test/b.mrs", Interval: 86400},
		"mrs classical":      {Name: "c1", Policy: GroupProxy, Behavior: "classical", Format: "mrs", URL: "https://example.test/b.mrs", Interval: 86400},
		"unknown policy":     {Name: "c2", Policy: "nowhere", Behavior: "domain", Format: "mrs", URL: "https://example.test/b.mrs", Interval: 86400},
		"non http url":       {Name: "c3", Policy: GroupProxy, Behavior: "domain", Format: "mrs", URL: "file:///etc/passwd", Interval: 86400},
		"unsafe name":        {Name: "../escape", Policy: GroupProxy, Behavior: "domain", Format: "mrs", URL: "https://example.test/b.mrs", Interval: 86400},
		"interval too short": {Name: "c4", Policy: GroupProxy, Behavior: "domain", Format: "mrs", URL: "https://example.test/b.mrs", Interval: 5},
	}
	for reason, set := range broken {
		if err := s.SaveRuleSet(background, set); err == nil {
			t.Fatalf("accepted a rule set with %s", reason)
		}
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
	// The upgrade also seeds routing, so the new feature is usable at once.
	if !state.Routing.Enabled || len(state.RuleSets) != len(DefaultRuleSets()) {
		t.Fatal("upgrade did not seed routing defaults")
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

func TestSeededRuleSetsAreNotRestoredAfterRemoval(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manager.db")
	store, err := OpenStore(path)
	requireOK(t, err)
	for _, set := range snapshot(t, store).RuleSets {
		requireOK(t, store.DeleteRuleSet(background, set.ID))
	}
	requireOK(t, store.Close())

	store, err = OpenStore(path)
	requireOK(t, err)
	defer store.Close()
	if sets := snapshot(t, store).RuleSets; len(sets) != 0 {
		t.Fatalf("restart restored %d removed rule sets", len(sets))
	}
}

// The generated routing configuration is only useful if the kernel accepts it.
// Opt in with MIHOMO_TEST_BIN; no external target is contacted by -t.
func TestRoutingConfigPassesKernelValidation(t *testing.T) {
	binary := os.Getenv("MIHOMO_TEST_BIN")
	if binary == "" {
		t.Skip("set MIHOMO_TEST_BIN to validate generated routing against the kernel")
	}
	binary, err := filepath.Abs(binary)
	requireOK(t, err)
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, "manager.db"))
	requireOK(t, err)
	defer store.Close()
	requireOK(t, store.SaveSubscription(background, Subscription{Name: "airport", URL: "https://example.test/sub"}))
	source := snapshot(t, store).Subscriptions[0].ID
	requireOK(t, store.ImportSubscription(background, source, parsed(t, `[{"name":"HK 01","type":"http","server":"127.0.0.1","port":8881},{"name":"JP 01","type":"http","server":"127.0.0.1","port":8882}]`), importer.Profile{
		Groups: []map[string]any{
			{"name": "Streaming", "type": "select", "proxies": []any{"HK 01", "JP 01", "DIRECT"}},
			{"name": GroupSelect, "type": "url-test", "filter": "HK", "include-all": true, "url": "https://www.gstatic.com/generate_204", "interval": 300},
		},
		Rules: []string{
			"DOMAIN-SUFFIX,netflix.com,Streaming",
			"IP-CIDR,203.0.113.0/24,Streaming,no-resolve",
			"AND,((DOMAIN,a.test),(NETWORK,tcp)),DIRECT",
			"GEOIP,CN,DIRECT",
			"RULE-SET,house,DIRECT",
		},
		Providers: map[string]map[string]any{
			"house": {"type": "http", "behavior": "domain", "format": "yaml", "url": "https://example.test/house.yaml"},
		},
	}))
	requireOK(t, store.SaveListener(background, Listener{Name: "rule", Port: 17892, Mode: ListenerModeRule, Enabled: true}))
	requireOK(t, store.SaveListener(background, Listener{Name: "fixed", Port: 17891, Mode: ListenerModeNode, NodeID: snapshot(t, store).Nodes[0].ID, Enabled: true}))

	raw, _, err := BuildConfig(snapshot(t, store), "127.0.0.1:9090", "test-secret")
	requireOK(t, err)
	path := filepath.Join(dir, "candidate.yaml")
	requireOK(t, os.WriteFile(path, raw, 0600))
	kernel := NewRuntime(binary, dir, "http://127.0.0.1:9090", "test-secret")
	requireOK(t, kernel.Validate(background, path))
	// Validation must not have needed the geo databases; downloading them
	// inside the apply path is what the geo rule filtering exists to avoid.
	for _, name := range []string{"geoip.metadb", "GeoSite.dat", "geosite.db", "country.mmdb"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			t.Fatalf("validation downloaded %s", name)
		}
	}
}
