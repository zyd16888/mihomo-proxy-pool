package manager

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"
)

// CoreConnections mirrors the kernel connection snapshot. Only the fields the
// management page shows are decoded.
type CoreConnections struct {
	DownloadTotal int64            `json:"downloadTotal"`
	UploadTotal   int64            `json:"uploadTotal"`
	Connections   []CoreConnection `json:"connections"`
}

type CoreConnection struct {
	ID       string `json:"id"`
	Metadata struct {
		Network         string `json:"network"`
		Type            string `json:"type"`
		SourceIP        string `json:"sourceIP"`
		SourcePort      string `json:"sourcePort"`
		DestinationIP   string `json:"destinationIP"`
		DestinationPort string `json:"destinationPort"`
		Host            string `json:"host"`
		InboundName     string `json:"inboundName"`
		InboundPort     string `json:"inboundPort"`
		Process         string `json:"process"`
	} `json:"metadata"`
	Upload      int64     `json:"upload"`
	Download    int64     `json:"download"`
	Start       time.Time `json:"start"`
	Chains      []string  `json:"chains"`
	Rule        string    `json:"rule"`
	RulePayload string    `json:"rulePayload"`
}

// Connection is one connection rewritten into the names the operator chose,
// instead of the generated node-<id> and listener-<id> identifiers.
type Connection struct {
	ID          string   `json:"id"`
	Network     string   `json:"network"`
	Kind        string   `json:"kind"`
	Source      string   `json:"source"`
	Target      string   `json:"target"`
	Listener    string   `json:"listener"`
	ListenerID  string   `json:"listenerId,omitempty"`
	Port        string   `json:"port"`
	Chains      []string `json:"chains"`
	Rule        string   `json:"rule"`
	Upload      int64    `json:"upload"`
	Download    int64    `json:"download"`
	StartedAt   string   `json:"startedAt"`
	ElapsedSecs int64    `json:"elapsedSeconds"`
}

type ConnectionSnapshot struct {
	Connections   []Connection `json:"connections"`
	DownloadTotal int64        `json:"downloadTotal"`
	UploadTotal   int64        `json:"uploadTotal"`
}

type LogEntry struct {
	Seq     int64  `json:"seq"`
	Time    string `json:"time"`
	Level   string `json:"level"`
	Payload string `json:"payload"`
}

type LogSnapshot struct {
	Entries   []LogEntry `json:"entries"`
	NextSeq   int64      `json:"nextSeq"`
	Level     string     `json:"level"`
	Connected bool       `json:"connected"`
	Error     string     `json:"error,omitempty"`
	Dropped   int64      `json:"dropped"`
}

const (
	logBufferSize   = 2000
	logMaxPerPoll   = 500
	logPayloadLimit = 2000
)

var logLevels = map[string]bool{"debug": true, "info": true, "warning": true, "error": true, "silent": true}

// Observer keeps a bounded window of kernel logs in memory and reads
// connections on demand. Nothing here is persisted: the log window is a live
// diagnostic view, and writing it to disk would grow without bound.
type Observer struct {
	manager *Manager
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup

	mu        sync.Mutex
	entries   []LogEntry
	nextSeq   int64
	dropped   int64
	level     string
	connected bool
	lastErr   string
	restart   chan struct{}
}

func NewObserver(m *Manager) *Observer {
	ctx, cancel := context.WithCancel(context.Background())
	o := &Observer{
		manager: m, ctx: ctx, cancel: cancel,
		entries: make([]LogEntry, 0, logBufferSize),
		level:   "info", nextSeq: 1, restart: make(chan struct{}, 1),
	}
	o.wg.Add(1)
	go func() { defer o.wg.Done(); o.run() }()
	return o
}

func (o *Observer) Close() {
	o.cancel()
	o.wg.Wait()
}

// run keeps one log stream open, reconnecting after kernel restarts and after
// a level change. A disconnected kernel is an expected state, not an error.
func (o *Observer) run() {
	backoff := time.Second
	for {
		if o.ctx.Err() != nil {
			return
		}
		o.mu.Lock()
		level := o.level
		o.mu.Unlock()
		streamCtx, cancel := context.WithCancel(o.ctx)
		watch := make(chan struct{})
		go func() {
			defer close(watch)
			select {
			case <-o.restart:
				cancel()
			case <-streamCtx.Done():
			}
		}()
		err := o.consume(streamCtx, level)
		cancel()
		<-watch
		if o.ctx.Err() != nil {
			return
		}
		o.mu.Lock()
		o.connected = false
		if err != nil && !errors.Is(err, context.Canceled) {
			o.lastErr = err.Error()
		} else {
			o.lastErr = ""
		}
		o.mu.Unlock()
		if err == nil {
			backoff = time.Second
		}
		select {
		case <-o.ctx.Done():
			return
		case <-o.restart:
			backoff = time.Second
		case <-time.After(backoff):
			if backoff < 15*time.Second {
				backoff *= 2
			}
		}
	}
}

