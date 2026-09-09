package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ExitProbeGroup is a selector that exists only to serve the exit probe
// listener. Probing switches this group instead of anything a listener uses,
// so a probe never changes where live traffic goes.
const ExitProbeGroup = "⚙️ 出口探测"

// DefaultExitIPService is queried through the proxy under test, so the address
// it reports is the address the remote side sees.
const DefaultExitIPService = "https://ip9.com.cn/get"

// The service allows sixty requests per minute per source address. Probes run
// one at a time with a gap, which stays well inside that for a shared exit.
const exitProbeInterval = 1100 * time.Millisecond

type ExitIPResult struct {
	Status    string `json:"status"`
	IP        string `json:"ip,omitempty"`
	Country   string `json:"country,omitempty"`
	Region    string `json:"region,omitempty"`
	City      string `json:"city,omitempty"`
	ISP       string `json:"isp,omitempty"`
	ASN       string `json:"asn,omitempty"`
	CheckedAt string `json:"checkedAt,omitempty"`
	Error     string `json:"error,omitempty"`
	InFlight  bool   `json:"inFlight"`
}

type ExitProbeBatch struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	Total     int    `json:"total"`
	Completed int    `json:"completed"`
	Succeeded int    `json:"succeeded"`
	Failed    int    `json:"failed"`
}

type ExitProbeSnapshot struct {
	Nodes     map[string]ExitIPResult `json:"nodes"`
	Listeners map[string]ExitIPResult `json:"listeners"`
	Batch     *ExitProbeBatch         `json:"batch,omitempty"`
	Service   string                  `json:"service"`
}

// ip9Response is the documented shape of the lookup service.
type ip9Response struct {
	Ret  int `json:"ret"`
	Data struct {
		IP      string `json:"ip"`
		Country string `json:"country"`
		Prov    string `json:"prov"`
		City    string `json:"city"`
		ISP     string `json:"isp"`
		ASN     string `json:"ip_asn"`
	} `json:"data"`
}

// ExitProbes reports the address each node and rule listener exits from.
// Results live in memory only: an exit address is a live observation, and a
// stored one would go stale without any signal that it had.
type ExitProbes struct {
	manager *Manager
	service string
	// probePort is the loopback listener bound to ExitProbeGroup.
	probePort int

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu        sync.Mutex
	closed    bool
	nodes     map[string]ExitIPResult
	listeners map[string]ExitIPResult
	batch     *ExitProbeBatch
	cancelRun context.CancelFunc
	// running serializes probes: the probe group is shared state, so two
	// concurrent probes would attribute each other's exit address.
	running bool
	lastRun time.Time
}

func NewExitProbes(m *Manager, probePort int) *ExitProbes {
	ctx, cancel := context.WithCancel(context.Background())
	return &ExitProbes{
		manager: m, service: DefaultExitIPService, probePort: probePort,
		ctx: ctx, cancel: cancel,
		nodes: map[string]ExitIPResult{}, listeners: map[string]ExitIPResult{},
	}
}

func (e *ExitProbes) Close() {
	e.mu.Lock()
	e.closed = true
	e.mu.Unlock()
	e.cancel()
	e.wg.Wait()
}

func (e *ExitProbes) Snapshot(state State) ExitProbeSnapshot {
	e.mu.Lock()
	defer e.mu.Unlock()
	snapshot := ExitProbeSnapshot{
		Nodes: map[string]ExitIPResult{}, Listeners: map[string]ExitIPResult{},
		Service: e.service,
	}
	known := map[string]bool{}
	for _, n := range state.Nodes {
		known[n.ID] = true
	}
	for id, result := range e.nodes {
		if !known[id] {
			delete(e.nodes, id)
			continue
		}
		snapshot.Nodes[id] = result
	}
	knownListeners := map[string]bool{}
	for _, l := range state.Listeners {
		knownListeners[l.ID] = true
	}
	for id, result := range e.listeners {
		if !knownListeners[id] {
			delete(e.listeners, id)
			continue
		}
		snapshot.Listeners[id] = result
	}
	if e.batch != nil {
		batch := *e.batch
		snapshot.Batch = &batch
	}
	return snapshot
}

func (e *ExitProbes) prepare(ctx context.Context) (State, error) {
	state, err := e.manager.Snapshot(ctx)
	if err != nil {
		return state, err
	}
	if state.Revision != state.AppliedRevision || state.LastError != "" {
		return state, errors.New("请先成功应用当前配置，再探测出口 IP")
	}
	probe, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if _, err := e.manager.Kernel.Version(probe); err != nil {
		return state, errors.New("内核未就绪，暂时无法探测")
	}
	return state, nil
}

// StartNodes probes the listed nodes one at a time in the background.
func (e *ExitProbes) StartNodes(ctx context.Context, ids []string) error {
	state, err := e.prepare(ctx)
	if err != nil {
		return err
	}
	available := map[string]Node{}
	for _, n := range state.Nodes {
		if n.Enabled && n.Available {
			available[n.ID] = n
		}
	}
	targets := []string{}
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		if _, ok := available[id]; ok {
			targets = append(targets, id)
		}
	}
	if len(targets) == 0 {
		return errors.New("选中的节点均不存在、已停用或已失效，请刷新列表")
	}
	return e.start(targets, nil)
}

// StartListener probes one rule listener through its own port, which is the
// only way to learn where the rules actually send this destination.
func (e *ExitProbes) StartListener(ctx context.Context, id string) error {
	state, err := e.prepare(ctx)
	if err != nil {
		return err
	}
	for _, l := range state.Listeners {
		if l.ID != id {
			continue
		}
		if !l.Enabled {
			return errors.New("监听已停用，请先启用后再探测")
		}
		return e.start(nil, &l)
	}
	return errors.New("监听不存在")
}

