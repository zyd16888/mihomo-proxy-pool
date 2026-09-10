package manager

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

const sourceFixture = `mixed-port: 12345
external-controller: 0.0.0.0:9999
secret: remote-secret
tun: {enable: true}
proxy-groups:
  - {name: Services, type: select, include-all: true}
  - {name: Video, type: select, proxies: [Services, DIRECT]}
rules:
  - DOMAIN-SUFFIX,example.com,Services
  - DOMAIN,video.example,Video
  - MATCH,DIRECT
`

type mutableSource struct {
	mu       sync.Mutex
	body     string
	status   int
	requests int
}

func newMutableSource(t *testing.T, body string) (*mutableSource, *httptest.Server) {
	t.Helper()
	source := &mutableSource{body: body, status: 200}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		source.mu.Lock()
		defer source.mu.Unlock()
		source.requests++
		w.WriteHeader(source.status)
		fmt.Fprint(w, source.body)
	}))
	t.Cleanup(server.Close)
	return source, server
}
func (s *mutableSource) set(body string, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.body = body
	s.status = status
}
func sourceManager(t *testing.T) (*Manager, *fakeKernel, *RoutingSourceService) {
	m, k := managerForTest(t)
	requireOK(t, m.Store.Import(background, "", parsed(t, nodeJSON("A", "18881"))))
	requireOK(t, m.Store.Import(background, "", parsed(t, nodeJSON("B", "18882"))))
	requireOK(t, m.Store.SaveListener(background, Listener{Name: "routing", Mode: ListenerModeRule, Port: freePort(t), Enabled: true}))
	if r := m.Apply(background); !r.Applied {
		t.Fatal(r.Error)
	}
	service := NewRoutingSourceService(m)
	t.Cleanup(service.Close)
	return m, k, service
}
func addSource(t *testing.T, service *RoutingSourceService, address string) string {
	t.Helper()
	report := service.Preview(background, RoutingSource{Name: "External", URL: address, Interval: 60})
	if len(report.Errors) > 0 {
		t.Fatal(report.Errors)
	}
	if report.Token == "" {
		t.Fatal("preview missing token")
	}
	_, err := service.SavePreview(background, report.Token)
	requireOK(t, err)
	return report.Source.ID
}

