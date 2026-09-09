package manager

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"strings"
	"sync"
	"time"
)

const checkConcurrency = 4

type NodeCheckResult struct {
	Status    string `json:"status"`
	DelayMS   *int   `json:"delayMs,omitempty"`
	CheckedAt string `json:"checkedAt,omitempty"`
	Error     string `json:"error,omitempty"`
	InFlight  bool   `json:"inFlight"`
}

type CheckBatch struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	Total     int    `json:"total"`
	Completed int    `json:"completed"`
	Succeeded int    `json:"succeeded"`
	Failed    int    `json:"failed"`
	Skipped   int    `json:"skipped"`
	Cancelled int    `json:"cancelled"`
}

type NodeCheckSnapshot struct {
	Results map[string]NodeCheckResult `json:"results"`
	Batch   *CheckBatch                `json:"batch,omitempty"`
}

type nodeCheckTask struct {
	node     Node
	result   NodeCheckResult
	ctx      context.Context
	cancel   context.CancelFunc
	done     chan struct{}
	finished bool
}

// NodeChecks is deliberately process-local. It never writes configuration or SQL.
type NodeChecks struct {
	manager     *Manager
	ctx         context.Context
	cancel      context.CancelFunc
	mu          sync.Mutex
	closed      bool
	wg          sync.WaitGroup
	slots       chan struct{}
	tasks       map[string]*nodeCheckTask
	batch       *CheckBatch
	batchCancel context.CancelFunc
}

func NewNodeChecks(m *Manager) *NodeChecks {
	ctx, cancel := context.WithCancel(context.Background())
	return &NodeChecks{manager: m, ctx: ctx, cancel: cancel, slots: make(chan struct{}, checkConcurrency), tasks: map[string]*nodeCheckTask{}}
}

func (c *NodeChecks) Close() { c.mu.Lock(); c.closed = true; c.cancel(); c.mu.Unlock(); c.wg.Wait() }

func (c *NodeChecks) prepare(ctx context.Context) (State, error) {
	state, err := c.manager.Snapshot(ctx)
	if err != nil {
		return state, err
	}
	if state.Revision != state.AppliedRevision || state.LastError != "" {
		return state, errors.New("请先成功应用当前配置，再检测节点")
	}
	probe, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if _, err := c.manager.Kernel.Version(probe); err != nil {
		return state, errors.New("内核未就绪，暂时无法检测")
	}
	return state, nil
}

func nodeForCheck(state State, id string) (Node, bool) {
	for _, n := range state.Nodes {
		if n.ID == id {
			return n, n.Enabled && n.Available
		}
	}
	return Node{}, false
}
func sameCheckNode(a, b Node) bool {
	return a.ID == b.ID && a.Enabled == b.Enabled && a.Available == b.Available && string(a.Config) == string(b.Config)
}

// Caller holds mu. An already queued or running task is shared by all callers.
func (c *NodeChecks) claim(node Node, parent context.Context) (*nodeCheckTask, bool) {
	if task := c.tasks[node.ID]; task != nil && !task.finished {
		if !sameCheckNode(task.node, node) {
			task.cancel()
		}
		return task, false
	}
	ctx, cancel := context.WithCancel(parent)
	task := &nodeCheckTask{node: node, ctx: ctx, cancel: cancel, done: make(chan struct{}), result: NodeCheckResult{Status: "queued", InFlight: true}}
	c.tasks[node.ID] = task
	return task, true
}

func (c *NodeChecks) StartSingle(ctx context.Context, id string) error {
	state, err := c.prepare(ctx)
	if err != nil {
		return err
	}
	node, ok := nodeForCheck(state, id)
	if !ok {
		return errors.New("节点不存在、已停用或已失效")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return errors.New("检测服务正在停止")
	}
	task, owned := c.claim(node, c.ctx)
	if owned {
		c.wg.Add(1)
		go func() { defer c.wg.Done(); c.execute(task) }()
	}
	return nil
}

func (c *NodeChecks) StartBatch(ctx context.Context, ids []string) error {
	state, err := c.prepare(ctx)
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return errors.New("当前筛选结果中没有可检测节点")
	}
	nodes := []Node{}
	seen := map[string]bool{}
	available := make(map[string]Node, len(state.Nodes))
	for _, node := range state.Nodes {
		if node.Enabled && node.Available {
			available[node.ID] = node
		}
	}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		node, ok := available[id]
		if ok {
			nodes = append(nodes, node)
		}
	}
	if len(nodes) == 0 {
		return errors.New("选中的节点均不存在、已停用或已失效，请刷新列表")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return errors.New("检测服务正在停止")
	}
	if c.batch != nil && (c.batch.Status == "running" || c.batch.Status == "stopping") {
		return nil
	}
	batchCtx, cancel := context.WithCancel(c.ctx)
	c.batchCancel = cancel
	batch := &CheckBatch{ID: newID(), Status: "running", Total: len(nodes)}
	c.batch = batch
	type workItem struct {
		task  *nodeCheckTask
		owned bool
	}
	queue := make(chan workItem, len(nodes))
	for _, node := range nodes {
		task, owned := c.claim(node, batchCtx)
		queue <- workItem{task, owned}
	}
	close(queue)
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		defer cancel()
		var workers sync.WaitGroup
		for i := 0; i < checkConcurrency; i++ {
			workers.Add(1)
			go func() {
				defer workers.Done()
				for item := range queue {
					if item.owned {
						c.execute(item.task)
					}
					status := "cancelled"
					select {
					case <-item.task.done:
						c.mu.Lock()
						status = item.task.result.Status
						c.mu.Unlock()
					case <-batchCtx.Done():
					}
					c.mu.Lock()
					batch.Completed++
					switch status {
					case "success":
						batch.Succeeded++
					case "failed", "timeout":
						batch.Failed++
					case "stale", "skipped":
						batch.Skipped++
					default:
						batch.Cancelled++
					}
					c.mu.Unlock()
				}
			}()
		}
		workers.Wait()
		c.mu.Lock()
		if batchCtx.Err() != nil {
			batch.Status = "stopped"
		} else {
			batch.Status = "completed"
		}
		c.mu.Unlock()
	}()
	return nil
}