func (e *ExitProbes) start(nodeIDs []string, listener *Listener) error {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return errors.New("探测服务正在停止")
	}
	if e.running {
		e.mu.Unlock()
		return errors.New("已有出口探测在进行，请等待完成")
	}
	runCtx, cancel := context.WithCancel(e.ctx)
	e.running = true
	e.cancelRun = cancel
	total := len(nodeIDs)
	if listener != nil {
		total = 1
	}
	batch := &ExitProbeBatch{ID: newID(), Status: "running", Total: total}
	e.batch = batch
	pending := ExitIPResult{Status: "queued", InFlight: true}
	for _, id := range nodeIDs {
		e.nodes[id] = pending
	}
	if listener != nil {
		e.listeners[listener.ID] = pending
	}
	e.mu.Unlock()

	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		defer cancel()
		defer func() {
			e.mu.Lock()
			e.running = false
			if batch.Status == "running" {
				if runCtx.Err() != nil {
					batch.Status = "stopped"
				} else {
					batch.Status = "completed"
				}
			}
			e.mu.Unlock()
		}()
		if listener != nil {
			e.probeListener(runCtx, *listener, batch)
			return
		}
		for _, id := range nodeIDs {
			if runCtx.Err() != nil {
				return
			}
			e.probeNode(runCtx, id, batch)
		}
	}()
	return nil
}

func (e *ExitProbes) Stop(id string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.batch == nil || e.batch.ID != id {
		return errors.New("探测任务不存在或已被新任务替换")
	}
	if e.batch.Status == "running" && e.cancelRun != nil {
		e.batch.Status = "stopping"
		e.cancelRun()
	}
	return nil
}

func (e *ExitProbes) record(target map[string]ExitIPResult, id string, result ExitIPResult, batch *ExitProbeBatch) {
	result.CheckedAt = time.Now().UTC().Format(time.RFC3339)
	e.mu.Lock()
	defer e.mu.Unlock()
	target[id] = result
	batch.Completed++
	if result.Status == "success" {
		batch.Succeeded++
	} else {
		batch.Failed++
	}
}

func (e *ExitProbes) probeNode(ctx context.Context, id string, batch *ExitProbeBatch) {
	e.mu.Lock()
	e.nodes[id] = ExitIPResult{Status: "running", InFlight: true}
	e.mu.Unlock()
	if err := e.pace(ctx); err != nil {
		return
	}
	selectCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	err := e.manager.Kernel.SelectProxy(selectCtx, ExitProbeGroup, "node-"+id)
	cancel()
	if err != nil {
		e.record(e.nodes, id, ExitIPResult{Status: "failed", Error: "无法切换到该节点：" + err.Error()}, batch)
		return
	}
	e.record(e.nodes, id, e.lookup(ctx, e.probePort), batch)
}

func (e *ExitProbes) probeListener(ctx context.Context, listener Listener, batch *ExitProbeBatch) {
	e.mu.Lock()
	e.listeners[listener.ID] = ExitIPResult{Status: "running", InFlight: true}
	e.mu.Unlock()
	if err := e.pace(ctx); err != nil {
		return
	}
	e.record(e.listeners, listener.ID, e.lookup(ctx, listener.Port), batch)
}

// pace keeps consecutive lookups apart so a shared exit address stays inside
// the service's per-address rate limit.
func (e *ExitProbes) pace(ctx context.Context) error {
	e.mu.Lock()
	wait := exitProbeInterval - time.Since(e.lastRun)
	e.lastRun = time.Now().Add(max(wait, 0))
	e.mu.Unlock()
	if wait <= 0 {
		return ctx.Err()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(wait):
		return nil
	}
}

// lookup asks the service, through the given local proxy port, which address
// it sees. Requests never bypass the proxy: a direct answer would report the
// host's own address and quietly look like a working node.
func (e *ExitProbes) lookup(ctx context.Context, port int) ExitIPResult {
	proxyURL, err := url.Parse("http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return ExitIPResult{Status: "failed", Error: "探测端口无效"}
	}
	transport := &http.Transport{Proxy: http.ProxyURL(proxyURL), DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 15 * time.Second}
	requestCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, e.service, nil)
	if err != nil {
		return ExitIPResult{Status: "failed", Error: "探测地址无效"}
	}
	req.Header.Set("User-Agent", "Mihomo-Manager")
	res, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ExitIPResult{Status: "cancelled", Error: "探测已取消"}
		}
		return ExitIPResult{Status: "failed", Error: "请求未通过该出口完成：" + trimError(err)}
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return ExitIPResult{Status: "failed", Error: fmt.Sprintf("查询服务返回 HTTP %d", res.StatusCode)}
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	if err != nil {
		return ExitIPResult{Status: "failed", Error: "查询服务响应读取失败"}
	}
	var parsed ip9Response
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return ExitIPResult{Status: "failed", Error: "查询服务返回了无法解析的内容"}
	}
	if parsed.Ret != 200 || strings.TrimSpace(parsed.Data.IP) == "" {
		return ExitIPResult{Status: "failed", Error: fmt.Sprintf("查询服务返回状态 %d", parsed.Ret)}
	}
	return ExitIPResult{
		Status: "success", IP: parsed.Data.IP, Country: parsed.Data.Country,
		Region: parsed.Data.Prov, City: parsed.Data.City, ISP: parsed.Data.ISP, ASN: parsed.Data.ASN,
	}
}

func trimError(err error) string {
	text := err.Error()
	if len(text) > 160 {
		text = text[:160] + "…"
	}
	return text
}