func TestRoutingSourceScopeRefreshAndLocalEdits(t *testing.T) {
	m, k, service := sourceManager(t)
	upstream, server := newMutableSource(t, sourceFixture)
	local := snapshot(t, m.Store)
	nodeA := "node-" + local.Nodes[0].ID
	nodeB := "node-" + local.Nodes[1].ID
	_, err := m.Change(background, func() error { return m.Store.SaveSelection(background, GroupSelect, nodeB) })
	requireOK(t, err)
	id := addSource(t, service, server.URL)
	requireOK(t, service.Activate(background, id))
	res, err := m.Change(background, func() error { return m.Store.SaveSelection(background, "Services", nodeA) })
	requireOK(t, err)
	if !res.Applied {
		t.Fatal(res.Error)
	}
	res, err = m.Change(background, func() error {
		return m.Store.SaveCategoryRule(background, id, CategoryRule{Policy: "Services", Kind: "IP", Value: "203.0.113.7", Enabled: true, Position: 50})
	})
	requireOK(t, err)
	if !res.Applied {
		t.Fatal(res.Error)
	}
	res, err = m.Change(background, func() error {
		return m.Store.SaveCategoryRule(background, id, CategoryRule{Policy: "Services", Kind: "DOMAIN-SUFFIX", Value: "vip.example.com", Enabled: true, Position: 10})
	})
	requireOK(t, err)
	if !res.Applied {
		t.Fatal(res.Error)
	}
	plan, err := compileRouting(snapshot(t, m.Store))
	requireOK(t, err)
	if plan.Rules[0] != "DOMAIN-SUFFIX,vip.example.com,Services" || plan.Rules[1] != "IP-CIDR,203.0.113.7/32,Services,no-resolve" {
		t.Fatalf("manual priority or IP normalization: %v", plan.Rules)
	}
	before := snapshot(t, m.Store)
	k.mu.Lock()
	reloads := k.reloads
	k.mu.Unlock()
	changed, err := service.Refresh(background, id)
	requireOK(t, err)
	if changed {
		t.Fatal("same content marked changed")
	}
	k.mu.Lock()
	afterReloads := k.reloads
	k.mu.Unlock()
	if afterReloads != reloads || snapshot(t, m.Store).Revision != before.Revision {
		t.Fatal("unchanged refresh reloaded kernel")
	}
	res, err = m.Change(background, func() error {
		return m.Store.SaveCategoryEdit(background, id, CategoryEdit{Name: "Services", Label: "My services", Deleted: true})
	})
	requireOK(t, err)
	if !res.Applied {
		t.Fatal(res.Error)
	}
	upstream.set(strings.Replace(sourceFixture, "- MATCH,DIRECT", "- DOMAIN,new.example,Services\n  - MATCH,DIRECT", 1), 200)
	changed, err = service.Refresh(background, id)
	requireOK(t, err)
	if !changed {
		t.Fatal("new rules not detected")
	}
	state := snapshot(t, m.Store)
	plan, err = compileRouting(state)
	requireOK(t, err)
	if slices.Contains(groupNames(plan.Groups), "Services") {
		t.Fatal("deleted source category resurrected")
	}
	for _, rule := range plan.Rules {
		if rulePolicy(rule) == "Services" {
			t.Fatal("deleted category retained routes")
		}
	}
	for _, g := range plan.Groups {
		if slices.Contains(stringsOf(g["proxies"]), "Services") {
			t.Fatal("deleted category still referenced")
		}
	}
	res, err = m.Change(background, func() error { return m.Store.RestoreCategory(background, id, "Services") })
	requireOK(t, err)
	if !res.Applied {
		t.Fatal(res.Error)
	}
	if stringsOf(groupByName(t, snapshot(t, m.Store), "Services")["proxies"])[0] != nodeA {
		t.Fatal("category restore lost saved selection")
	}
	requireOK(t, service.Activate(background, ""))
	state = snapshot(t, m.Store)
	if state.Selections[GroupSelect] != nodeB || len(state.CategoryRules) != 0 {
		t.Fatal("external state contaminated local scheme")
	}
	requireOK(t, service.Activate(background, id))
	state = snapshot(t, m.Store)
	if state.Selections["Services"] != nodeA || len(state.CategoryRules) != 2 {
		t.Fatal("switching source lost overrides")
	}
	config, _, err := BuildConfig(state, m.CoreAddr, m.Secret)
	requireOK(t, err)
	if strings.Contains(string(config), "12345") || strings.Contains(string(config), "remote-secret") || strings.Contains(string(config), "tun:") {
		t.Fatal("remote system settings imported")
	}
}

func TestRoutingSourceFailureKeepsSuccessfulSnapshot(t *testing.T) {
	m, k, service := sourceManager(t)
	upstream, server := newMutableSource(t, sourceFixture)
	id := addSource(t, service, server.URL)
	requireOK(t, service.Activate(background, id))
	good := snapshot(t, m.Store)
	raw, err := os.ReadFile(filepath.Join(m.Dir, "last-good.yaml"))
	requireOK(t, err)
	for _, failure := range []string{"http", "parse", "validate", "reload"} {
		t.Run(failure, func(t *testing.T) {
			upstream.set(sourceFixture, 200)
			k.validateErr = nil
			switch failure {
			case "http":
				upstream.set("unavailable", 503)
			case "parse":
				upstream.set("<html>error</html>", 200)
			case "validate":
				upstream.set(strings.Replace(sourceFixture, "example.com", "changed.example", 1), 200)
				k.validateErr = assertionError("kernel rejects")
			case "reload":
				upstream.set(strings.Replace(sourceFixture, "example.com", "changed.example", 1), 200)
				k.failReload = true
			}
			if _, err := service.Refresh(background, id); err == nil {
				t.Fatal("failed source accepted")
			}
			state := snapshot(t, m.Store)
			if state.RoutingSources[0].Digest != good.RoutingSources[0].Digest || state.Revision != good.Revision {
				t.Fatal("failure overwrote successful source")
			}
			if state.RoutingSources[0].LastError == "" {
				t.Fatal("failure not recorded")
			}
			after, err := os.ReadFile(filepath.Join(m.Dir, "last-good.yaml"))
			requireOK(t, err)
			if string(after) != string(raw) {
				t.Fatal("failure changed last good")
			}
		})
	}
}

