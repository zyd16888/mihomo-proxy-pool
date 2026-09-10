package manager

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
	"mihomo-proxy/internal/importer"
)

func groupByName(t *testing.T, state State, name string) map[string]any {
	t.Helper()
	for _, group := range routingPreview(state) {
		if stringOf(group["name"]) == name {
			return group
		}
	}
	t.Fatalf("missing group %s", name)
	return nil
}

func TestGroupSelectionSurvivesReopenAndMissingNode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manager.db")
	s, err := OpenStore(path)
	requireOK(t, err)
	requireOK(t, s.Import(background, "", parsed(t, nodeJSON("A", "8881"))))
	requireOK(t, s.Import(background, "", parsed(t, nodeJSON("B", "8882"))))
	requireOK(t, s.AddRuleTemplates(background, []string{"category-ai-!cn", "youtube"}))
	state := snapshot(t, s)
	b := "node-" + state.Nodes[1].ID
	requireOK(t, s.SaveSelection(background, "AI 服务", b))
	requireOK(t, s.Close())
	s, err = OpenStore(path)
	requireOK(t, err)
	defer s.Close()
	state = snapshot(t, s)
	if state.Selections["AI 服务"] != b || stringsOf(groupByName(t, state, "AI 服务")["proxies"])[0] != b {
		t.Fatal("selection was not recovered")
	}
	requireOK(t, s.DeleteNode(background, state.Nodes[1].ID))
	state = snapshot(t, s)
	if slices.Contains(stringsOf(groupByName(t, state, "AI 服务")["proxies"]), b) {
		t.Fatal("removed node emitted")
	}
	server := &Server{Manager: &Manager{Store: s, Kernel: &fakeKernel{}}}
	views, _, _ := server.groupViews(background, state)
	for _, v := range views {
		if v.Name == "AI 服务" && v.Warning == "" {
			t.Fatal("missing selection was not reported")
		}
	}
}

func TestTemplatesAtomicPriorityAndGroupReferences(t *testing.T) {
	s, _ := routingStore(t)
	before := snapshot(t, s)
	if err := s.AddRuleTemplates(background, []string{"youtube", "unknown"}); err == nil {
		t.Fatal("accepted unknown template")
	}
	after := snapshot(t, s)
	if len(after.ProxyGroups) != 0 || len(after.RuleSets) != len(before.RuleSets) || after.Revision != before.Revision {
		t.Fatal("partial template import")
	}
	requireOK(t, s.AddRuleTemplates(background, []string{"netflix", "telegram"}))
	state := snapshot(t, s)
	if len(state.ProxyGroups) != 2 || len(state.RuleSets) != len(before.RuleSets)+4 {
		t.Fatal("templates did not create groups and domain/IP sets")
	}
	for _, set := range state.RuleSets {
		if set.Policy == "Netflix" || set.Policy == "Telegram" {
			if set.Position >= state.Routing.SubRulePosition {
				t.Fatal("specific rules shadowed by subscriptions")
			}
			if set.Behavior == "ipcidr" && !set.NoResolve {
				t.Fatal("IP set lost no-resolve")
			}
		}
	}
	if err := s.AddRuleTemplates(background, []string{"netflix"}); err == nil {
		t.Fatal("duplicate import overwrote config")
	}
	if err := s.DeleteProxyGroup(background, state.ProxyGroups[0].ID); err == nil {
		t.Fatal("deleted referenced group")
	}
}

func TestCustomGroupOrderAndDNSFollowSelection(t *testing.T) {
	s, _ := routingStore(t)
	requireOK(t, s.Import(background, "", parsed(t, nodeJSON("A", "8881"))))
	requireOK(t, s.Import(background, "", parsed(t, nodeJSON("B", "8882"))))
	state := snapshot(t, s)
	requireOK(t, s.SaveProxyGroup(background, ProxyGroup{Name: "Work", Kind: "fallback", NodeIDs: []string{state.Nodes[1].ID, state.Nodes[0].ID}}))
	group := snapshot(t, s).ProxyGroups[0]
	if stringsOf(groupByName(t, snapshot(t, s), "Work")["proxies"])[0] != "node-"+state.Nodes[1].ID {
		t.Fatal("fallback order lost")
	}
	if err := s.SaveSelection(background, "Work", "node-"+state.Nodes[0].ID); err == nil {
		t.Fatal("manual selection accepted for automatic group")
	}
	group.Kind = "select"
	requireOK(t, s.SaveProxyGroup(background, group))
	requireOK(t, s.SaveRuleSet(background, RuleSet{Name: "work", Policy: "Work", Behavior: "domain", Format: "yaml", URL: "https://example.test/work", Interval: 86400, Enabled: true, Position: 300}))
	requireOK(t, s.SaveSelection(background, "Work", "node-"+state.Nodes[0].ID))
	state = snapshot(t, s)
	groups := routingPreview(state)
	if !foreignPolicy("Work", groups, map[string]bool{}) {
		t.Fatal("custom node group not resolved as foreign DNS")
	}
	requireOK(t, s.SaveSelection(background, "Work", "DIRECT"))
	if foreignPolicy("Work", routingPreview(snapshot(t, s)), map[string]bool{}) {
		t.Fatal("direct override retained foreign DNS")
	}
	if err := s.SaveSelection(background, "Work", "non-member"); err == nil {
		t.Fatal("invalid selection accepted")
	}
	if err := s.SaveSelection(background, ExitProbeGroup, "DIRECT"); err == nil {
		t.Fatal("internal probe exposed")
	}
	routing := state.Routing
	routing.DefaultPolicy = "Work"
	requireOK(t, s.SaveRouting(background, routing))
	if stringsOf(groupByName(t, snapshot(t, s), GroupFinal)["proxies"])[0] != "Work" {
		t.Fatal("default policy collapsed to generic node group")
	}
	routing.DefaultPolicy = GroupFinal
	if err := s.SaveRouting(background, routing); err == nil {
		t.Fatal("self-referencing final group")
	}
}

