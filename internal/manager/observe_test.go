package manager

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func logLine(level, payload string) string {
	raw, _ := json.Marshal(map[string]string{"type": level, "payload": payload})
	return string(raw)
}

func observerForTest(t *testing.T, lines ...string) (*Observer, *fakeKernel) {
	t.Helper()
	m, kernel := managerForTest(t)
	kernel.logLines = lines
	observer := NewObserver(m)
	t.Cleanup(observer.Close)
	return observer, kernel
}

func awaitLogs(t *testing.T, o *Observer, want int) LogSnapshot {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		snapshot := o.Logs(0)
		if len(snapshot.Entries) >= want {
			return snapshot
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected %d log entries, have %d", want, len(snapshot.Entries))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestLogsStreamIntoTheBufferAndPollBySequence(t *testing.T) {
	observer, _ := observerForTest(t,
		logLine("info", "[TCP] 127.0.0.1:5000 --> example.com:443 match RuleSet(cn) using DIRECT"),
		logLine("warning", "[TCP] dial failed"),
	)
	first := awaitLogs(t, observer, 2)
	if first.Entries[0].Level != "info" || first.Entries[1].Level != "warning" {
		t.Fatalf("levels not preserved: %+v", first.Entries)
	}
	if first.Entries[0].Seq != 1 || first.Entries[1].Seq != 2 {
		t.Fatal("sequence numbers are not consecutive from one")
	}
	if !first.Connected {
		t.Fatal("stream reported as disconnected while delivering entries")
	}
	// A poll that carries the last sequence must return only what is new,
	// which is what keeps the page from re-rendering the whole window.
	repeat := observer.Logs(first.NextSeq - 1)
	if len(repeat.Entries) != 0 {
		t.Fatalf("poll returned %d entries already seen", len(repeat.Entries))
	}
}

func TestLogBufferDropsOldestAndReportsTheGap(t *testing.T) {
	observer, _ := observerForTest(t)
	for i := 0; i < logBufferSize+50; i++ {
		observer.append("info", "line")
	}
	snapshot := observer.Logs(0)
	if snapshot.Dropped != 50 {
		t.Fatalf("dropped %d entries, want 50", snapshot.Dropped)
	}
	// The window is bounded, and a single poll is bounded again so that a
	// busy kernel cannot hand the page an unbounded response.
	if len(snapshot.Entries) != logMaxPerPoll {
		t.Fatalf("poll returned %d entries, want at most %d", len(snapshot.Entries), logMaxPerPoll)
	}
	if snapshot.NextSeq != int64(logBufferSize+51) {
		t.Fatalf("sequence did not keep counting past the window: %d", snapshot.NextSeq)
	}
}

func TestLogLevelChangeReopensTheStream(t *testing.T) {
	observer, kernel := observerForTest(t, logLine("info", "first"))
	awaitLogs(t, observer, 1)
	requireOK(t, observer.SetLevel("debug"))
	deadline := time.Now().Add(3 * time.Second)
	for {
		kernel.mu.Lock()
		level := kernel.logLevel
		kernel.mu.Unlock()
		if level == "debug" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("stream still open at level %q", level)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if observer.Logs(0).Level != "debug" {
		t.Fatal("reported level did not follow the change")
	}
	if err := observer.SetLevel("verbose"); err == nil {
		t.Fatal("accepted a level the kernel does not define")
	}
}

func TestObserverSurvivesAnUnreachableKernel(t *testing.T) {
	m, kernel := managerForTest(t)
	kernel.logErr = errors.New("connection refused")
	observer := NewObserver(m)
	defer observer.Close()
	deadline := time.Now().Add(3 * time.Second)
	for {
		snapshot := observer.Logs(0)
		if !snapshot.Connected && snapshot.Error != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a kernel that never answers was never reported as disconnected")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Closing must not hang on the reconnect backoff.
	done := make(chan struct{})
	go func() { observer.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("observer did not stop while reconnecting")
	}
}

func TestConnectionsResolveGeneratedIdentifiersToNames(t *testing.T) {
	m, kernel := managerForTest(t)
	requireOK(t, m.Store.Import(background, "", parsed(t, nodeJSON("HK 01", "8881"))))
	node := snapshot(t, m.Store).Nodes[0]
	requireOK(t, m.Store.SaveListener(background, Listener{Name: "采集线路 A", Port: 17891, Mode: ListenerModeNode, NodeID: node.ID, Enabled: true}))
	state := snapshot(t, m.Store)
	listener := state.Listeners[0]

	var conn CoreConnection
	conn.ID = "abc"
	conn.Metadata.Network = "tcp"
	conn.Metadata.Type = "HTTP"
	conn.Metadata.SourceIP = "127.0.0.1"
	conn.Metadata.SourcePort = "5000"
	conn.Metadata.Host = "example.com"
	conn.Metadata.DestinationPort = "443"
	conn.Metadata.InboundName = "listener-" + listener.ID
	conn.Metadata.InboundPort = "17891"
	conn.Chains = []string{"node-" + node.ID, GroupSelect}
	conn.Rule = "RuleSet"
	conn.RulePayload = "cn"
	conn.Start = time.Now().Add(-30 * time.Second)
	kernel.connections = CoreConnections{Connections: []CoreConnection{conn}, DownloadTotal: 10, UploadTotal: 20}

	observer := NewObserver(m)
	defer observer.Close()
	snapshot, err := observer.Connections(background, state)
	requireOK(t, err)
	if len(snapshot.Connections) != 1 {
		t.Fatalf("expected one connection, got %d", len(snapshot.Connections))
	}
	got := snapshot.Connections[0]
	// The kernel speaks in generated identifiers; the page must show the
	// names the operator actually chose.
	if got.Listener != "采集线路 A" || got.ListenerID != listener.ID {
		t.Fatalf("listener not resolved: %+v", got)
	}
	if len(got.Chains) != 2 || got.Chains[0] != GroupSelect || got.Chains[1] != "HK 01" {
		t.Fatalf("chain not resolved inbound-first: %v", got.Chains)
	}
	if got.Target != "example.com:443" {
		t.Fatalf("target is %q", got.Target)
	}
	if got.Rule != "RuleSet(cn)" {
		t.Fatalf("matched rule is %q", got.Rule)
	}
	if got.ElapsedSecs < 29 || got.ElapsedSecs > 31 {
		t.Fatalf("elapsed time is %d seconds", got.ElapsedSecs)
	}
}

func TestConnectionsFallBackToRawInboundWhenListenerIsUnknown(t *testing.T) {
	m, kernel := managerForTest(t)
	var conn CoreConnection
	conn.ID = "abc"
	conn.Metadata.InboundName = "listener-removed"
	conn.Metadata.DestinationIP = "203.0.113.7"
	conn.Metadata.DestinationPort = "80"
	conn.Chains = []string{"DIRECT"}
	conn.Start = time.Now()
	kernel.connections = CoreConnections{Connections: []CoreConnection{conn}}
	observer := NewObserver(m)
	defer observer.Close()

	snapshot, err := observer.Connections(background, snapshot(t, m.Store))
	requireOK(t, err)
	got := snapshot.Connections[0]
	if got.Listener != "listener-removed" || got.ListenerID != "" {
		t.Fatalf("unknown inbound was not passed through: %+v", got)
	}
	// A connection with no host must still say where it went.
	if got.Target != "203.0.113.7:80" {
		t.Fatalf("target is %q", got.Target)
	}
}

func TestObserverReadsConnectionsWithoutTouchingConfiguration(t *testing.T) {
	m, kernel := managerForTest(t)
	requireOK(t, m.Store.Import(background, "", parsed(t, nodeJSON("A", "8881"))))
	before := snapshot(t, m.Store)
	kernel.mu.Lock()
	reloadsBefore := kernel.reloads
	kernel.mu.Unlock()

	observer := NewObserver(m)
	defer observer.Close()
	if _, err := observer.Connections(background, before); err != nil {
		t.Fatal(err)
	}
	requireOK(t, m.Kernel.CloseConnections(background))
	requireOK(t, m.Kernel.CloseConnection(background, "abc"))

	after := snapshot(t, m.Store)
	if after.Revision != before.Revision {
		t.Fatal("reading connections changed the configuration version")
	}
	kernel.mu.Lock()
	defer kernel.mu.Unlock()
	if kernel.reloads != reloadsBefore {
		t.Fatal("observing the kernel triggered a reload")
	}
	if len(kernel.closed) != 2 || kernel.closed[0] != "*" || kernel.closed[1] != "abc" {
		t.Fatalf("close requests not forwarded verbatim: %v", kernel.closed)
	}
}

func TestConnectionsReportKernelFailureInsteadOfEmptyList(t *testing.T) {
	m, _ := managerForTest(t)
	m.Kernel = failingConnectionsKernel{Kernel: m.Kernel}
	observer := NewObserver(m)
	defer observer.Close()
	if _, err := observer.Connections(context.Background(), snapshot(t, m.Store)); err == nil {
		t.Fatal("an unreachable kernel looked like zero connections")
	}
}

type failingConnectionsKernel struct{ Kernel }

func (failingConnectionsKernel) Connections(context.Context) (CoreConnections, error) {
	return CoreConnections{}, errors.New("kernel unreachable")
}
