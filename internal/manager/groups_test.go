package manager

import (
	"slices"
	"testing"
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

func TestCategoryConfigAtomicAndPersistent(t *testing.T) {
	m, _, service := sourceManager(t)
	_, server := newMutableSource(t, sourceFixture)
	id := addSource(t, service, server.URL)
	requireOK(t, service.Activate(background, id))
	node := "node-" + snapshot(t, m.Store).Nodes[0].ID
	edit := CategoryEdit{Label: "Work", Kind: "select", AllNodes: true}
	rules := []CategoryRule{{Kind: "DOMAIN-SUFFIX", Value: "Example.COM", Position: 100}, {Kind: "IP", Value: "203.0.113.7", Position: 101}}
	before := snapshot(t, m.Store)
	invalid := append(append([]CategoryRule{}, rules...), CategoryRule{Kind: "IP", Value: "invalid"})
	if err := m.Store.SaveCategoryConfig(background, id, edit, invalid, node); err == nil {
		t.Fatal("invalid batch accepted")
	}
	if err := m.Store.SaveCategoryConfig(background, id, edit, rules, "unknown"); err == nil {
		t.Fatal("invalid selection accepted")
	}
	after := snapshot(t, m.Store)
	if len(after.CategoryEdits) != len(before.CategoryEdits) || len(after.CategoryRules) != len(before.CategoryRules) || after.Revision != before.Revision {
		t.Fatal("partial category persisted")
	}
	requireOK(t, m.Store.SaveCategoryConfig(background, id, edit, rules, node))
	state := snapshot(t, m.Store)
	name := state.CategoryEdits[0].Name
	if err := m.Store.SaveCategoryEdit(background, id, CategoryEdit{Name: name, Label: "Rename"}); err == nil {
		t.Fatal("local category inherited a nonexistent source definition")
	}
	if err := m.Store.RestoreCategory(background, id, name); err == nil {
		t.Fatal("local category reset removed its definition")
	}
	cfg := buildFor(t, m.Store)
	if cfg.Rules[0] != "DOMAIN-SUFFIX,example.com,"+name || cfg.Rules[1] != "IP-CIDR,203.0.113.7/32,"+name+",no-resolve" {
		t.Fatalf("bad rules: %v", cfg.Rules)
	}
	if cfg.members(t, name)[0] != node {
		t.Fatal("default exit not applied")
	}
	requireOK(t, m.Store.SaveSelection(background, name, "node-"+state.Nodes[1].ID))
	if err := m.Store.SaveCategoryConfig(background, id, CategoryEdit{Name: name, Kind: "select", AllNodes: true}, rules, node); err == nil {
		t.Fatal("existing category accepted duplicate batch")
	}
	requireOK(t, service.Activate(background, ""))
	if len(buildFor(t, m.Store).Listeners) != 0 {
		t.Fatal("deactivation did not park port")
	}
	requireOK(t, service.Activate(background, id))
	var seq int
	var schema, databasePath string
	requireOK(t, m.Store.db.QueryRow(`PRAGMA database_list`).Scan(&seq, &schema, &databasePath))
	reopened, err := OpenStore(databasePath)
	requireOK(t, err)
	defer reopened.Close()
	restored := snapshot(t, reopened)
	if len(restored.CategoryRules) != 2 || restored.Selections[name] != "node-"+state.Nodes[1].ID {
		t.Fatal("reopen lost category state")
	}
	requireOK(t, m.Store.SetNodeEnabled(background, state.Nodes[1].ID, false))
	if slices.Contains(buildFor(t, m.Store).members(t, name), "node-"+state.Nodes[1].ID) {
		t.Fatal("disabled node remains selectable")
	}
}