func TestSelectionAppliedAndFailedApplyKeepsLastGood(t *testing.T) {
	m, k := managerForTest(t)
	requireOK(t, m.Store.Import(background, "", parsed(t, nodeJSON("A", "8881"))))
	_, err := m.Change(background, func() error { return m.Store.AddRuleTemplates(background, []string{"youtube"}) })
	requireOK(t, err)
	state := snapshot(t, m.Store)
	_, err = m.Change(background, func() error {
		return m.Store.SaveListener(background, Listener{Name: "rules", Mode: ListenerModeRule, Port: freePort(t), Enabled: true})
	})
	requireOK(t, err)
	selected := "node-" + state.Nodes[0].ID
	res, err := m.Change(background, func() error { return m.Store.SaveSelection(background, "YouTube", selected) })
	requireOK(t, err)
	if !res.Applied {
		t.Fatal(res.Error)
	}
	if !slices.Contains(k.selected, "YouTube="+selected) {
		t.Fatal("kernel selection not restored")
	}
	good, err := os.ReadFile(filepath.Join(m.Dir, "last-good.yaml"))
	requireOK(t, err)
	k.validateErr = assertionError("invalid candidate")
	res, err = m.Change(background, func() error { return m.Store.SaveSelection(background, "YouTube", "DIRECT") })
	requireOK(t, err)
	if res.Applied {
		t.Fatal("failed apply reported success")
	}
	after, err := os.ReadFile(filepath.Join(m.Dir, "last-good.yaml"))
	requireOK(t, err)
	if string(good) != string(after) {
		t.Fatal("failed candidate overwrote last good")
	}
	var cfg struct {
		Groups []map[string]any `yaml:"proxy-groups"`
	}
	requireOK(t, yaml.Unmarshal(after, &cfg))
	for _, g := range cfg.Groups {
		if stringOf(g["name"]) == "YouTube" && stringsOf(g["proxies"])[0] != selected {
			t.Fatal("last good lost choice")
		}
	}
}

func TestGroupEndpointsRequireAuthAndValidateRequests(t *testing.T) {
	m, _ := managerForTest(t)
	s := NewServer(m, "test-key")
	defer s.Close()
	h := s.Handler(http.NotFoundHandler())
	for _, path := range []string{"/api/proxy-groups", "/api/proxy-selection", "/api/rule-templates"} {
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatalf("unprotected %s", path)
		}
	}
	s.sessions["session"] = futureTime()
	r := httptest.NewRequest(http.MethodPut, "/api/proxy-selection", strings.NewReader(`{"name":"missing","member":"DIRECT"}`))
	r.Header.Set("Content-Type", "application/json")
	r.AddCookie(&http.Cookie{Name: "manager_session", Value: "session"})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 400 {
		t.Fatalf("invalid selection status: %d %s", w.Code, w.Body.String())
	}
}

func TestGroupReservedNamesAndEmptyAutomaticGroups(t *testing.T) {
	s := storeForTest(t)
	for _, name := range []string{GroupAuto, GroupSelect, GroupFinal, "GLOBAL", ExitProbeGroup, "DIRECT"} {
		if err := s.SaveProxyGroup(background, ProxyGroup{Name: name, Kind: "select"}); err == nil {
			t.Fatalf("accepted reserved group %s", name)
		}
	}
	requireOK(t, s.SaveProxyGroup(background, ProxyGroup{Name: "Automatic", Kind: "fallback"}))
	if got := stringsOf(groupByName(t, snapshot(t, s), "Automatic")["proxies"]); len(got) != 1 || got[0] != "REJECT" {
		t.Fatalf("empty automatic group leaked to direct: %v", got)
	}
	state := snapshot(t, s)
	state.Routing.DefaultPolicy = "removed-subscription-group"
	if got := stringsOf(groupByName(t, state, GroupFinal)["proxies"]); len(got) == 0 || got[0] != "REJECT" {
		t.Fatalf("missing default bypassed proxy: %v", got)
	}
	if binary := os.Getenv("MIHOMO_TEST_BIN"); binary != "" {
		state.Listeners = []Listener{{Mode: ListenerModeRule, Port: freePort(t), Enabled: true}}
		raw, _, err := BuildConfig(state, "127.0.0.1:9090", "test-secret")
		requireOK(t, err)
		dir := t.TempDir()
		path := filepath.Join(dir, "candidate.yaml")
		requireOK(t, os.WriteFile(path, raw, 0600))
		requireOK(t, NewRuntime(binary, dir, "http://127.0.0.1:9090", "test-secret").Validate(background, path))
	}
}

func TestSubscriptionGroupSelectionSurvivesSync(t *testing.T) {
	s, source := routingStore(t)
	profile := importer.Profile{Groups: []map[string]any{{"name": GroupSelect, "type": "select", "proxies": []any{"A", "B"}}}}
	nodes := parsed(t, `[{"name":"A","type":"http","server":"127.0.0.1","port":8881},{"name":"B","type":"http","server":"127.0.0.1","port":8882}]`)
	requireOK(t, s.ImportSubscription(background, source, nodes, profile))
	state := snapshot(t, s)
	name := "[airport] " + GroupSelect
	member := "node-" + state.Nodes[1].ID
	requireOK(t, s.SaveSelection(background, name, member))
	requireOK(t, s.ImportSubscription(background, source, nodes, profile))
	if got := stringsOf(groupByName(t, snapshot(t, s), name)["proxies"])[0]; got != member {
		t.Fatalf("subscription update lost choice: %s", got)
	}
}