func TestRoutingSourceBindingsAndStrictParsing(t *testing.T) {
	state := State{Nodes: []Node{{ID: "a", Name: "Shared", Config: json.RawMessage(`{"type":"http","server":"a","port":80}`)}, {ID: "b", Name: "Shared", Config: json.RawMessage(`{"type":"http","server":"b","port":80}`)}}}
	raw := []byte("proxy-groups:\n - {name: Work, type: select, proxies: [Shared]}\nrules:\n - MATCH,Work\n")
	source := RoutingSource{ID: "source"}
	_, report, err := parseRoutingDocument(raw, source, state)
	if err == nil || !slices.Contains(report.Unresolved, "Shared") {
		t.Fatal("ambiguous node name guessed")
	}
	source.Bindings = map[string]string{"Shared": "b"}
	doc, _, err := parseRoutingDocument(raw, source, state)
	requireOK(t, err)
	if stringsOf(doc.Groups[0]["proxies"])[0] != "node-b" {
		t.Fatal("manual binding ignored")
	}
	for _, body := range []string{
		"proxy-groups: [{name: Work, type: select, proxies: [DIRECT]}]\nrules: [UNKNOWN,a,Work]",
		"proxy-groups: [{name: Work, type: select, proxies: [DIRECT]}]\nrules: ['RULE-SET,missing,Work']",
		"proxy-groups: [{name: Work, type: select, proxies: [DIRECT]}]\nrules: ['MATCH,DIRECT','DOMAIN,a,Work']",
		"proxy-groups: [{name: Work, type: select, proxies: [DIRECT]}]\nrules: ['GEOIP,CN,Work']",
		"rules: ['MATCH,DIRECT']\n---\nrules: ['MATCH,REJECT']",
	} {
		if _, _, err := parseRoutingDocument([]byte(body), source, state); err == nil {
			t.Fatalf("accepted invalid routing: %s", body)
		}
	}
}

func TestCategoryRuleReplacementAndDeletionPersistence(t *testing.T) {
	m, _, service := sourceManager(t)
	_, server := newMutableSource(t, sourceFixture)
	id := addSource(t, service, server.URL)
	requireOK(t, service.Activate(background, id))
	original := "DOMAIN-SUFFIX,example.com,Services"
	res, err := m.Change(background, func() error {
		return m.Store.SaveCategoryRule(background, id, CategoryRule{Policy: "Video", Kind: "DOMAIN-SUFFIX", Value: "example.com", Enabled: true, Position: 1, ReplacesText: original})
	})
	requireOK(t, err)
	if !res.Applied {
		t.Fatal(res.Error)
	}
	state := snapshot(t, m.Store)
	plan, err := compileRouting(state)
	requireOK(t, err)
	if slices.Contains(plan.Rules, original) || plan.Rules[0] != "DOMAIN-SUFFIX,example.com,Video" {
		t.Fatal("inherited rule edit was not an atomic override")
	}
	res, err = m.Change(background, func() error {
		return m.Store.SaveCategoryEdit(background, id, CategoryEdit{Name: "Services", Label: "Renamed"})
	})
	requireOK(t, err)
	if !res.Applied {
		t.Fatal(res.Error)
	}
	if !slices.Contains(stringsOf(groupByName(t, snapshot(t, m.Store), "Services")["proxies"]), "node-"+state.Nodes[0].ID) {
		t.Fatal("display rename replaced inherited members")
	}
	if err := m.Store.SaveCategoryRule(background, "", CategoryRule{Policy: "Video", Kind: "DOMAIN", Value: "wrong.example", Enabled: true}); err == nil {
		t.Fatal("stale dialog modified different source")
	}
	if err := m.Store.SaveCategoryEdit(background, id, CategoryEdit{Name: "Services", Kind: "select", Members: []string{"Video"}}); err == nil {
		t.Fatal("cycle was not rejected")
	}
	if err := m.Store.SaveCategoryRule(background, id, CategoryRule{Policy: "Video", Kind: "IP", Value: "bad IP"}); err == nil {
		t.Fatal("invalid IP accepted")
	}
	var sequence int
	var databaseName, databasePath string
	requireOK(t, m.Store.db.QueryRow(`PRAGMA database_list`).Scan(&sequence, &databaseName, &databasePath))
	reopened, err := OpenStore(databasePath)
	requireOK(t, err)
	defer reopened.Close()
	state = snapshot(t, reopened)
	if state.BlockedRules[routingEntryID(original)] != original || len(state.CategoryRules) != 1 {
		t.Fatal("rule override not stored")
	}
}

