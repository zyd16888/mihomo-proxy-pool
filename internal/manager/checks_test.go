package manager

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type blockingCheckKernel struct {
	*fakeKernel
	mu                sync.Mutex
	active, maxActive int
	calls             map[string]int
	started           chan string
	release           chan struct{}
	failure           error
}

func (k *blockingCheckKernel) Delay(ctx context.Context, id string) (int, error) {
	k.mu.Lock()
	k.active++
	k.calls[id]++
	if k.active > k.maxActive {
		k.maxActive = k.active
	}
	failure := k.failure
	k.mu.Unlock()
	defer func() { k.mu.Lock(); k.active--; k.mu.Unlock() }()
	k.started <- id
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-k.release:
		return 427, failure
	}
}

func checksForTest(t *testing.T, count int) (*Manager, *NodeChecks, *blockingCheckKernel, []string) {
	t.Helper()
	m, _ := managerForTest(t)
	k := &blockingCheckKernel{fakeKernel: &fakeKernel{}, calls: map[string]int{}, started: make(chan string, 64), release: make(chan struct{})}
	m.Kernel = k
	for i := 0; i < count; i++ {
		requireOK(t, m.Store.Import(background, "", parsed(t, nodeJSON(fmt.Sprintf("Node %02d", i), fmt.Sprint(18000+i)))))
	}
	if result := m.Apply(background); !result.Applied {
		t.Fatal(result)
	}
	c := NewNodeChecks(m)
	t.Cleanup(c.Close)
	ids := []string{}
	for _, n := range snapshot(t, m.Store).Nodes {
		ids = append(ids, n.ID)
	}
	return m, c, k, ids
}

func awaitCheck(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for node check")
		}
		time.Sleep(time.Millisecond)
	}
}
func awaitStarted(t *testing.T, k *blockingCheckKernel) {
	t.Helper()
	select {
	case <-k.started:
	case <-time.After(3 * time.Second):
		t.Fatal("probe never started")
	}
}

