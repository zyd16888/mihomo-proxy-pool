package manager

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"mihomo-proxy/internal/importer"
	_ "modernc.org/sqlite"
)

type Store struct{ db *sql.DB }

func OpenStore(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // PRAGMA and foreign keys apply to the single writer connection.
	for _, stmt := range []string{
		`PRAGMA foreign_keys=ON`, `PRAGMA journal_mode=WAL`, `PRAGMA busy_timeout=5000`,
		`CREATE TABLE IF NOT EXISTS subscriptions(id TEXT PRIMARY KEY,name TEXT NOT NULL,url TEXT NOT NULL UNIQUE,updated_at TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE IF NOT EXISTS subscription_usage(subscription_id TEXT PRIMARY KEY REFERENCES subscriptions(id) ON DELETE CASCADE,upload_bytes INTEGER,download_bytes INTEGER,total_bytes INTEGER,expire INTEGER,updated_at TEXT NOT NULL DEFAULT '',checked_at TEXT NOT NULL,status TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS nodes(id TEXT PRIMARY KEY,name TEXT NOT NULL,source_id TEXT NOT NULL,identity TEXT NOT NULL,protocol TEXT NOT NULL,server TEXT NOT NULL,port INTEGER NOT NULL,enabled INTEGER NOT NULL DEFAULT 1,available INTEGER NOT NULL DEFAULT 1,config TEXT NOT NULL,UNIQUE(source_id,name))`,
		listenersSchema,
		`CREATE TABLE IF NOT EXISTS routing_sources(id TEXT PRIMARY KEY,config TEXT NOT NULL,document TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS routing_active(id INTEGER PRIMARY KEY CHECK(id=1),source_id TEXT NOT NULL DEFAULT '')`,
		`INSERT OR IGNORE INTO routing_active(id) VALUES(1)`,
		`CREATE TABLE IF NOT EXISTS category_edits(scope TEXT NOT NULL,name TEXT NOT NULL,data TEXT NOT NULL,PRIMARY KEY(scope,name))`,
		`CREATE TABLE IF NOT EXISTS category_rules(scope TEXT NOT NULL,id TEXT NOT NULL,data TEXT NOT NULL,PRIMARY KEY(scope,id))`,
		`CREATE TABLE IF NOT EXISTS routing_blocked_rules(scope TEXT NOT NULL,id TEXT NOT NULL,rule TEXT NOT NULL,PRIMARY KEY(scope,id))`,
		`CREATE TABLE IF NOT EXISTS routing_source_selections(scope TEXT NOT NULL,name TEXT NOT NULL,member TEXT NOT NULL,PRIMARY KEY(scope,name))`,
		`CREATE TABLE IF NOT EXISTS proxy_groups(id TEXT PRIMARY KEY,name TEXT NOT NULL UNIQUE,kind TEXT NOT NULL,node_ids TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS proxy_selections(name TEXT PRIMARY KEY,member TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS rule_sets(id TEXT PRIMARY KEY,name TEXT NOT NULL UNIQUE,policy TEXT NOT NULL,behavior TEXT NOT NULL,format TEXT NOT NULL,url TEXT NOT NULL,interval INTEGER NOT NULL,no_resolve INTEGER NOT NULL DEFAULT 0,enabled INTEGER NOT NULL DEFAULT 1,position INTEGER NOT NULL DEFAULT 500,builtin INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE IF NOT EXISTS routing(id INTEGER PRIMARY KEY CHECK(id=1),enabled INTEGER NOT NULL DEFAULT 1,default_policy TEXT NOT NULL,merge_sub_rules INTEGER NOT NULL DEFAULT 1,sub_rule_position INTEGER NOT NULL DEFAULT 500,allow_geo_rules INTEGER NOT NULL DEFAULT 0,rule_set_proxy TEXT NOT NULL DEFAULT 'DIRECT',dns_enabled INTEGER NOT NULL DEFAULT 1,dns_domestic TEXT NOT NULL DEFAULT '',dns_foreign TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE IF NOT EXISTS subscription_profiles(subscription_id TEXT PRIMARY KEY REFERENCES subscriptions(id) ON DELETE CASCADE,groups TEXT NOT NULL,rules TEXT NOT NULL,providers TEXT NOT NULL,group_count INTEGER NOT NULL DEFAULT 0,rule_count INTEGER NOT NULL DEFAULT 0,provider_count INTEGER NOT NULL DEFAULT 0,updated_at TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS state(id INTEGER PRIMARY KEY CHECK(id=1),revision INTEGER NOT NULL DEFAULT 0,applied_revision INTEGER NOT NULL DEFAULT -1,last_error TEXT NOT NULL DEFAULT '',applied_at TEXT NOT NULL DEFAULT '')`,
		`INSERT OR IGNORE INTO state(id) VALUES(1)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			db.Close()
			return nil, err
		}
	}
	store := &Store{db}
	if err := store.migrateListeners(); err != nil {
		db.Close()
		return nil, err
	}
	if err := store.seedRouting(); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

// node_id is nullable because a rule listener resolves its outbound per
// request instead of binding one node.
const listenersSchema = `CREATE TABLE IF NOT EXISTS listeners(
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	port INTEGER NOT NULL UNIQUE CHECK(port BETWEEN 1 AND 65535),
	mode TEXT NOT NULL DEFAULT 'node' CHECK(mode IN ('node','rule')),
	node_id TEXT REFERENCES nodes(id) ON DELETE RESTRICT,
	enabled INTEGER NOT NULL DEFAULT 1,
	CHECK(mode='rule' OR node_id IS NOT NULL))`

// Databases written before rule listeners existed have a NOT NULL node_id and
// no mode column, neither of which SQLite can alter in place.
func (s *Store) migrateListeners() error {
	rows, err := s.db.Query(`PRAGMA table_info(listeners)`)
	if err != nil {
		return err
	}
	hasMode := false
	for rows.Next() {
		var index int
		var name, kind string
		var notNull, primary int
		var fallback any
		if err := rows.Scan(&index, &name, &kind, &notNull, &fallback, &primary); err != nil {
			rows.Close()
			return err
		}
		if name == "mode" {
			hasMode = true
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil || hasMode {
		return err
	}
	if _, err := s.db.Exec(`PRAGMA foreign_keys=OFF`); err != nil {
		return err
	}
	defer s.db.Exec(`PRAGMA foreign_keys=ON`)
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, stmt := range []string{
		strings.Replace(listenersSchema, "IF NOT EXISTS listeners", "listeners_migrated", 1),
		`INSERT INTO listeners_migrated(id,name,port,mode,node_id,enabled) SELECT id,name,port,'node',node_id,enabled FROM listeners`,
		`DROP TABLE listeners`,
		`ALTER TABLE listeners_migrated RENAME TO listeners`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Rule sets are seeded only when the routing row is first created, so a user
// who removes a default set does not get it back on the next restart.
func (s *Store) seedRouting() error {
	defaults := DefaultRouting()
	result, err := s.db.Exec(`INSERT OR IGNORE INTO routing(id,enabled,default_policy,merge_sub_rules,sub_rule_position,allow_geo_rules,rule_set_proxy,dns_enabled,dns_domestic,dns_foreign)
		VALUES(1,1,?,1,?,0,?,1,?,?)`,
		defaults.DefaultPolicy, defaults.SubRulePosition, defaults.RuleSetProxy,
		strings.Join(defaults.DNSDomestic, "\n"), strings.Join(defaults.DNSForeign, "\n"))
	if err != nil {
		return err
	}
	created, err := result.RowsAffected()
	if err != nil || created == 0 {
		return err
	}
	for _, set := range DefaultRuleSets() {
		if set.Interval == 0 {
			set.Interval = 86400
		}
		if _, err := s.db.Exec(`INSERT INTO rule_sets(id,name,policy,behavior,format,url,interval,no_resolve,enabled,position,builtin)
			VALUES(?,?,?,?,?,?,?,?,1,?,1)`,
			newID(), set.Name, set.Policy, set.Behavior, set.Format, set.URL, set.Interval, set.NoResolve, set.Position); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) nodeForCheck(ctx context.Context, id string) (Node, bool, error) {
	var node Node
	var raw string
	var applied bool
	err := s.db.QueryRowContext(ctx, `SELECT n.id,n.enabled,n.available,n.config,
		s.revision=s.applied_revision AND s.last_error=''
		FROM nodes n CROSS JOIN state s WHERE n.id=? AND s.id=1`, id).
		Scan(&node.ID, &node.Enabled, &node.Available, &raw, &applied)
	node.Config = json.RawMessage(raw)
	return node, applied, err
}

func newID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

func (s *Store) Snapshot(ctx context.Context) (State, error) {
	state := State{Nodes: []Node{}, Listeners: []Listener{}, Subscriptions: []Subscription{}}
	rows, err := s.db.QueryContext(ctx, `SELECT id,name,source_id,protocol,server,port,enabled,available,config FROM nodes ORDER BY name,id`)
	if err != nil {
		return state, err
	}
	for rows.Next() {
		var n Node
		var raw string
		if err = rows.Scan(&n.ID, &n.Name, &n.SourceID, &n.Protocol, &n.Server, &n.Port, &n.Enabled, &n.Available, &raw); err != nil {
			rows.Close()
			return state, err
		}
		n.Config = json.RawMessage(raw)
		state.Nodes = append(state.Nodes, n)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return state, err
	}
	rows, err = s.db.QueryContext(ctx, `SELECT id,name,port,mode,COALESCE(node_id,''),enabled FROM listeners ORDER BY port`)
	if err != nil {
		return state, err
	}
	for rows.Next() {
		var l Listener
		if err = rows.Scan(&l.ID, &l.Name, &l.Port, &l.Mode, &l.NodeID, &l.Enabled); err != nil {
			rows.Close()
			return state, err
		}
		state.Listeners = append(state.Listeners, l)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return state, err
	}
	rows, err = s.db.QueryContext(ctx, `SELECT s.id,s.name,s.url,s.updated_at,u.subscription_id,
		u.upload_bytes,u.download_bytes,u.total_bytes,u.expire,COALESCE(u.updated_at,''),COALESCE(u.checked_at,''),COALESCE(u.status,''),
		p.subscription_id,COALESCE(p.group_count,0),COALESCE(p.rule_count,0),COALESCE(p.provider_count,0),COALESCE(p.updated_at,'')
		FROM subscriptions s
		LEFT JOIN subscription_usage u ON u.subscription_id=s.id
		LEFT JOIN subscription_profiles p ON p.subscription_id=s.id ORDER BY s.name`)
	if err != nil {
		return state, err
	}
	for rows.Next() {
		var sub Subscription
		var usageID, profileID *string
		var usage SubscriptionUsage
		var profile ProfileSummary
		if err = rows.Scan(&sub.ID, &sub.Name, &sub.URL, &sub.UpdatedAt, &usageID, &usage.UploadBytes, &usage.DownloadBytes, &usage.TotalBytes, &usage.Expire, &usage.UpdatedAt, &usage.CheckedAt, &usage.Status,
			&profileID, &profile.Groups, &profile.Rules, &profile.Providers, &profile.UpdatedAt); err != nil {
			rows.Close()
			return state, err
		}
		if usageID != nil {
			usage.calculate()
			sub.Usage = &usage
		}
		if profileID != nil {
			sub.Profile = &profile
		}
		state.Subscriptions = append(state.Subscriptions, sub)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return state, err
	}
	if state.RuleSets, err = s.ruleSets(ctx); err != nil {
		return state, err
	}
	if state.Routing, err = s.routing(ctx); err != nil {
		return state, err
	}
	if state.Profiles, err = s.subscriptionProfiles(ctx); err != nil {
		return state, err
	}
	if state.ProxyGroups, state.Selections, err = s.proxyGroups(ctx); err != nil {
		return state, err
	}
	if err = s.loadRoutingSources(ctx, &state); err != nil {
		return state, err
	}
	err = s.db.QueryRowContext(ctx, `SELECT revision,applied_revision,last_error,applied_at FROM state WHERE id=1`).Scan(&state.Revision, &state.AppliedRevision, &state.LastError, &state.AppliedAt)
	return state, err
}

func (s *Store) mutate(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = fn(tx); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE state SET revision=revision+1 WHERE id=1`); err != nil {
		return err
	}
	return tx.Commit()
}

