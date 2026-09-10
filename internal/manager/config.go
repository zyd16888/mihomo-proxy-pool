package manager

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

type Manager struct {
	Store                 *Store
	Kernel                Kernel
	Dir, CoreAddr, Secret string
	ReservedPorts         []int
	// ProbePort is the loopback port used to measure exit addresses. Zero
	// leaves the probe listener out of the generated configuration.
	ProbePort int
	mu        sync.Mutex
}

// snapshotLocked reads stored state and adds the settings that live in the
// process rather than the database.
func (m *Manager) snapshotLocked(ctx context.Context) (State, error) {
	state, err := m.Store.Snapshot(ctx)
	state.ProbePort = m.ProbePort
	return state, err
}

func ReadSecret(dir string) (string, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "core.secret")
	raw, err := os.ReadFile(path)
	if err == nil {
		if len(raw) < 32 {
			return "", errors.New("core.secret 已损坏，请从备份恢复")
		}
		return string(raw), nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	var data [32]byte
	if _, err = rand.Read(data[:]); err != nil {
		return "", err
	}
	secret := hex.EncodeToString(data[:])
	err = os.WriteFile(path, []byte(secret), 0600)
	return secret, err
}

func BuildConfig(state State, coreAddr, secret string) ([]byte, []int, error) {
	nodes := map[string]bool{}
	proxies := []map[string]any{}
	listeners := []map[string]any{}
	activeNodes := []string{}
	ports := []int{}
	for _, n := range state.Nodes {
		if !n.Enabled || !n.Available {
			continue
		}
		var cfg map[string]any
		if err := json.Unmarshal(n.Config, &cfg); err != nil {
			return nil, nil, err
		}
		cfg["name"] = "node-" + n.ID
		proxies = append(proxies, cfg)
		nodes[n.ID] = true
		activeNodes = append(activeNodes, "node-"+n.ID)
	}
	// A rule listener carries no proxy field, which is what sends its traffic
	// through the rule engine instead of a single pinned outbound.
	ruleReady := state.Routing.Enabled && state.ActiveRoutingSource != ""
	for _, l := range state.Listeners {
		if !l.Enabled {
			continue
		}
		entry := map[string]any{"name": "listener-" + l.ID, "type": "mixed", "listen": "0.0.0.0", "port": l.Port, "udp": true}
		if l.RuleMode() {
			if !ruleReady {
				continue
			}
		} else {
			if !nodes[l.NodeID] {
				continue
			}
			entry["proxy"] = "node-" + l.NodeID
		}
		listeners = append(listeners, entry)
		ports = append(ports, l.Port)
	}

	// The exit probe needs a way to reach a chosen node. A loopback-only
	// listener bound to its own selector provides one without disturbing any
	// listener that carries real traffic.
	var probeGroup map[string]any
	probePort := state.ProbePort
	if probePort > 0 && len(activeNodes) > 0 {
		members := []any{"DIRECT"}
		for _, node := range activeNodes {
			members = append(members, node)
		}
		probeGroup = map[string]any{"name": ExitProbeGroup, "type": "select", "proxies": members}
		listeners = append(listeners, map[string]any{
			"name": "exit-probe", "type": "mixed", "listen": "127.0.0.1", "port": probePort,
			"proxy": ExitProbeGroup, "udp": false,
		})
		ports = append(ports, probePort)
	}

	plan, err := compileRouting(state)
	if err != nil {
		return nil, nil, err
	}
	groups, rules, providers, dns := plan.Groups, plan.Rules, plan.Providers, plan.DNS
	if probeGroup != nil {
		groups = append(groups, probeGroup)
	}
	if len(rules) == 0 {
		// Without a rule listener nothing may reach the rule engine, so the
		// fallthrough stays a rejection rather than an accidental open proxy.
		rules = []string{"MATCH,REJECT"}
	}
	cfg := map[string]any{
		"allow-lan": true, "bind-address": "*", "mode": "rule", "log-level": "info", "ipv6": false,
		"external-controller": coreAddr, "secret": secret,
		"proxies": proxies, "listeners": listeners, "rules": rules,
		"profile": map[string]any{"store-selected": false},
	}
	if len(groups) > 0 {
		cfg["proxy-groups"] = groups
	}
	if len(providers) > 0 {
		cfg["rule-providers"] = providers
	}
	if dns != nil {
		cfg["dns"] = dns
	}
	raw, err := yaml.Marshal(cfg)
	return raw, ports, err
}