func TestNodeChecksShareGlobalLimitAndDeduplicate(t *testing.T) {
	m, c, k, ids := checksForTest(t, 9)
	requireOK(t, c.StartSingle(background, ids[0]))
	awaitStarted(t, k)
	requireOK(t, c.StartBatch(background, append(ids[:8], ids[0])))
	batchID := c.Snapshot(snapshot(t, m.Store)).Batch.ID
	requireOK(t, c.StartBatch(background, ids))
	if c.Snapshot(snapshot(t, m.Store)).Batch.ID != batchID {
		t.Fatal("duplicate request replaced active batch")
	}
	requireOK(t, c.StartSingle(background, ids[0]))
	requireOK(t, c.StartSingle(background, ids[8]))
	for i := 0; i < 3; i++ {
		awaitStarted(t, k)
	}
	k.mu.Lock()
	active := k.active
	k.mu.Unlock()
	if active != 4 {
		t.Fatalf("expected 4 shared slots, got %d", active)
	}
	close(k.release)
	awaitCheck(t, func() bool {
		view := c.Snapshot(snapshot(t, m.Store))
		if view.Batch.Status != "completed" {
			return false
		}
		for _, r := range view.Results {
			if r.InFlight {
				return false
			}
		}
		return true
	})
	view := c.Snapshot(snapshot(t, m.Store))
	if view.Batch.Total != 8 || view.Batch.Succeeded != 8 {
		t.Fatalf("incorrect batch count: %+v", view.Batch)
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.maxActive > 4 {
		t.Fatal("exceeded shared concurrency")
	}
	for _, id := range ids {
		if k.calls[id] != 1 {
			t.Fatalf("node probed more than once: %d", k.calls[id])
		}
	}
}

func TestStoppingBatchCancelsQueueButPreservesSharedSingle(t *testing.T) {
	m, c, k, ids := checksForTest(t, 8)
	requireOK(t, c.StartSingle(background, ids[0]))
	awaitStarted(t, k)
	requireOK(t, c.StartBatch(background, ids))
	for i := 0; i < 3; i++ {
		awaitStarted(t, k)
	}
	batchID := c.Snapshot(snapshot(t, m.Store)).Batch.ID
	requireOK(t, c.StopBatch(batchID))
	awaitCheck(t, func() bool { return c.Snapshot(snapshot(t, m.Store)).Batch.Status == "stopped" })
	view := c.Snapshot(snapshot(t, m.Store))
	if view.Batch.Completed != view.Batch.Total || view.Batch.Cancelled != view.Batch.Total {
		t.Fatalf("incomplete stop: %+v", view.Batch)
	}
	if !view.Results[ids[0]].InFlight {
		t.Fatal("batch stop cancelled a separately started probe")
	}
	k.mu.Lock()
	calls := 0
	for _, count := range k.calls {
		calls += count
	}
	k.mu.Unlock()
	if calls > 4 {
		t.Fatal("queued probes started after stop")
	}
	close(k.release)
	awaitCheck(t, func() bool { return c.Snapshot(snapshot(t, m.Store)).Results[ids[0]].Status == "success" })
}

func TestNodeCheckResultsStayInMemoryAndFailureClearsDelay(t *testing.T) {
	m, c, k, ids := checksForTest(t, 1)
	before := snapshot(t, m.Store)
	config, err := os.ReadFile(filepath.Join(m.Dir, "config.yaml"))
	requireOK(t, err)
	close(k.release)
	requireOK(t, c.StartSingle(background, ids[0]))
	awaitCheck(t, func() bool { return c.Snapshot(snapshot(t, m.Store)).Results[ids[0]].Status == "success" })
	view := c.Snapshot(snapshot(t, m.Store))
	if view.Results[ids[0]].DelayMS == nil || *view.Results[ids[0]].DelayMS != 427 {
		t.Fatal("missing latency")
	}
	k.mu.Lock()
	k.failure = context.DeadlineExceeded
	k.mu.Unlock()
	requireOK(t, c.StartSingle(background, ids[0]))
	awaitCheck(t, func() bool { return c.Snapshot(snapshot(t, m.Store)).Results[ids[0]].Status == "timeout" })
	view = c.Snapshot(snapshot(t, m.Store))
	if view.Results[ids[0]].DelayMS != nil || view.Results[ids[0]].CheckedAt == "" {
		t.Fatal("failed check retained a successful latency")
	}
	after := snapshot(t, m.Store)
	if before.Revision != after.Revision || before.AppliedRevision != after.AppliedRevision {
		t.Fatal("probe changed config revision")
	}
	current, err := os.ReadFile(filepath.Join(m.Dir, "config.yaml"))
	requireOK(t, err)
	if !bytes.Equal(config, current) {
		t.Fatal("probe rewrote kernel configuration")
	}
	fresh := NewNodeChecks(m)
	defer fresh.Close()
	if len(fresh.Snapshot(after).Results) != 0 {
		t.Fatal("new service restored probe state")
	}
}

func TestProbeCannotAttachToChangedOrDeletedNode(t *testing.T) {
	for _, mode := range []string{"changed", "deleted"} {
		t.Run(mode, func(t *testing.T) {
			m, c, k, ids := checksForTest(t, 1)
			requireOK(t, c.StartSingle(background, ids[0]))
			awaitStarted(t, k)
			if mode == "changed" {
				requireOK(t, m.Store.Import(background, "", parsed(t, nodeJSON("Node 00", "19999"))))
			} else {
				requireOK(t, m.Store.DeleteNode(background, ids[0]))
			}
			if r := m.Apply(background); !r.Applied {
				t.Fatal(r)
			}
			close(k.release)
			awaitCheck(t, func() bool { k.mu.Lock(); defer k.mu.Unlock(); return k.active == 0 })
			c.Close()
			view := c.Snapshot(snapshot(t, m.Store))
			if mode == "deleted" {
				if _, exists := view.Results[ids[0]]; exists {
					t.Fatal("deleted node result resurrected")
				}
			} else if view.Results[ids[0]].Status != "stale" || view.Results[ids[0]].DelayMS != nil {
				t.Fatal("old config result remained current")
			}
		})
	}
}

func TestChecksSkipUnavailableNodesAndRequireAppliedConfig(t *testing.T) {
	m, c, k, ids := checksForTest(t, 2)
	requireOK(t, m.Store.SetNodeEnabled(background, ids[1], false))
	if err := c.StartSingle(background, ids[0]); err == nil {
		t.Fatal("probed unapplied config")
	}
	if r := m.Apply(background); !r.Applied {
		t.Fatal(r)
	}
	if err := c.StartSingle(background, ids[1]); err == nil {
		t.Fatal("probed disabled node")
	}
	close(k.release)
	requireOK(t, c.StartBatch(background, append(ids, "missing")))
	awaitCheck(t, func() bool { return c.Snapshot(snapshot(t, m.Store)).Batch.Status == "completed" })
	if c.Snapshot(snapshot(t, m.Store)).Batch.Total != 1 {
		t.Fatal("invalid nodes entered batch")
	}
}

func TestChecksCloseCancelsAllBackgroundWork(t *testing.T) {
	m, c, k, ids := checksForTest(t, 9)
	requireOK(t, c.StartBatch(background, ids))
	awaitStarted(t, k)
	finished := make(chan struct{})
	go func() { c.Close(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown hung")
	}
	if c.Snapshot(snapshot(t, m.Store)).Batch.Status != "stopped" {
		t.Fatal("batch remained running after shutdown")
	}
	if err := c.StartSingle(background, ids[0]); err == nil {
		t.Fatal("closed service accepted new work")
	}
}

func TestSingleCheckHTTPReturnsBeforeProbeFinishes(t *testing.T) {
	m, _, k, ids := checksForTest(t, 1)
	srv := NewServer(m, "test-key")
	defer srv.Close()
	srv.sessions["test"] = futureTime()
	r := httptest.NewRequest("POST", "/api/nodes/"+ids[0]+"/check", bytes.NewBufferString(`{}`))
	r.Header.Set("Content-Type", "application/json")
	r.AddCookie(&http.Cookie{Name: "manager_session", Value: "test"})
	w := httptest.NewRecorder()
	srv.Handler(http.NotFoundHandler()).ServeHTTP(w, r)
	if w.Code != http.StatusAccepted {
		t.Fatal(w.Body.String())
	}
	awaitStarted(t, k)
	if !srv.Checks.Snapshot(snapshot(t, m.Store)).Results[ids[0]].InFlight {
		t.Fatal("request waited for network completion")
	}
	k.mu.Lock()
	k.failure = errors.New("connection refused")
	k.mu.Unlock()
	close(k.release)
}

func TestExpiredProbeDoesNotReappearAfterConfigIsRestored(t *testing.T) {
	m, c, k, ids := checksForTest(t, 1)
	close(k.release)
	requireOK(t, c.StartSingle(background, ids[0]))
	awaitCheck(t, func() bool { return c.Snapshot(snapshot(t, m.Store)).Results[ids[0]].Status == "success" })
	requireOK(t, m.Store.Import(background, "", parsed(t, nodeJSON("Node 00", "19999"))))
	if c.Snapshot(snapshot(t, m.Store)).Results[ids[0]].Status != "stale" {
		t.Fatal("changed config retained latency")
	}
	requireOK(t, m.Store.Import(background, "", parsed(t, nodeJSON("Node 00", "18000"))))
	if result := c.Snapshot(snapshot(t, m.Store)).Results[ids[0]]; result.Status != "stale" || result.DelayMS != nil {
		t.Fatal("expired latency reappeared")
	}
}
