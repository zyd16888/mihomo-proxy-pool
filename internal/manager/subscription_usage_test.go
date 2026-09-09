package manager

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestSubscriptionUsageCalculations(t *testing.T) {
	u, err := ParseSubscriptionUsage("upload=100; download=300; total=1000; expire=2000000000")
	requireOK(t, err)
	if *u.UsedBytes != 400 || *u.RemainingBytes != 600 || *u.OverageBytes != 0 {
		t.Fatal("incorrect byte arithmetic")
	}
	u, err = ParseSubscriptionUsage("upload=500; download=700; total=1000")
	requireOK(t, err)
	if *u.RemainingBytes != 0 || *u.OverageBytes != 200 {
		t.Fatal("overage incorrectly displayed as negative remaining")
	}
	u, err = ParseSubscriptionUsage("upload=0; download=0; total=0; expire=0")
	requireOK(t, err)
	if u.UsedBytes == nil || *u.UsedBytes != 0 || u.RemainingBytes != nil {
		t.Fatal("zero usage confused with unknown quota")
	}
	u, err = ParseSubscriptionUsage("upload=100; total=1000; unknown=value")
	requireOK(t, err)
	if u.UsedBytes != nil || u.RemainingBytes != nil || u.DownloadBytes != nil {
		t.Fatal("missing counters treated as zero")
	}
}

func TestSubscriptionUsageRejectsInvalidHeader(t *testing.T) {
	for _, raw := range []string{"", "foo=bar", "upload=-1", "upload=1.5", "upload=1;upload=2", "total=99999999999999999999999", "upload=9223372036854775807;download=1", "expire=999999999999"} {
		if _, err := ParseSubscriptionUsage(raw); err == nil {
			t.Fatalf("accepted invalid header: %s", raw)
		}
	}
}

func TestUsageSnapshotPersistsAndDoesNotChangeConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manager.db")
	s, err := OpenStore(path)
	requireOK(t, err)
	requireOK(t, s.SaveSubscription(background, Subscription{Name: "source", URL: "https://example.invalid/sub"}))
	sub := snapshot(t, s).Subscriptions[0]
	revision := snapshot(t, s).Revision
	u, err := ParseSubscriptionUsage("upload=100; download=300; total=1000")
	requireOK(t, err)
	requireOK(t, s.RecordSubscriptionUsage(background, sub.ID, sub.URL, u, "current"))
	requireOK(t, s.Close())
	s, err = OpenStore(path)
	requireOK(t, err)
	defer s.Close()
	state := snapshot(t, s)
	if state.Revision != revision || *state.Subscriptions[0].Usage.UsedBytes != 400 {
		t.Fatal("usage lost or changed revision")
	}
	for _, status := range []string{"missing", "invalid", "fetch_failed"} {
		requireOK(t, s.RecordSubscriptionUsage(background, sub.ID, sub.URL, SubscriptionUsage{}, status))
		usage := snapshot(t, s).Subscriptions[0].Usage
		if *usage.UsedBytes != 400 || usage.Status != status || usage.UpdatedAt == "" {
			t.Fatal("refresh failure destroyed previous snapshot")
		}
	}
	partial, err := ParseSubscriptionUsage("total=2000")
	requireOK(t, err)
	requireOK(t, s.RecordSubscriptionUsage(background, sub.ID, sub.URL, partial, "current"))
	usage := snapshot(t, s).Subscriptions[0].Usage
	if usage.UsedBytes != nil || usage.UploadBytes != nil || *usage.TotalBytes != 2000 {
		t.Fatal("mixed counters from different snapshots")
	}
}

func TestUsageURLChangeInvalidatesOldSnapshot(t *testing.T) {
	s := storeForTest(t)
	requireOK(t, s.SaveSubscription(background, Subscription{Name: "old", URL: "https://old.invalid"}))
	sub := snapshot(t, s).Subscriptions[0]
	u, _ := ParseSubscriptionUsage("upload=10;download=20;total=100")
	requireOK(t, s.RecordSubscriptionUsage(background, sub.ID, sub.URL, u, "current"))
	sub.Name = "renamed"
	requireOK(t, s.SaveSubscription(background, sub))
	if snapshot(t, s).Subscriptions[0].Usage == nil {
		t.Fatal("rename discarded usage")
	}
	oldURL := sub.URL
	sub.URL = "https://new.invalid"
	requireOK(t, s.SaveSubscription(background, sub))
	if snapshot(t, s).Subscriptions[0].Usage != nil {
		t.Fatal("new address retained old account usage")
	}
	if err := s.RecordSubscriptionUsage(background, sub.ID, oldURL, u, "current"); err == nil {
		t.Fatal("stale URL response replaced new account")
	}
	requireOK(t, s.RecordSubscriptionUsage(background, sub.ID, sub.URL, u, "current"))
	requireOK(t, s.DeleteSubscription(background, sub.ID))
	var count int
	requireOK(t, s.db.QueryRow(`SELECT count(*) FROM subscription_usage`).Scan(&count))
	if count != 0 {
		t.Fatal("deleted subscription retained usage")
	}
}

