package manager

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"mihomo-proxy/internal/importer"
)

var background = context.Background()

func storeForTest(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
func parsed(t *testing.T, raw string) []importer.Proxy {
	t.Helper()
	nodes, _, err := importer.Parse(importer.ImportRequest{Raw: raw})
	if err != nil {
		t.Fatal(err)
	}
	return nodes
}
func snapshot(t *testing.T, s *Store) State {
	t.Helper()
	v, err := s.Snapshot(background)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func requireOK(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func nodeJSON(name string, port string) string {
	return `[{"name":"` + name + `","type":"http","server":"127.0.0.1","port":` + port + `}]`
}

func TestSubscriptionIdentityAndMissingNode(t *testing.T) {
	s := storeForTest(t)
	requireOK(t, s.SaveSubscription(background, Subscription{Name: "airport", URL: "https://example.test/sub"}))
	source := snapshot(t, s).Subscriptions[0].ID
	requireOK(t, s.Import(background, source, parsed(t, nodeJSON("A", "8881"))))
	original := snapshot(t, s).Nodes[0]
	requireOK(t, s.SaveListener(background, Listener{Name: "fixed", Port: 17891, NodeID: original.ID, Enabled: true}))
	requireOK(t, s.Import(background, source, parsed(t, nodeJSON("A", "8882"))))
	state := snapshot(t, s)
	if state.Nodes[0].ID != original.ID || state.Nodes[0].Port != 8882 {
		t.Fatal("update changed identity")
	}
	requireOK(t, s.Import(background, source, parsed(t, nodeJSON("Renamed A", "8882"))))
	state = snapshot(t, s)
	if len(state.Nodes) != 1 || state.Nodes[0].ID != original.ID {
		t.Fatal("rename lost binding")
	}
	requireOK(t, s.Import(background, source, parsed(t, nodeJSON("B", "8883"))))
	state = snapshot(t, s)
	if len(state.Nodes) != 2 || state.Listeners[0].NodeID != original.ID {
		t.Fatal("missing node reassigned binding")
	}
	_, ports, err := BuildConfig(state, "127.0.0.1:9090", "key")
	requireOK(t, err)
	if len(ports) != 0 {
		t.Fatal("missing node listener still exposed")
	}
	if err := s.DeleteNode(background, original.ID); err == nil {
		t.Fatal("deleted referenced node")
	}
	if err := s.DeleteSubscription(background, source); err == nil {
		t.Fatal("deleted referenced subscription")
	}
}

func TestIndependentListenerLifecycleAndPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manager.db")
	s, err := OpenStore(path)
	requireOK(t, err)
	requireOK(t, s.Import(background, "", parsed(t, `[{"name":"A","type":"http","server":"127.0.0.1","port":8881},{"name":"B","type":"http","server":"127.0.0.1","port":8882}]`)))
	state := snapshot(t, s)
	if len(state.Listeners) != 0 {
		t.Fatal("import allocated ports")
	}
	requireOK(t, s.SaveListener(background, Listener{Name: "fixed", Port: 17891, NodeID: state.Nodes[0].ID, Enabled: true}))
	l := snapshot(t, s).Listeners[0]
	l.NodeID = state.Nodes[1].ID
	requireOK(t, s.SaveListener(background, l))
	requireOK(t, s.Close())
	s, err = OpenStore(path)
	requireOK(t, err)
	defer s.Close()
	reloaded := snapshot(t, s)
	if reloaded.Listeners[0].Port != 17891 || reloaded.Listeners[0].NodeID != l.NodeID {
		t.Fatal("binding not persisted")
	}
	requireOK(t, s.DeleteListener(background, l.ID))
	if len(snapshot(t, s).Nodes) != 2 {
		t.Fatal("listener deletion deleted node")
	}
}

func TestImportAliasesDoNotCollapse(t *testing.T) {
	s := storeForTest(t)
	items := parsed(t, `[{"name":"A","type":"http","server":"127.0.0.1","port":8881},{"name":"B","type":"http","server":"127.0.0.1","port":8881}]`)
	requireOK(t, s.Import(background, "", items))
	before := snapshot(t, s)
	requireOK(t, s.Import(background, "", items))
	after := snapshot(t, s)
	if len(after.Nodes) != 2 || after.Nodes[0].ID != before.Nodes[0].ID || after.Nodes[1].ID != before.Nodes[1].ID {
		t.Fatal("aliases collapsed or changed identity")
	}
}

func TestPortUniquenessAndUnknownUpdates(t *testing.T) {
	s := storeForTest(t)
	requireOK(t, s.Import(background, "", parsed(t, nodeJSON("A", "8881"))))
	id := snapshot(t, s).Nodes[0].ID
	l := Listener{Name: "first", Port: 17891, NodeID: id, Enabled: true}
	requireOK(t, s.SaveListener(background, l))
	before := snapshot(t, s).Revision
	if err := s.SaveListener(background, l); err == nil {
		t.Fatal("duplicate port accepted")
	}
	if snapshot(t, s).Revision != before {
		t.Fatal("failed save incremented revision")
	}
	l.ID = "missing"
	l.Port = 17892
	if err := s.SaveListener(background, l); err == nil {
		t.Fatal("unknown update created listener")
	}
}

type fakeKernel struct {
	validateErr error
	failReload  bool
	failVerify  bool
	reloads     int
	mu          sync.Mutex
}

func (f *fakeKernel) Validate(context.Context, string) error { return f.validateErr }
func (f *fakeKernel) Reload(context.Context, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reloads++
	if f.failReload {
		f.failReload = false
		return errors.New("reload rejected")
	}
	return nil
}
func (f *fakeKernel) Verify(context.Context, []int, []int) error {
	if f.failVerify {
		f.failVerify = false
		return errors.New("listener did not bind")
	}
	return nil
}
func (f *fakeKernel) Version(context.Context) (string, error)    { return "test", nil }
func (f *fakeKernel) Delay(context.Context, string) (int, error) { return 12, nil }
func managerForTest(t *testing.T) (*Manager, *fakeKernel) {
	t.Helper()
	s := storeForTest(t)
	kernel := &fakeKernel{}
	dir := t.TempDir()
	m := &Manager{Store: s, Kernel: kernel, Dir: dir, CoreAddr: "127.0.0.1:9090", Secret: "test"}
	requireOK(t, m.Bootstrap(background))
	return m, kernel
}

func TestFailedApplyRestoresPreviousRuntimeAndKeepsDesired(t *testing.T) {
	for _, mode := range []string{"reload", "verify"} {
		t.Run(mode, func(t *testing.T) {
			m, k := managerForTest(t)
			if r := m.Apply(background); !r.Applied {
				t.Fatal(r)
			}
			old, err := os.ReadFile(filepath.Join(m.Dir, "config.yaml"))
			requireOK(t, err)
			if mode == "reload" {
				k.failReload = true
			} else {
				k.failVerify = true
			}
			r, err := m.Change(background, func() error { return m.Store.Import(background, "", parsed(t, nodeJSON("A", "8881"))) })
			requireOK(t, err)
			if r.Applied || !strings.Contains(r.Error, "已恢复") {
				t.Fatalf("incorrect apply status: %+v", r)
			}
			current, err := os.ReadFile(filepath.Join(m.Dir, "config.yaml"))
			requireOK(t, err)
			if string(current) != string(old) {
				t.Fatal("runtime file not restored")
			}
			state := snapshot(t, m.Store)
			if state.Revision == state.AppliedRevision || len(state.Nodes) != 1 || state.LastError == "" {
				t.Fatal("desired state lost or falsely applied")
			}
			if r = m.Apply(background); !r.Applied {
				t.Fatal(r)
			}
			if snapshot(t, m.Store).LastError != "" {
				t.Fatal("retry did not clear failure")
			}
		})
	}
}

func TestInvalidConfigNeverReplacesActiveFile(t *testing.T) {
	m, k := managerForTest(t)
	old, err := os.ReadFile(filepath.Join(m.Dir, "config.yaml"))
	requireOK(t, err)
	k.validateErr = errors.New("bad config")
	r := m.Apply(background)
	if r.Applied || k.reloads != 0 {
		t.Fatal("invalid candidate was applied")
	}
	current, err := os.ReadFile(filepath.Join(m.Dir, "config.yaml"))
	requireOK(t, err)
	if string(current) != string(old) {
		t.Fatal("invalid candidate replaced active config")
	}
}

func TestPortConflictDoesNotReloadKernel(t *testing.T) {
	m, k := managerForTest(t)
	requireOK(t, m.Store.Import(background, "", parsed(t, nodeJSON("A", "8881"))))
	id := snapshot(t, m.Store).Nodes[0].ID
	occupied, err := net.Listen("tcp4", "127.0.0.1:0")
	requireOK(t, err)
	defer occupied.Close()
	port := occupied.Addr().(*net.TCPAddr).Port
	requireOK(t, m.Store.SaveListener(background, Listener{Name: "conflict", Port: port, NodeID: id, Enabled: true}))
	r := m.Apply(background)
	if r.Applied || k.reloads != 0 || !strings.Contains(r.Error, "占用") {
		t.Fatalf("port conflict ignored: %+v", r)
	}
}

func TestBootstrapUsesLastAcknowledgedConfig(t *testing.T) {
	m, _ := managerForTest(t)
	if r := m.Apply(background); !r.Applied {
		t.Fatal(r)
	}
	good, err := os.ReadFile(filepath.Join(m.Dir, "config.yaml"))
	requireOK(t, err)
	requireOK(t, os.WriteFile(filepath.Join(m.Dir, "config.yaml"), []byte("unacknowledged candidate"), 0600))
	requireOK(t, m.Bootstrap(background))
	restored, err := os.ReadFile(filepath.Join(m.Dir, "config.yaml"))
	requireOK(t, err)
	if string(restored) != string(good) {
		t.Fatal("booted unacknowledged config")
	}
}

func TestConcurrentChangesAreSerialized(t *testing.T) {
	m, _ := managerForTest(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := m.Change(background, func() error {
				return m.Store.SaveSubscription(background, Subscription{Name: newID(), URL: "https://example.test/" + newID()})
			})
			if err != nil || !r.Applied {
				t.Errorf("change failed: %v %+v", err, r)
			}
		}()
	}
	wg.Wait()
	state := snapshot(t, m.Store)
	if state.Revision != 8 || state.AppliedRevision != 8 || len(state.Subscriptions) != 8 {
		t.Fatalf("lost concurrent change: %+v", state)
	}
}

func TestBootstrapUpdatesInternalControllerAddress(t *testing.T) {
	m, _ := managerForTest(t)
	if r := m.Apply(background); !r.Applied {
		t.Fatal(r)
	}
	m.CoreAddr = "127.0.0.1:19090"
	requireOK(t, m.Bootstrap(background))
	raw, err := os.ReadFile(filepath.Join(m.Dir, "config.yaml"))
	requireOK(t, err)
	if !strings.Contains(string(raw), "127.0.0.1:19090") {
		t.Fatal("bootstrap retained obsolete controller address")
	}
}