func changed(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return errors.New("记录不存在")
	}
	return nil
}

func (s *Store) SaveListener(ctx context.Context, l Listener) error {
	l.Name = strings.TrimSpace(l.Name)
	if l.Name == "" || len(l.Name) > 120 {
		return errors.New("请输入不超过 120 字符的监听名称")
	}
	if l.Port < 1 || l.Port > 65535 {
		return errors.New("监听端口范围为 1–65535")
	}
	if l.Mode == "" {
		l.Mode = ListenerModeNode
	}
	if l.Mode != ListenerModeNode && l.Mode != ListenerModeRule {
		return errors.New("监听模式必须是固定节点或规则分流")
	}
	return s.mutate(ctx, func(tx *sql.Tx) error {
		var node any
		if l.RuleMode() {
			l.NodeID = ""
			var routingEnabled bool
			if err := tx.QueryRowContext(ctx, `SELECT enabled FROM routing WHERE id=1`).Scan(&routingEnabled); err != nil {
				return err
			}
			if l.Enabled && !routingEnabled {
				return errors.New("规则分流已关闭，请先在分流设置中启用")
			}
		} else {
			var enabled, available bool
			if err := tx.QueryRowContext(ctx, `SELECT enabled,available FROM nodes WHERE id=?`, l.NodeID).Scan(&enabled, &available); err != nil {
				return errors.New("绑定节点不存在")
			}
			if l.Enabled && (!enabled || !available) {
				return errors.New("绑定节点已停用或不在当前订阅中，请选择可用节点")
			}
			node = l.NodeID
		}
		var other int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM listeners WHERE port=? AND id<>?`, l.Port, l.ID).Scan(&other); err != nil {
			return err
		}
		if other > 0 {
			return errors.New("该端口已被另一个监听使用")
		}
		if l.ID == "" {
			_, err := tx.ExecContext(ctx, `INSERT INTO listeners(id,name,port,mode,node_id,enabled) VALUES(?,?,?,?,?,?)`, newID(), l.Name, l.Port, l.Mode, node, l.Enabled)
			return err
		}
		return changed(tx.ExecContext(ctx, `UPDATE listeners SET name=?,port=?,mode=?,node_id=?,enabled=? WHERE id=?`, l.Name, l.Port, l.Mode, node, l.Enabled, l.ID))
	})
}

func (s *Store) DeleteListener(ctx context.Context, id string) error {
	return s.mutate(ctx, func(tx *sql.Tx) error { return changed(tx.ExecContext(ctx, `DELETE FROM listeners WHERE id=?`, id)) })
}

func (s *Store) SetNodeEnabled(ctx context.Context, id string, enabled bool) error {
	return s.mutate(ctx, func(tx *sql.Tx) error {
		return changed(tx.ExecContext(ctx, `UPDATE nodes SET enabled=? WHERE id=?`, enabled, id))
	})
}

func (s *Store) DeleteNode(ctx context.Context, id string) error {
	return s.mutate(ctx, func(tx *sql.Tx) error {
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM listeners WHERE node_id=?`, id).Scan(&count); err != nil {
			return err
		}
		if count > 0 {
			return errors.New("节点仍被监听引用，请先删除监听或更换绑定")
		}
		return changed(tx.ExecContext(ctx, `DELETE FROM nodes WHERE id=?`, id))
	})
}