func (o *Observer) consume(ctx context.Context, level string) error {
	if level == "silent" {
		<-ctx.Done()
		return ctx.Err()
	}
	body, err := o.manager.Kernel.StreamLogs(ctx, level)
	if err != nil {
		return err
	}
	defer body.Close()
	o.mu.Lock()
	o.connected = true
	o.lastErr = ""
	o.mu.Unlock()
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 8192), 256*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var entry struct {
			Type    string `json:"type"`
			Payload string `json:"payload"`
		}
		if json.Unmarshal([]byte(line), &entry) != nil {
			continue
		}
		o.append(entry.Type, entry.Payload)
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return ctx.Err()
}

func (o *Observer) append(level, payload string) {
	if len(payload) > logPayloadLimit {
		payload = payload[:logPayloadLimit] + "…"
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	entry := LogEntry{Seq: o.nextSeq, Time: time.Now().UTC().Format(time.RFC3339), Level: level, Payload: payload}
	o.nextSeq++
	if len(o.entries) == logBufferSize {
		o.entries = append(o.entries[:0], o.entries[1:]...)
		o.dropped++
	}
	o.entries = append(o.entries, entry)
}

// Logs returns entries newer than since. A since below the retained window
// reports the gap through Dropped rather than silently skipping entries.
func (o *Observer) Logs(since int64) LogSnapshot {
	o.mu.Lock()
	defer o.mu.Unlock()
	snapshot := LogSnapshot{
		NextSeq: o.nextSeq, Level: o.level, Connected: o.connected,
		Error: o.lastErr, Dropped: o.dropped, Entries: []LogEntry{},
	}
	for _, entry := range o.entries {
		if entry.Seq <= since {
			continue
		}
		snapshot.Entries = append(snapshot.Entries, entry)
	}
	if len(snapshot.Entries) > logMaxPerPoll {
		snapshot.Entries = snapshot.Entries[len(snapshot.Entries)-logMaxPerPoll:]
	}
	return snapshot
}

func (o *Observer) SetLevel(level string) error {
	if !logLevels[level] {
		return errors.New("日志级别必须是 debug、info、warning、error 或 silent")
	}
	o.mu.Lock()
	if o.level == level {
		o.mu.Unlock()
		return nil
	}
	o.level = level
	o.connected = false
	o.mu.Unlock()
	select {
	case o.restart <- struct{}{}:
	default:
	}
	return nil
}

func (o *Observer) Clear() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.entries = o.entries[:0]
	o.dropped = 0
}

// Connections resolves generated identifiers back to the names the operator
// gave, so a connection can be traced to the listener that accepted it.
func (o *Observer) Connections(ctx context.Context, state State) (ConnectionSnapshot, error) {
	raw, err := o.manager.Kernel.Connections(ctx)
	if err != nil {
		return ConnectionSnapshot{}, err
	}
	nodeNames := map[string]string{}
	for _, n := range state.Nodes {
		nodeNames["node-"+n.ID] = n.Name
	}
	for _, edit := range state.CategoryEdits {
		if edit.Label != "" {
			nodeNames[edit.Name] = edit.Label
		}
	}
	listeners := map[string]Listener{}
	for _, l := range state.Listeners {
		listeners["listener-"+l.ID] = l
	}
	snapshot := ConnectionSnapshot{
		Connections:   []Connection{},
		DownloadTotal: raw.DownloadTotal,
		UploadTotal:   raw.UploadTotal,
	}
	now := time.Now()
	for _, item := range raw.Connections {
		meta := item.Metadata
		target := meta.Host
		if target == "" {
			target = meta.DestinationIP
		}
		if meta.DestinationPort != "" {
			target += ":" + meta.DestinationPort
		}
		chains := make([]string, 0, len(item.Chains))
		// The kernel reports the chain outermost first; reading it as the
		// operator does, from inbound to outbound, means reversing it.
		for i := len(item.Chains) - 1; i >= 0; i-- {
			hop := item.Chains[i]
			if name, ok := nodeNames[hop]; ok {
				hop = name
			}
			chains = append(chains, hop)
		}
		rule := item.Rule
		if item.RulePayload != "" {
			rule += "(" + item.RulePayload + ")"
		}
		conn := Connection{
			ID:          item.ID,
			Network:     strings.ToUpper(meta.Network),
			Kind:        meta.Type,
			Source:      meta.SourceIP + ":" + meta.SourcePort,
			Target:      target,
			Listener:    meta.InboundName,
			Port:        meta.InboundPort,
			Chains:      chains,
			Rule:        rule,
			Upload:      item.Upload,
			Download:    item.Download,
			StartedAt:   item.Start.UTC().Format(time.RFC3339),
			ElapsedSecs: int64(now.Sub(item.Start).Seconds()),
		}
		if listener, ok := listeners[meta.InboundName]; ok {
			conn.Listener = listener.Name
			conn.ListenerID = listener.ID
		}
		snapshot.Connections = append(snapshot.Connections, conn)
	}
	return snapshot, nil
}