func TestRoutingSourceSchedulerChecksDueSource(t *testing.T) {
	m, _, service := sourceManager(t)
	upstream, server := newMutableSource(t, sourceFixture)
	id := addSource(t, service, server.URL)
	state := snapshot(t, m.Store)
	source := state.RoutingSources[0]
	source.AutoUpdate = true
	source.CheckedAt = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	requireOK(t, m.Store.saveRoutingSource(background, source, false))
	upstream.mu.Lock()
	before := upstream.requests
	upstream.mu.Unlock()
	service.Start()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		state = snapshot(t, m.Store)
		if state.RoutingSources[0].ID == id && state.RoutingSources[0].CheckedAt != source.CheckedAt {
			upstream.mu.Lock()
			after := upstream.requests
			upstream.mu.Unlock()
			if after <= before {
				t.Fatal("scheduler did not fetch")
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("scheduled source was not checked")
}

func TestDeletedLocalCategoryRestoresDefinitionAndRules(t *testing.T) {
	m, _, _ := sourceManager(t)
	requireOK(t, m.Store.SaveCategoryEdit(background, "", CategoryEdit{Label: "My group", Kind: "select", AllNodes: true}))
	state := snapshot(t, m.Store)
	edit := state.CategoryEdits[0]
	requireOK(t, m.Store.SaveCategoryRule(background, "", CategoryRule{Policy: edit.Name, Kind: "DOMAIN", Value: "private.example", Enabled: true}))
	requireOK(t, m.Store.SaveCategoryEdit(background, "", CategoryEdit{Name: edit.Name, Deleted: true}))
	state = snapshot(t, m.Store)
	if slices.Contains(groupNames(routingPreview(state)), edit.Name) {
		t.Fatal("deleted category still emitted")
	}
	requireOK(t, m.Store.RestoreCategory(background, "", edit.Name))
	state = snapshot(t, m.Store)
	if !slices.Contains(groupNames(routingPreview(state)), edit.Name) || state.CategoryEdits[0].Label != "My group" || len(state.CategoryRules) != 1 {
		t.Fatal("restore lost the new category definition or its rules")
	}
}

func TestSourceDNSAndDeletedProviders(t *testing.T) {
	state := State{Routing: DefaultRouting(), Listeners: []Listener{{Mode: ListenerModeRule, Enabled: true}}}
	raw := sourceFixture + `dns:
 enable: true
 listen: 0.0.0.0:53
 enhanced-mode: fake-ip
 respect-rules: true
 nameserver: [https://dns.alidns.com/dns-query]
rule-providers:
 extra: {type: inline, behavior: domain, payload: [example.org]}
`
	raw = strings.Replace(raw, "- DOMAIN-SUFFIX,example.com,Services", "- RULE-SET,extra,Services", 1)
	source := RoutingSource{ID: "source", ImportDNS: true}
	doc, report, err := parseRoutingDocument([]byte(raw), source, state)
	requireOK(t, err)
	if doc.DNS["listen"] != nil || doc.DNS["enhanced-mode"] != "redir-host" || doc.DNS["respect-rules"] != false {
		t.Fatal("remote DNS listener or hijack settings imported")
	}
	if !slices.Contains(report.Ignored, "dns.listen") {
		t.Fatal("ignored DNS fields not reported")
	}
	source.Document = doc
	source.Digest = "validated"
	state.RoutingSources = []RoutingSource{source}
	state.ActiveRoutingSource = source.ID
	state.CategoryEdits = []CategoryEdit{{Name: "Services", Deleted: true}}
	plan, err := compileRouting(state)
	requireOK(t, err)
	if plan.Providers["extra"] != nil {
		t.Fatal("whole-category removal retained orphan provider")
	}
	state.Routing.DNSEnabled = false
	plan, err = compileRouting(state)
	requireOK(t, err)
	if plan.DNS != nil {
		t.Fatal("source DNS overrode disabled DNS setting")
	}
}
