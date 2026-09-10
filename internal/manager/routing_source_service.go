package manager

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"
)

type routingDraft struct {
	source   RoutingSource
	revision int64
	created  time.Time
}
type RoutingSourceService struct {
	manager    *Manager
	client     *http.Client
	ctx        context.Context
	cancel     context.CancelFunc
	wg         sync.WaitGroup
	mu         sync.Mutex
	drafts     map[string]routingDraft
	refreshing map[string]bool
}

func NewRoutingSourceService(m *Manager) *RoutingSourceService {
	ctx, cancel := context.WithCancel(context.Background())
	s := &RoutingSourceService{manager: m, client: &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{Proxy: nil}}, ctx: ctx, cancel: cancel, drafts: map[string]routingDraft{}, refreshing: map[string]bool{}}
	return s
}
func (s *RoutingSourceService) Start() { s.wg.Add(1); go s.loop() }
func (s *RoutingSourceService) Close() { s.cancel(); s.wg.Wait(); s.client.CloseIdleConnections() }

func sourceCandidate(state State, source RoutingSource) State {
	state.RoutingSources = append([]RoutingSource{}, state.RoutingSources...)
	found := false
	for i := range state.RoutingSources {
		if state.RoutingSources[i].ID == source.ID {
			state.RoutingSources[i] = source
			found = true
			break
		}
	}
	if !found {
		state.RoutingSources = append(state.RoutingSources, source)
	}
	state.ActiveRoutingSource = source.ID
	return state
}

func (s *RoutingSourceService) prepare(ctx context.Context, source RoutingSource) (RoutingSourcePreview, routingDraft) {
	report := RoutingSourcePreview{Errors: []string{}, Unresolved: []string{}, Mappings: map[string]string{}}
	fail := func(err error) (RoutingSourcePreview, routingDraft) {
		report.Errors = append(report.Errors, err.Error())
		return report, routingDraft{}
	}
	if err := validateRoutingSource(&source); err != nil {
		return fail(err)
	}
	state, err := s.manager.Snapshot(ctx)
	if err != nil {
		return fail(err)
	}
	if source.ID == "" {
		source.ID = newID()
		source.Version = 0
	} else {
		found := false
		for _, old := range state.RoutingSources {
			if old.ID == source.ID {
				found = true
				if source.Version != old.Version {
					return fail(errors.New("方案已修改，请刷新后重试"))
				}
				break
			}
		}
		if !found {
			return fail(errors.New("方案不存在"))
		}
	}
	state.ActiveRoutingSource = source.ID
	if err := s.manager.Store.loadRoutingScope(ctx, &state); err != nil {
		return fail(err)
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source.URL, nil)
	if err != nil {
		return fail(errors.New("订阅地址无效"))
	}
	req.Header.Set("User-Agent", "Clash.Meta/Mihomo-Manager")
	res, err := s.client.Do(req)
	if err != nil {
		return fail(errors.New("分流订阅下载失败，请检查地址和网络"))
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fail(fmt.Errorf("分流订阅返回 HTTP %d", res.StatusCode))
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, (32<<20)+1))
	if err != nil || len(raw) > 32<<20 {
		return fail(errors.New("分流订阅下载不完整或超过 32 MiB"))
	}
	doc, parsed, err := parseRoutingDocument(raw, source, state)
	report = parsed
	if err != nil {
		return fail(err)
	}
	source.Document = doc
	source.Groups = len(doc.Groups)
	source.Rules = len(doc.Rules)
	source.Providers = len(doc.Providers)
	source.Ignored = report.Ignored
	content, _ := json.Marshal(struct {
		Document  RoutingDocument
		Sources   []string
		ImportDNS bool
	}{doc, source.SourceIDs, source.ImportDNS})
	source.Digest = digestBytes(content)
	// Only the successfully parsed routing document participates in the digest;
	// quota counters, tokens in node definitions and ignored system fields do not.
	report.Changed = true
	for _, old := range state.RoutingSources {
		if old.ID == source.ID && old.Digest == source.Digest {
			report.Changed = false
		}
	}
	candidate := sourceCandidate(state, source)
	candidate.Routing.Enabled = true
	if !hasRuleListener(candidate) {
		port := 49999
		for {
			used := false
			for _, l := range candidate.Listeners {
				if l.Port == port {
					used = true
					port++
					break
				}
			}
			if !used {
				break
			}
		}
		candidate.Listeners = append(append([]Listener{}, candidate.Listeners...), Listener{ID: "source-preview", Mode: ListenerModeRule, Port: port, Enabled: true})
	}
	if _, err := compileRouting(candidate); err != nil {
		return fail(err)
	}
	if report.Changed {
		config, _, err := BuildConfig(candidate, s.manager.CoreAddr, s.manager.Secret)
		if err != nil {
			return fail(err)
		}
		file, err := os.CreateTemp(s.manager.Dir, "routing-preview-*.yaml")
		if err != nil {
			return fail(err)
		}
		path := file.Name()
		defer os.Remove(path)
		_, err = file.Write(config)
		closeErr := file.Close()
		if err != nil {
			return fail(err)
		}
		if closeErr != nil {
			return fail(closeErr)
		}
		if err = s.manager.Kernel.Validate(ctx, path); err != nil {
			return fail(errors.New("分流方案未通过 Mihomo 校验，请检查规则、分组引用或 DNS 格式"))
		}
	}
	source.CheckedAt = time.Now().UTC().Format(time.RFC3339)
	source.LastError = ""
	report.Source = source
	return report, routingDraft{source: source, revision: state.Revision, created: time.Now()}
}