func (s *Store) SaveSubscription(ctx context.Context, sub Subscription) error {
	if strings.TrimSpace(sub.Name) == "" || strings.TrimSpace(sub.URL) == "" {
		return errors.New("订阅名称和地址不能为空")
	}
	return s.mutate(ctx, func(tx *sql.Tx) error {
		if sub.ID == "" {
			_, err := tx.ExecContext(ctx, `INSERT INTO subscriptions(id,name,url) VALUES(?,?,?)`, newID(), strings.TrimSpace(sub.Name), sub.URL)
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM subscription_usage WHERE subscription_id=? AND EXISTS(SELECT 1 FROM subscriptions WHERE id=? AND url<>?)`, sub.ID, sub.ID, sub.URL); err != nil {
			return err
		}
		return changed(tx.ExecContext(ctx, `UPDATE subscriptions SET name=?,url=? WHERE id=?`, strings.TrimSpace(sub.Name), sub.URL, sub.ID))
	})
}

func (s *Store) DeleteSubscription(ctx context.Context, id string) error {
	return s.mutate(ctx, func(tx *sql.Tx) error {
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM listeners l JOIN nodes n ON l.node_id=n.id WHERE n.source_id=?`, id).Scan(&count); err != nil {
			return err
		}
		if count > 0 {
			return errors.New("订阅节点仍被监听引用，请先处理绑定")
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM nodes WHERE source_id=?`, id); err != nil {
			return err
		}
		return changed(tx.ExecContext(ctx, `DELETE FROM subscriptions WHERE id=?`, id))
	})
}

// Source + name is the stable subscription key; identical proxy identity also
// preserves bindings across a rename. Removed entries remain as unavailable tombstones.
func (s *Store) Import(ctx context.Context, source string, items []importer.Proxy) error {
	return s.importInto(ctx, source, items, nil)
}

// ImportSubscription stores nodes and routing rules from one sync in a single
// transaction, so the two never describe different versions of a subscription.
func (s *Store) ImportSubscription(ctx context.Context, source string, items []importer.Proxy, profile importer.Profile) error {
	return s.importInto(ctx, source, items, &profile)
}

func (s *Store) importInto(ctx context.Context, source string, items []importer.Proxy, profile *importer.Profile) error {
	if len(items) == 0 {
		return errors.New("订阅未包含有效节点，保留原数据")
	}
	return s.mutate(ctx, func(tx *sql.Tx) error {
		incoming := map[string]bool{}
		for _, item := range items {
			if incoming[item.Node.Name] {
				return errors.New("节点名称重复")
			}
			incoming[item.Node.Name] = true
		}
		type existingNode struct{ id, name, identity string }
		byName := map[string]string{}
		renameCandidates := map[string][]string{}
		rows, err := tx.QueryContext(ctx, `SELECT id,name,identity FROM nodes WHERE source_id=?`, source)
		if err != nil {
			return err
		}
		for rows.Next() {
			var n existingNode
			if err = rows.Scan(&n.id, &n.name, &n.identity); err != nil {
				rows.Close()
				return err
			}
			byName[n.name] = n.id
			if !incoming[n.name] {
				renameCandidates[n.identity] = append(renameCandidates[n.identity], n.id)
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		used := map[string]bool{}

		if source != "" {
			var count int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM subscriptions WHERE id=?`, source).Scan(&count); err != nil {
				return err
			}
			if count == 0 {
				return errors.New("订阅不存在")
			}
			if _, err := tx.ExecContext(ctx, `UPDATE nodes SET available=0 WHERE source_id=?`, source); err != nil {
				return err
			}
		}
		for _, item := range items {
			cfg := make(map[string]any, len(item.Proxy))
			for k, v := range item.Proxy {
				cfg[k] = v
			}
			delete(cfg, "name")
			raw, err := json.Marshal(cfg)
			if err != nil {
				return err
			}
			digest := sha256.Sum256(raw)
			identity := hex.EncodeToString(digest[:])
			name := item.Node.Name

			id := byName[name]
			if id == "" {
				candidates := renameCandidates[identity]
				if len(candidates) == 1 && !used[candidates[0]] {
					id = candidates[0]
				}
			}
			if id == "" {
				id = newID()
			}
			used[id] = true
			protocol := fmt.Sprint(cfg["type"])
			_, err = tx.ExecContext(ctx, `INSERT INTO nodes(id,name,source_id,identity,protocol,server,port,config) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET name=excluded.name,identity=excluded.identity,protocol=excluded.protocol,server=excluded.server,port=excluded.port,config=excluded.config,available=1`, id, name, source, identity, protocol, item.Node.Server, item.Node.RawPort, string(raw))
			if err != nil {
				return err
			}
		}
		if source != "" {
			if profile != nil {
				if err := saveProfile(ctx, tx, source, *profile); err != nil {
					return err
				}
			}
			_, err := tx.ExecContext(ctx, `UPDATE subscriptions SET updated_at=? WHERE id=?`, time.Now().UTC().Format(time.RFC3339), source)
			return err
		}
		return nil
	})
}

func (s *Store) recordApply(ctx context.Context, revision int64, applyErr error) error {
	if applyErr != nil {
		_, err := s.db.ExecContext(ctx, `UPDATE state SET last_error=? WHERE id=1`, applyErr.Error())
		return err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE state SET applied_revision=?,last_error='',applied_at=? WHERE id=1`, revision, time.Now().UTC().Format(time.RFC3339))
	return err
}