func portsFromConfig(raw []byte) []int {
	var cfg struct {
		Listeners []struct {
			Port int `yaml:"port"`
		} `yaml:"listeners"`
	}
	if yaml.Unmarshal(raw, &cfg) != nil {
		return nil
	}
	ports := []int{}
	for _, l := range cfg.Listeners {
		ports = append(ports, l.Port)
	}
	return ports
}
func difference(a, b []int) []int {
	have := map[int]bool{}
	for _, p := range b {
		have[p] = true
	}
	out := []int{}
	for _, p := range a {
		if !have[p] {
			out = append(out, p)
		}
	}
	return out
}

func atomicWrite(path string, raw []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".config-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(raw)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(name, path)
}

// Bootstrap always starts the last acknowledged configuration, including after an
// interruption between candidate reload and database acknowledgement. Legacy local
// routing is retired before startup when no subscription is active.
func (m *Manager) Bootstrap(ctx context.Context) error {
	good := filepath.Join(m.Dir, "last-good.yaml")
	raw, err := os.ReadFile(good)
	if errors.Is(err, os.ErrNotExist) {
		raw, _, err = BuildConfig(State{}, m.CoreAddr, m.Secret)
	}
	if err != nil {
		return err
	}
	var cfg map[string]any
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return fmt.Errorf("上一版运行配置损坏: %w", err)
	}
	if cfg == nil {
		return errors.New("上一版运行配置为空")
	}
	state, err := m.Store.Snapshot(ctx)
	if err != nil {
		return err
	}
	retired := false
	if state.ActiveRoutingSource == "" {
		// Keep acknowledged fixed listeners and their node definitions intact.
		listeners, _ := cfg["listeners"].([]any)
		kept := []any{}
		for _, item := range listeners {
			listener, ok := item.(map[string]any)
			if ok && stringOf(listener["proxy"]) == "" {
				retired = true
				continue
			}
			kept = append(kept, item)
		}
		if retired {
			cfg["listeners"] = kept
			groups := []any{}
			if existing, ok := cfg["proxy-groups"].([]any); ok {
				for _, item := range existing {
					if group, ok := item.(map[string]any); ok && stringOf(group["name"]) == ExitProbeGroup {
						groups = append(groups, item)
					}
				}
			}
			cfg["proxy-groups"] = groups
			cfg["rules"] = []string{"MATCH,REJECT"}
			delete(cfg, "rule-providers")
			delete(cfg, "dns")
			raw, err = yaml.Marshal(cfg)
			if err != nil {
				return err
			}
		}
	}
	for _, p := range portsFromConfig(raw) {
		for _, reserved := range m.ReservedPorts {
			if p == reserved {
				return fmt.Errorf("管理端口 %d 与已有监听冲突，请调整管理端口", p)
			}
		}
	}
	cfg["external-controller"] = m.CoreAddr
	cfg["secret"] = m.Secret
	raw, err = yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	if retired {
		// A later failed reload must not restore the obsolete local scheme either.
		if err := atomicWrite(good, raw); err != nil {
			return err
		}
	}
	return atomicWrite(filepath.Join(m.Dir, "config.yaml"), raw)
}

func (m *Manager) Snapshot(ctx context.Context) (State, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.snapshotLocked(ctx)
}

func (m *Manager) Change(ctx context.Context, fn func() error) (ApplyResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := fn(); err != nil {
		return ApplyResult{}, err
	}
	// A disconnected browser must not interrupt recovery after data was committed.
	applyCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 45*time.Second)
	defer cancel()
	return m.applyLocked(applyCtx), nil
}
func (m *Manager) Apply(ctx context.Context) ApplyResult {
	m.mu.Lock()
	defer m.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 45*time.Second)
	defer cancel()
	return m.applyLocked(ctx)
}