func (s *RoutingSourceService) Preview(ctx context.Context, source RoutingSource) RoutingSourcePreview {
	report, draft := s.prepare(ctx, source)
	if len(report.Errors) > 0 {
		return report
	}
	token := newID()
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, old := range s.drafts {
		if time.Since(old.created) > 10*time.Minute {
			delete(s.drafts, key)
		}
	}
	if len(s.drafts) >= 20 {
		report.Errors = append(report.Errors, "预览数量过多，请稍后再试")
		return report
	}
	s.drafts[token] = draft
	report.Token = token
	return report
}

func (s *RoutingSourceService) SavePreview(ctx context.Context, token string) (bool, error) {
	s.mu.Lock()
	draft, ok := s.drafts[token]
	if ok {
		delete(s.drafts, token)
	}
	s.mu.Unlock()
	if !ok || time.Since(draft.created) > 10*time.Minute {
		return false, errors.New("预览已过期，请重新预览")
	}
	return s.accept(ctx, draft)
}

func (s *RoutingSourceService) accept(ctx context.Context, draft routingDraft) (bool, error) {
	m := s.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
	defer cancel()
	state, err := m.snapshotLocked(ctx)
	if err != nil {
		return false, err
	}
	if state.Revision != draft.revision {
		return false, errors.New("配置在下载期间发生变化，请重新预览或更新")
	}
	source := draft.source
	exists := false
	changed := true
	for _, old := range state.RoutingSources {
		if old.ID == source.ID {
			exists = true
			if old.Version != source.Version {
				return false, errors.New("方案已修改，请重新预览")
			}
			changed = old.Digest != source.Digest
			if !changed {
				source.UpdatedAt = old.UpdatedAt
			}
			break
		}
	}
	if !exists && source.Version != 0 {
		return false, errors.New("方案已删除")
	}
	source.Version++
	if changed {
		source.UpdatedAt = source.CheckedAt
	}
	active := state.ActiveRoutingSource == source.ID
	candidate := sourceCandidate(state, source)
	if active && changed {
		if state.Revision != state.AppliedRevision || state.LastError != "" {
			return false, errors.New("当前有待应用配置，请先应用后再更新方案")
		}
		if err = m.applyConfig(ctx, candidate); err != nil {
			return false, err
		}
	}
	if err = m.Store.saveRoutingSource(ctx, source, active && changed); err != nil {
		if active && changed {
			_ = m.applyConfig(ctx, state)
		}
		return false, err
	}
	if active && changed {
		fresh, err := m.snapshotLocked(ctx)
		if err != nil {
			return false, err
		}
		if err = m.Store.recordApply(ctx, fresh.Revision, nil); err != nil {
			return false, err
		}
	}
	return changed, nil
}

func (s *RoutingSourceService) Refresh(ctx context.Context, id string) (bool, error) {
	s.mu.Lock()
	if s.refreshing[id] {
		s.mu.Unlock()
		return false, errors.New("该方案正在更新")
	}
	s.refreshing[id] = true
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.refreshing, id); s.mu.Unlock() }()
	state, err := s.manager.Snapshot(ctx)
	if err != nil {
		return false, err
	}
	var source RoutingSource
	for _, v := range state.RoutingSources {
		if v.ID == id {
			source = v
		}
	}
	if source.ID == "" {
		return false, errors.New("方案不存在")
	}
	report, draft := s.prepare(ctx, source)
	if len(report.Errors) > 0 {
		err = errors.New(report.Errors[0])
	} else {
		var changed bool
		changed, err = s.accept(ctx, draft)
		if err == nil {
			return changed, nil
		}
	}
	s.manager.mu.Lock()
	_ = s.manager.Store.sourceStatus(context.WithoutCancel(ctx), source.ID, source.Version, err.Error())
	s.manager.mu.Unlock()
	return false, err
}

func (s *RoutingSourceService) Activate(ctx context.Context, id string) error {
	m := s.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
	defer cancel()
	state, err := m.snapshotLocked(ctx)
	if err != nil {
		return err
	}
	if state.ActiveRoutingSource == id {
		return nil
	}
	candidate := state
	candidate.ActiveRoutingSource = id
	if id != "" && candidate.activeRoutingSource() == nil {
		return errors.New("方案不存在")
	}
	if err = m.Store.loadRoutingScope(ctx, &candidate); err != nil {
		return err
	}
	if err = m.applyConfig(ctx, candidate); err != nil {
		return err
	}
	err = m.Store.mutate(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE routing_active SET source_id=? WHERE id=1`, id)
		return err
	})
	if err != nil {
		_ = m.applyConfig(ctx, state)
		return err
	}
	fresh, err := m.snapshotLocked(ctx)
	if err != nil {
		return err
	}
	return m.Store.recordApply(ctx, fresh.Revision, nil)
}

func (s *RoutingSourceService) loop() {
	defer s.wg.Done()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-timer.C:
			state, err := s.manager.Snapshot(s.ctx)
			if err == nil {
				for _, source := range state.RoutingSources {
					if !source.AutoUpdate {
						continue
					}
					checked, _ := time.Parse(time.RFC3339, source.CheckedAt)
					if time.Since(checked) >= time.Duration(source.Interval)*time.Second {
						_, _ = s.Refresh(s.ctx, source.ID)
					}
					if s.ctx.Err() != nil {
						return
					}
				}
			}
			timer.Reset(30 * time.Second)
		}
	}
}