func (c *NodeChecks) StopBatch(id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.batch == nil || c.batch.ID != id {
		return errors.New("批量任务不存在或已被新任务替换")
	}
	if c.batch.Status == "running" {
		c.batch.Status = "stopping"
		c.batchCancel()
	}
	return nil
}

func (c *NodeChecks) execute(task *nodeCheckTask) {
	result := NodeCheckResult{Status: "cancelled"}
	defer func() {
		task.cancel()
		result.CheckedAt = time.Now().UTC().Format(time.RFC3339)
		c.mu.Lock()
		task.result = result
		task.finished = true
		close(task.done)
		c.mu.Unlock()
	}()
	if task.ctx.Err() != nil {
		return
	}
	select {
	case c.slots <- struct{}{}:
		defer func() { <-c.slots }()
	case <-task.ctx.Done():
		return
	}
	if task.ctx.Err() != nil {
		return
	}
	current, applied, err := c.currentNode(task.ctx, task.node.ID)
	if task.ctx.Err() != nil {
		return
	}
	if errors.Is(err, sql.ErrNoRows) {
		result.Status = "stale"
		result.Error = "节点已移除"
		return
	}
	if err != nil {
		result.Status = "failed"
		result.Error = "读取节点状态失败"
		return
	}
	if !sameCheckNode(current, task.node) {
		result.Status = "stale"
		result.Error = "节点配置已变化，请重新检测"
		return
	}
	if !applied {
		result.Status = "skipped"
		result.Error = "配置尚未成功应用"
		return
	}
	c.mu.Lock()
	task.result.Status = "running"
	c.mu.Unlock()
	probe, cancel := context.WithTimeout(task.ctx, 7*time.Second)
	delay, checkErr := c.manager.Kernel.Delay(probe, task.node.ID)
	cancel()
	if task.ctx.Err() != nil {
		return
	}
	// Never attach an old probe to a replaced or removed node.
	current, applied, err = c.currentNode(task.ctx, task.node.ID)
	if task.ctx.Err() != nil {
		return
	}
	if err != nil || !sameCheckNode(current, task.node) {
		result.Status = "stale"
		result.Error = "节点配置已变化，请重新检测"
		return
	}
	if !applied {
		result.Status = "stale"
		result.Error = "检测期间配置发生变更，请应用后重试"
		return
	}
	if checkErr != nil {
		result.Status = "failed"
		result.Error = checkErr.Error()
		var networkError net.Error
		message := strings.ToLower(checkErr.Error())
		if errors.Is(checkErr, context.DeadlineExceeded) || (errors.As(checkErr, &networkError) && networkError.Timeout()) || strings.Contains(message, "timeout") || strings.Contains(message, "timed out") {
			result.Status = "timeout"
			result.Error = "检测超时"
		}
		return
	}
	result.Status = "success"
	result.DelayMS = &delay
}

// Point lookup avoids reading the complete node pool twice for every probe.
// Serialize this short read with configuration application, never the network call.
func (c *NodeChecks) currentNode(ctx context.Context, id string) (Node, bool, error) {
	c.manager.mu.Lock()
	defer c.manager.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Node{}, false, err
	}
	// sqlite v1.38.2 can lose the rows handle if cancellation arrives while a
	// QueryContext result is being returned. Finish this short, indexed local
	// read; execute checks cancellation immediately afterwards. Network probes
	// remain cancellable, and SQLite's busy_timeout still bounds lock waits.
	return c.manager.Store.nodeForCheck(context.WithoutCancel(ctx), id)
}

func (c *NodeChecks) Snapshot(state State) NodeCheckSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := NodeCheckSnapshot{Results: map[string]NodeCheckResult{}}
	nodes := map[string]Node{}
	for _, n := range state.Nodes {
		nodes[n.ID] = n
	}
	for id, task := range c.tasks {
		n, exists := nodes[id]
		if !exists {
			task.cancel()
			delete(c.tasks, id)
			continue
		}
		result := task.result
		if !sameCheckNode(n, task.node) {
			task.cancel()
			result.Status = "stale"
			result.DelayMS = nil
			result.Error = "节点配置已变化，请重新检测"
			task.result = result
		}
		result.InFlight = !task.finished
		out.Results[id] = result
	}
	if c.batch != nil {
		batch := *c.batch
		out.Batch = &batch
	}
	return out
}
