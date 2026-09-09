package manager

import (
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

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	requireOK(t, err)
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port
}
func futureTime() time.Time { return time.Now().Add(time.Hour) }

// Opt in with MIHOMO_TEST_BIN. Uses only local proxy fixtures, never a subscription
// or external target. Covers real sockets, routing, reload, and kernel restart.
func TestRealKernelListenerLifecycle(t *testing.T) {
	binary := os.Getenv("MIHOMO_TEST_BIN")
	if binary == "" {
		t.Skip("set MIHOMO_TEST_BIN to run local kernel integration")
	}
	binary, err := filepath.Abs(binary)
	requireOK(t, err)
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, "manager.db"))
	requireOK(t, err)
	defer store.Close()
	coreAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(freePort(t)))
	kernel := NewRuntime(binary, dir, "http://"+coreAddr, "test-secret")
	m := &Manager{Store: store, Kernel: kernel, Dir: dir, CoreAddr: coreAddr, Secret: "test-secret"}
	requireOK(t, m.Bootstrap(background))
	startCore := func() (*exec.Cmd, *os.File) {
		logfile, err := os.CreateTemp(dir, "kernel-*.log")
		requireOK(t, err)
		cmd := exec.Command(binary, "-d", dir, "-f", filepath.Join(dir, "config.yaml"))
		cmd.Stdout = logfile
		cmd.Stderr = logfile
		requireOK(t, cmd.Start())
		ready := false
		for i := 0; i < 100; i++ {
			ctx, cancel := context.WithTimeout(background, 100*time.Millisecond)
			_, err := kernel.Version(ctx)
			cancel()
			if err == nil {
				ready = true
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if !ready {
			cmd.Process.Kill()
			cmd.Wait()
			logfile.Close()
			logs, _ := os.ReadFile(logfile.Name())
			t.Fatalf("kernel did not start: %s", logs)
		}
		return cmd, logfile
	}
	cmd, logfile := startCore()
	defer func() {
		if cmd != nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
		logfile.Close()
	}()
	proxyA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "via-A") }))
	defer proxyA.Close()
	proxyB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "via-B") }))
	defer proxyB.Close()
	_, portA, _ := net.SplitHostPort(strings.TrimPrefix(proxyA.URL, "http://"))
	_, portB, _ := net.SplitHostPort(strings.TrimPrefix(proxyB.URL, "http://"))
	change := func(fn func() error) {
		t.Helper()
		res, err := m.Change(background, fn)
		requireOK(t, err)
		if !res.Applied {
			t.Fatal(res.Error)
		}
	}
	change(func() error { return store.Import(background, "", parsed(t, nodeJSON("A", portA))) })
	change(func() error { return store.Import(background, "", parsed(t, nodeJSON("B", portB))) })
	state := snapshot(t, store)
	port := freePort(t)
	change(func() error {
		return store.SaveListener(background, Listener{Name: "fixed", Port: port, NodeID: state.Nodes[0].ID, Enabled: true})
	})
	proxyURL, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", port))
	transport := &http.Transport{Proxy: http.ProxyURL(proxyURL), DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	assertRoute := func(want string) {
		t.Helper()
		res, err := client.Get(proxyA.URL + "/fixture")
		requireOK(t, err)
		body, err := io.ReadAll(res.Body)
		res.Body.Close()
		requireOK(t, err)
		if string(body) != want {
			t.Fatalf("wrong route: want %s, got %s", want, body)
		}
	}
	assertRoute("via-A")
	listener := snapshot(t, store).Listeners[0]
	listener.NodeID = state.Nodes[1].ID
	change(func() error { return store.SaveListener(background, listener) })
	assertRoute("via-B")
	// Restart from persisted last-good configuration, preserving port and binding.
	cmd.Process.Kill()
	cmd.Wait()
	cmd = nil
	logfile.Close()
	requireOK(t, m.Bootstrap(background))
	cmd, logfile = startCore()
	assertRoute("via-B")
	listener.Enabled = false
	change(func() error { return store.SaveListener(background, listener) })
	if err := socksProbe(background, port); err == nil {
		t.Fatal("disabled listener remained open")
	}
	listener.Enabled = true
	change(func() error { return store.SaveListener(background, listener) })
	assertRoute("via-B")
	change(func() error { return store.DeleteListener(background, listener.ID) })
	if err := socksProbe(background, port); err == nil {
		t.Fatal("deleted listener remained open")
	}
	if len(snapshot(t, store).Nodes) != 2 {
		t.Fatal("listener deletion removed nodes")
	}
}
