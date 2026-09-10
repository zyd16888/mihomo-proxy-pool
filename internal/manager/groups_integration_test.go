package manager

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestRealKernelGroupRoutingAndRestart(t *testing.T) {
	binary := os.Getenv("MIHOMO_TEST_BIN")
	if binary == "" {
		t.Skip("set MIHOMO_TEST_BIN for real group routing")
	}
	binary, err := filepath.Abs(binary)
	requireOK(t, err)
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, "manager.db"))
	requireOK(t, err)
	defer func() { store.Close() }()
	addr := "127.0.0.1:" + strconv.Itoa(freePort(t))
	kernel := NewRuntime(binary, dir, "http://"+addr, "test-secret")
	m := &Manager{Store: store, Kernel: kernel, Dir: dir, CoreAddr: addr, Secret: "test-secret"}
	requireOK(t, m.Bootstrap(background))
	var cmd *exec.Cmd
	start := func() {
		t.Helper()
		logfile, err := os.CreateTemp(dir, "core-*.log")
		requireOK(t, err)
		cmd = exec.Command(binary, "-d", dir, "-f", filepath.Join(dir, "config.yaml"))
		cmd.Stdout = logfile
		cmd.Stderr = logfile
		requireOK(t, cmd.Start())
		logfile.Close()
		for i := 0; i < 100; i++ {
			ctx, cancel := context.WithTimeout(background, 100*time.Millisecond)
			_, err := kernel.Version(ctx)
			cancel()
			if err == nil {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatal("kernel startup timeout")
	}
	stop := func() {
		if cmd != nil {
			cmd.Process.Kill()
			cmd.Wait()
			cmd = nil
		}
	}
	defer stop()
	start()
	fixture := func(marker string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodConnect {
				conn, buffer, err := w.(http.Hijacker).Hijack()
				if err != nil {
					return
				}
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(3 * time.Second))
				fmt.Fprint(conn, "HTTP/1.1 200 Connection established\r\n\r\n")
				request, err := http.ReadRequest(bufio.NewReader(buffer))
				if err != nil {
					return
				}
				request.Body.Close()
				fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(marker), marker)
				return
			}
			fmt.Fprint(w, marker)
		}))
	}
	a, b := fixture("via-A"), fixture("via-B")
	defer a.Close()
	defer b.Close()
	for i, p := range []*httptest.Server{a, b} {
		_, port, _ := net.SplitHostPort(strings.TrimPrefix(p.URL, "http://"))
		requireOK(t, store.Import(background, "", parsed(t, nodeJSON([]string{"A", "B"}[i], port))))
	}
	for _, set := range snapshot(t, store).RuleSets {
		requireOK(t, store.DeleteRuleSet(background, set.ID))
	}
	routing := snapshot(t, store).Routing
	routing.DNSEnabled = false
	requireOK(t, store.SaveRouting(background, routing))
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "payload:\n  - '%s.demo.test'\n", strings.TrimPrefix(r.URL.Path, "/"))
	}))
	defer provider.Close()
	for _, name := range []string{"ai", "video"} {
		requireOK(t, store.SaveProxyGroup(background, ProxyGroup{Name: name, Kind: "select"}))
		requireOK(t, store.SaveRuleSet(background, RuleSet{Name: name, Policy: name, Behavior: "domain", Format: "yaml", URL: provider.URL + "/" + name, Interval: 86400, Enabled: true, Position: 300}))
	}
	state := snapshot(t, store)
	requireOK(t, store.SaveSelection(background, "ai", "node-"+state.Nodes[0].ID))
	requireOK(t, store.SaveSelection(background, "video", "node-"+state.Nodes[1].ID))
	port := freePort(t)
	requireOK(t, store.SaveListener(background, Listener{Name: "rules", Mode: ListenerModeRule, Port: port, Enabled: true}))
	apply := func() {
		t.Helper()
		if res := m.Apply(background); !res.Applied {
			t.Fatal(res.Error)
		}
	}
	apply()
	for _, name := range []string{"ai", "video"} {
		requireOK(t, kernel.RefreshRuleProvider(background, name))
	}
	proxyURL, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", port))
	transport := &http.Transport{Proxy: http.ProxyURL(proxyURL), DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 4 * time.Second}
	assertRoute := func(target, want string) {
		t.Helper()
		res, err := client.Get("http://" + target + ".demo.test/fixture")
		requireOK(t, err)
		body, err := io.ReadAll(res.Body)
		res.Body.Close()
		requireOK(t, err)
		if string(body) != want {
			t.Fatalf("%s: got %q, want %s", target, body, want)
		}
	}
	assertRoute("ai", "via-A")
	assertRoute("video", "via-B")
	requireOK(t, store.SaveSelection(background, "ai", "node-"+state.Nodes[1].ID))
	apply()
	assertRoute("ai", "via-B")
	assertRoute("video", "via-B")
	// Reopen both the manager database and the real kernel from last-good.yaml.
	stop()
	requireOK(t, store.Close())
	store, err = OpenStore(filepath.Join(dir, "manager.db"))
	requireOK(t, err)
	m.Store = store
	requireOK(t, m.Bootstrap(background))
	start()
	assertRoute("ai", "via-B")
	assertRoute("video", "via-B")
	apply()
	assertRoute("ai", "via-B")
}
