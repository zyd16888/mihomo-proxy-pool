package manager

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHTTPAuthenticationAndCSRF(t *testing.T) {
	m, _ := managerForTest(t)
	handler := NewServer(m, "a-strong-test-key").Handler(http.NotFoundHandler())
	request := func(method, path, body string, cookie *http.Cookie, origin string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		r.Header.Set("Content-Type", "application/json")
		if cookie != nil {
			r.AddCookie(cookie)
		}
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	if w := request("GET", "/api/state", "", nil, ""); w.Code != 401 {
		t.Fatalf("unprotected admin data: %d", w.Code)
	}
	if w := request("POST", "/api/login", `{"key":"wrong"}`, nil, ""); w.Code != 401 {
		t.Fatal("wrong key accepted")
	}
	w := request("POST", "/api/login", `{"key":"a-strong-test-key"}`, nil, "")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	cookie := w.Result().Cookies()[0]
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatal("session cookie missing restrictions")
	}
	if w = request("GET", "/api/state", "", cookie, ""); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if w = request("POST", "/api/apply", `{}`, cookie, "https://another.example"); w.Code != 403 {
		t.Fatal("cross-site mutation accepted")
	}
	if w = request("POST", "/api/logout", `{}`, cookie, ""); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if w = request("GET", "/api/state", "", cookie, ""); w.Code != 401 {
		t.Fatal("logged-out cookie reused")
	}
}

func TestHTTPReportsSavedButUnapplied(t *testing.T) {
	m, k := managerForTest(t)
	k.validateErr = assertionError("validation failed")
	server := NewServer(m, "test-key")
	server.sessions["test-session"] = futureTime()
	handler := server.Handler(http.NotFoundHandler())
	r := httptest.NewRequest("POST", "/api/import", bytes.NewBufferString(`{"nodes":[{"name":"A","type":"http","server":"127.0.0.1","port":8881}]}`))
	r.Header.Set("Content-Type", "application/json")
	r.AddCookie(&http.Cookie{Name: "manager_session", Value: "test-session"})
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var result struct {
		Saved bool        `json:"saved"`
		Apply ApplyResult `json:"apply"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Saved || result.Apply.Applied || result.Apply.Error == "" {
		t.Fatal("saved confused with applied")
	}
}

type assertionError string

func (e assertionError) Error() string { return string(e) }

func TestSubscriptionSyncPreservesStateOnPartialInvalidResponse(t *testing.T) {
	m, _ := managerForTest(t)
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `[{"name":"good","type":"http","server":"127.0.0.1","port":8080},{"name":"broken"}]`)
	}))
	defer source.Close()
	requireOK(t, m.Store.SaveSubscription(background, Subscription{Name: "source", URL: source.URL}))
	id := snapshot(t, m.Store).Subscriptions[0].ID
	requireOK(t, m.Store.Import(background, id, parsed(t, nodeJSON("existing", "8881"))))
	before := snapshot(t, m.Store)
	srv := NewServer(m, "test-key")
	srv.sessions["session"] = futureTime()
	r := httptest.NewRequest("POST", "/api/subscriptions/"+id+"/sync", bytes.NewBufferString(`{}`))
	r.Header.Set("Content-Type", "application/json")
	r.AddCookie(&http.Cookie{Name: "manager_session", Value: "session"})
	w := httptest.NewRecorder()
	srv.Handler(http.NotFoundHandler()).ServeHTTP(w, r)
	if w.Code != 400 {
		t.Fatal(w.Body.String())
	}
	after := snapshot(t, m.Store)
	if after.Revision != before.Revision || len(after.Nodes) != 1 || !after.Nodes[0].Available {
		t.Fatal("bad download changed prior nodes")
	}
}