func (m *Manager) applyLocked(ctx context.Context) ApplyResult {
	state, err := m.snapshotLocked(ctx)
	result := ApplyResult{Revision: state.Revision}
	if err == nil {
		err = m.applyConfig(ctx, state)
	}
	if err != nil {
		result.Error = err.Error()
	} else {
		result.Applied = true
	}
	if recordErr := m.Store.recordApply(context.WithoutCancel(ctx), state.Revision, err); recordErr != nil {
		result.Applied = false
		result.Error = fmt.Sprintf("保存应用状态失败: %v", recordErr)
	}
	return result
}

func (m *Manager) applyConfig(ctx context.Context, state State) error {
	raw, ports, err := BuildConfig(state, m.CoreAddr, m.Secret)
	if err != nil {
		return err
	}
	path := filepath.Join(m.Dir, "config.yaml")
	old, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	oldPorts := portsFromConfig(old)
	for _, p := range ports {
		for _, reserved := range m.ReservedPorts {
			if p == reserved {
				return fmt.Errorf("端口 %d 是管理服务保留端口", p)
			}
		}
	}
	for _, p := range difference(ports, oldPorts) {
		// Windows can permit wildcard and specific-address binds to coexist.
		// An already reachable loopback socket must also count as a conflict.
		probe, probeErr := net.DialTimeout("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(p)), 200*time.Millisecond)
		if probeErr == nil {
			probe.Close()
			return fmt.Errorf("端口 %d 被占用", p)
		}
		conn, err := net.Listen("tcp4", ":"+strconv.Itoa(p))
		if err != nil {
			return fmt.Errorf("端口 %d 被占用: %w", p, err)
		}
		conn.Close()
		udp, err := net.ListenPacket("udp4", ":"+strconv.Itoa(p))
		if err != nil {
			return fmt.Errorf("UDP 端口 %d 被占用: %w", p, err)
		}
		udp.Close()
	}
	candidate := filepath.Join(m.Dir, "candidate.yaml")
	if err = atomicWrite(candidate, raw); err != nil {
		return err
	}
	defer os.Remove(candidate)
	if err = m.Kernel.Validate(ctx, candidate); err != nil {
		return err
	}
	if _, err = m.Kernel.Version(ctx); err != nil {
		return fmt.Errorf("内核不可用: %w", err)
	}
	if err = atomicWrite(path, raw); err != nil {
		return err
	}
	var candidateConfig struct {
		Groups []map[string]any `yaml:"proxy-groups"`
	}
	if err := yaml.Unmarshal(raw, &candidateConfig); err != nil {
		return err
	}
	applyErr := m.Kernel.Reload(ctx, path)
	if applyErr == nil {
		applyErr = m.restoreSelections(ctx, candidateConfig.Groups)
	}
	if applyErr == nil {
		applyErr = m.Kernel.Verify(ctx, ports, difference(oldPorts, ports))
	}
	if applyErr == nil {
		applyErr = atomicWrite(filepath.Join(m.Dir, "last-good.yaml"), raw)
	}
	if applyErr == nil {
		return nil
	}
	recoveryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	recoveryErr := atomicWrite(path, old)
	if recoveryErr == nil {
		recoveryErr = m.Kernel.Reload(recoveryCtx, path)
		if recoveryErr == nil {
			var previous struct {
				Groups []map[string]any `yaml:"proxy-groups"`
			}
			recoveryErr = yaml.Unmarshal(old, &previous)
			if recoveryErr == nil {
				recoveryErr = m.restoreSelections(recoveryCtx, previous.Groups)
			}
		}
	}
	if recoveryErr == nil {
		recoveryErr = m.Kernel.Verify(recoveryCtx, oldPorts, difference(ports, oldPorts))
	}
	if recoveryErr != nil {
		return fmt.Errorf("应用失败: %v；恢复失败: %v，请检查内核运行状态", applyErr, recoveryErr)
	}
	return fmt.Errorf("应用失败，已恢复上一版运行配置: %w", applyErr)
}