func TestExistingDatabaseGainsUsageTableWithoutLosingBindings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manager.db")
	s, err := OpenStore(path)
	requireOK(t, err)
	requireOK(t, s.Import(background, "", parsed(t, nodeJSON("existing", "8080"))))
	id := snapshot(t, s).Nodes[0].ID
	requireOK(t, s.SaveListener(background, Listener{Name: "fixed", Port: 17891, NodeID: id, Enabled: true}))
	_, err = s.db.Exec(`DROP TABLE subscription_usage`)
	requireOK(t, err)
	requireOK(t, s.Close())
	s, err = OpenStore(path)
	requireOK(t, err)
	defer s.Close()
	state := snapshot(t, s)
	if len(state.Nodes) != 1 || len(state.Listeners) != 1 || state.Listeners[0].NodeID != id {
		t.Fatal("schema upgrade lost data")
	}
}

func TestUsageRefreshUsesHeadersWithoutImportOrReload(t *testing.T) {
	responseHeader := "upload=50;download=150;total=1000"
	code := 200
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Subscription-Userinfo", responseHeader)
		w.WriteHeader(code)
		w.Write([]byte("not a node list"))
	}))
	defer source.Close()
	m, k := managerForTest(t)
	requireOK(t, m.Store.SaveSubscription(background, Subscription{Name: "source", URL: source.URL}))
	sub := snapshot(t, m.Store).Subscriptions[0]
	before := snapshot(t, m.Store)
	srv := NewServer(m, "test")
	defer srv.Close()
	srv.sessions["session"] = futureTime()
	handler := srv.Handler(http.NotFoundHandler())
	request := func(path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", path, bytes.NewBufferString(`{}`))
		r.AddCookie(&http.Cookie{Name: "manager_session", Value: "session"})
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	w := request("/api/subscriptions/" + sub.ID + "/usage")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if k.reloads != 0 || snapshot(t, m.Store).Revision != before.Revision || *snapshot(t, m.Store).Subscriptions[0].Usage.UsedBytes != 200 {
		t.Fatal("quota refresh imported or reloaded config")
	}
	responseHeader = "upload=999;total=oops"
	w = request("/api/subscriptions/" + sub.ID + "/usage")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	u := snapshot(t, m.Store).Subscriptions[0].Usage
	if u.Status != "invalid" || *u.UsedBytes != 200 {
		t.Fatal("invalid header overwrote valid snapshot")
	}
	responseHeader = ""
	request("/api/subscriptions/" + sub.ID + "/usage")
	if snapshot(t, m.Store).Subscriptions[0].Usage.Status != "missing" {
		t.Fatal("missing header not reported")
	}
	code = 503
	w = request("/api/subscriptions/" + sub.ID + "/usage")
	if w.Code != 502 || snapshot(t, m.Store).Subscriptions[0].Usage.Status != "fetch_failed" {
		t.Fatal("download failure not reported")
	}
	code = 200
	responseHeader = "upload=100;download=200;total=1000"
	w = request("/api/subscriptions/" + sub.ID + "/sync")
	if w.Code != 400 {
		t.Fatal("invalid node payload accepted")
	}
	if *snapshot(t, m.Store).Subscriptions[0].Usage.UsedBytes != 300 || snapshot(t, m.Store).Revision != before.Revision {
		t.Fatal("valid metadata depended on successful node import")
	}
}

func TestReimportVLESSFixPreservesNodeAndListener(t *testing.T) {
	s := storeForTest(t)
	old := `[{"name":"Reality","type":"vless","server":"example.invalid","port":443,"uuid":"00000000-0000-4000-8000-000000000001"}]`
	requireOK(t, s.Import(context.Background(), "", parsed(t, old)))
	id := snapshot(t, s).Nodes[0].ID
	requireOK(t, s.SaveListener(background, Listener{Name: "fixed", Port: 17891, NodeID: id, Enabled: true}))
	key := base64.RawURLEncoding.EncodeToString([]byte("01234567890123456789012345678901"))
	link := "vless://00000000-0000-4000-8000-000000000001@example.invalid:443?security=reality&pbk=" + key + "&sni=www.example.com&flow=xtls-rprx-vision#Reality"
	requireOK(t, s.Import(background, "", parsed(t, link)))
	state := snapshot(t, s)
	if len(state.Nodes) != 1 || state.Nodes[0].ID != id || state.Listeners[0].NodeID != id {
		t.Fatal("reimport lost binding")
	}
	var cfg map[string]any
	requireOK(t, json.Unmarshal(state.Nodes[0].Config, &cfg))
	if cfg["tls"] != true || cfg["reality-opts"] == nil || cfg["flow"] != "xtls-rprx-vision" {
		t.Fatal("stored config still missing Reality options")
	}
}
