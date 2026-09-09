package manager

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"mihomo-proxy/internal/importer"
)

// ip9Server stands in for the lookup service. It records the address it was
// reached from so a test can prove the request really traversed the proxy.
func ip9Server(t *testing.T, ip string) (*httptest.Server, *int) {
	t.Helper()
	hits := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"ret":200,"data":{"ip":%q,"country":"美国","prov":"加州","city":"洛杉矶","isp":"Cloudflare","ip_asn":"AS13335"},"qt":0.001}`, ip)
	}))
	t.Cleanup(server.Close)
	return server, &hits
}

func applyOK(t *testing.T, m *Manager) {
	t.Helper()
	if result := m.Apply(background); !result.Applied {
		t.Fatalf("apply failed: %s", result.Error)
	}
}

// profileNamingProbeGroup is a subscription that ships a group named after the
// probe selector, which the merge must refuse to hand over.
func profileNamingProbeGroup() importer.Profile {
	return importer.Profile{
		Groups: []map[string]any{{"name": ExitProbeGroup, "type": "select", "proxies": []any{"A"}}},
		Rules:  []string{"DOMAIN,probe.test," + ExitProbeGroup},
	}
}

func exitProbesForTest(t *testing.T) (*Manager, *fakeKernel, *ExitProbes) {
	t.Helper()
	m, kernel := managerForTest(t)
	m.ProbePort = freePort(t)
	probes := NewExitProbes(m, m.ProbePort)
	t.Cleanup(probes.Close)
	return m, kernel, probes
}

func awaitProbe(t *testing.T, probes *ExitProbes, state State, done func(ExitProbeSnapshot) bool) ExitProbeSnapshot {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		snapshot := probes.Snapshot(state)
		if done(snapshot) {
			return snapshot
		}
		if time.Now().After(deadline) {
			t.Fatalf("probe did not settle: %+v", snapshot)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestProbeListenerReportsTheAddressTheServiceSees(t *testing.T) {
	m, _, probes := exitProbesForTest(t)
	service, hits := ip9Server(t, "203.0.113.9")
	probes.service = service.URL + "/get"

	// The port must be free while the configuration is applied, so the stand-in
	// proxy starts only afterwards.
	port := freePort(t)
	requireOK(t, m.Store.SaveListener(background, Listener{Name: "rule", Port: port, Mode: ListenerModeRule, Enabled: true}))
	applyOK(t, m)
	state := snapshot(t, m.Store)
	listener := state.Listeners[0]

	// A plain HTTP proxy in front of the service stands in for the listener.
	proxy := &http.Server{Addr: net.JoinHostPort("127.0.0.1", fmt.Sprint(port)), Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		res, err := http.Get(service.URL + r.URL.Path)
		if err != nil {
			w.WriteHeader(502)
			return
		}
		defer res.Body.Close()
		w.WriteHeader(res.StatusCode)
		buf := make([]byte, 4096)
		n, _ := res.Body.Read(buf)
		w.Write(buf[:n])
	})}
	go proxy.ListenAndServe()
	defer proxy.Close()
	waitForPort(t, port)

	requireOK(t, probes.StartListener(background, listener.ID))
	got := awaitProbe(t, probes, state, func(s ExitProbeSnapshot) bool {
		return !s.Listeners[listener.ID].InFlight && s.Listeners[listener.ID].Status != ""
	})
	result := got.Listeners[listener.ID]
	if result.Status != "success" {
		t.Fatalf("probe failed: %+v", result)
	}
	if result.IP != "203.0.113.9" || result.Country != "美国" || result.ISP != "Cloudflare" || result.ASN != "AS13335" {
		t.Fatalf("lookup fields not carried through: %+v", result)
	}
	if *hits == 0 {
		t.Fatal("the service was never reached")
	}
}

func TestProbeFailsRatherThanReportingTheHostAddress(t *testing.T) {
	m, _, probes := exitProbesForTest(t)
	service, hits := ip9Server(t, "198.51.100.1")
	probes.service = service.URL + "/get"

	// Nothing is listening on the port, so the request cannot traverse a proxy.
	dead := freePort(t)
	requireOK(t, m.Store.SaveListener(background, Listener{Name: "rule", Port: dead, Mode: ListenerModeRule, Enabled: true}))
	applyOK(t, m)
	state := snapshot(t, m.Store)
	listener := state.Listeners[0]

	requireOK(t, probes.StartListener(background, listener.ID))
	got := awaitProbe(t, probes, state, func(s ExitProbeSnapshot) bool {
		return !s.Listeners[listener.ID].InFlight && s.Listeners[listener.ID].Status != ""
	})
	result := got.Listeners[listener.ID]
	// Reporting the host's own address here would make a dead outbound look
	// like a working one, which is the whole point of the probe.
	if result.Status != "failed" || result.IP != "" {
		t.Fatalf("probe fell back to a direct answer: %+v", result)
	}
	if *hits != 0 {
		t.Fatal("the lookup bypassed the proxy under test")
	}
}

func TestNodeProbeSwitchesOnlyTheProbeSelector(t *testing.T) {
	m, kernel, probes := exitProbesForTest(t)
	requireOK(t, m.Store.Import(background, "", parsed(t, `[{"name":"A","type":"http","server":"127.0.0.1","port":8881},{"name":"B","type":"http","server":"127.0.0.1","port":8882}]`)))
	applyOK(t, m)
	state := snapshot(t, m.Store)
	ids := []string{state.Nodes[0].ID, state.Nodes[1].ID}

	// No proxy is listening on the probe port, so every lookup fails. What is
	// under test is which selector the probe touches, and in what order.
	requireOK(t, probes.StartNodes(background, ids))
	awaitProbe(t, probes, state, func(s ExitProbeSnapshot) bool {
		return s.Batch != nil && s.Batch.Completed == 2
	})
	kernel.mu.Lock()
	defer kernel.mu.Unlock()
	if len(kernel.selected) != 2 {
		t.Fatalf("expected one selection per node, got %v", kernel.selected)
	}
	for i, id := range ids {
		want := ExitProbeGroup + "=node-" + id
		if kernel.selected[i] != want {
			t.Fatalf("selection %d is %q, want %q", i, kernel.selected[i], want)
		}
	}
}

func TestProbesRunOneAtATime(t *testing.T) {
	m, kernel, probes := exitProbesForTest(t)
	requireOK(t, m.Store.Import(background, "", parsed(t, nodeJSON("A", "8881"))))
	applyOK(t, m)
	state := snapshot(t, m.Store)
	id := state.Nodes[0].ID

	gate := make(chan struct{})
	kernel.mu.Lock()
	kernel.selectGate = gate
	kernel.mu.Unlock()

	requireOK(t, probes.StartNodes(background, []string{id}))
	awaitProbe(t, probes, state, func(s ExitProbeSnapshot) bool {
		return s.Nodes[id].Status == "running"
	})
	// The probe selector is shared state: a second concurrent run would
	// attribute one node's exit address to another.
	if err := probes.StartNodes(background, []string{id}); err == nil {
		t.Fatal("a second probe started while one was running")
	}
	close(gate)
	kernel.mu.Lock()
	kernel.selectGate = nil
	kernel.mu.Unlock()

	awaitProbe(t, probes, state, func(s ExitProbeSnapshot) bool {
		return s.Batch != nil && s.Batch.Status != "running"
	})
	requireOK(t, probes.StartNodes(background, []string{id}))
}

func TestProbeRefusesUnappliedConfiguration(t *testing.T) {
	m, _, probes := exitProbesForTest(t)
	requireOK(t, m.Store.Import(background, "", parsed(t, nodeJSON("A", "8881"))))
	id := snapshot(t, m.Store).Nodes[0].ID
	// The node exists in the database but not yet in the running kernel, so the
	// probe selector could not name it.
	if err := probes.StartNodes(background, []string{id}); err == nil {
		t.Fatal("probed against a configuration the kernel has not loaded")
	}
}

func TestProbeListenerAndGroupAppearOnlyWithNodes(t *testing.T) {
	s := storeForTest(t)
	state := snapshot(t, s)
	state.ProbePort = 37891

	raw, ports, err := BuildConfig(state, "127.0.0.1:9090", "secret")
	requireOK(t, err)
	if strings.Contains(string(raw), "exit-probe") || containsInt(ports, 37891) {
		t.Fatal("probe listener bound a port with no node to probe")
	}

	requireOK(t, s.Import(background, "", parsed(t, nodeJSON("A", "8881"))))
	state = snapshot(t, s)
	state.ProbePort = 37891
	raw, ports, err = BuildConfig(state, "127.0.0.1:9090", "secret")
	requireOK(t, err)
	if !containsInt(ports, 37891) {
		t.Fatalf("probe port not exposed: %v", ports)
	}
	var cfg builtConfig
	requireOK(t, yaml.Unmarshal(raw, &cfg))
	var probe map[string]any
	for _, listener := range cfg.Listeners {
		if stringOf(listener["name"]) == "exit-probe" {
			probe = listener
		}
	}
	if probe == nil {
		t.Fatal("probe listener missing")
	}
	// The probe port must never be reachable from the network: it can be
	// pointed at any node, so exposing it would expose every node.
	if stringOf(probe["listen"]) != "127.0.0.1" {
		t.Fatalf("probe listener bound %q", probe["listen"])
	}
	if stringOf(probe["proxy"]) != ExitProbeGroup {
		t.Fatalf("probe listener bound %q instead of its own selector", probe["proxy"])
	}
	if cfg.group(ExitProbeGroup) == nil {
		t.Fatal("probe selector missing")
	}
}

func TestProbePortIsExcludedFromRoutingGroups(t *testing.T) {
	s, source := routingStore(t)
	ruleListener(t, s, 17892)
	requireOK(t, s.ImportSubscription(background, source, parsed(t, nodeJSON("A", "8881")), profileNamingProbeGroup()))
	state := snapshot(t, s)
	state.ProbePort = 37891
	raw, _, err := BuildConfig(state, "127.0.0.1:9090", "secret")
	requireOK(t, err)
	var cfg builtConfig
	requireOK(t, yaml.Unmarshal(raw, &cfg))

	// A subscription group must not be able to take over the selector the
	// probe drives, or a probe would redirect live traffic.
	seen := 0
	for _, name := range cfg.groupNames() {
		if name == ExitProbeGroup {
			seen++
		}
	}
	if seen != 1 {
		t.Fatalf("probe selector appears %d times: %v", seen, cfg.groupNames())
	}
	if members := cfg.members(t, ExitProbeGroup); !containsAny(members, "DIRECT") {
		t.Fatalf("probe selector was replaced by the subscription group: %v", members)
	}
}

func containsInt(list []int, value int) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

func waitForPort(t *testing.T, port int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", fmt.Sprint(port)), 100*time.Millisecond)
		if err == nil {
			conn.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("port %d never opened", port)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
