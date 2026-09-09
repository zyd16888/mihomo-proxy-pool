package manager

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"mihomo-proxy/internal/importer"
)

func splitLines(raw string) []string {
	out := []string{}
	for _, line := range strings.Split(raw, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func (s *Store) ruleSets(ctx context.Context) ([]RuleSet, error) {
	sets := []RuleSet{}
	rows, err := s.db.QueryContext(ctx, `SELECT id,name,policy,behavior,format,url,interval,no_resolve,enabled,position,builtin FROM rule_sets ORDER BY position,name`)
	if err != nil {
		return sets, err
	}
	defer rows.Close()
	for rows.Next() {
		var set RuleSet
		if err := rows.Scan(&set.ID, &set.Name, &set.Policy, &set.Behavior, &set.Format, &set.URL, &set.Interval, &set.NoResolve, &set.Enabled, &set.Position, &set.Builtin); err != nil {
			return sets, err
		}
		sets = append(sets, set)
	}
	return sets, rows.Err()
}

func (s *Store) routing(ctx context.Context) (Routing, error) {
	var routing Routing
	var domestic, foreign string
	err := s.db.QueryRowContext(ctx, `SELECT enabled,default_policy,merge_sub_rules,sub_rule_position,allow_geo_rules,rule_set_proxy,dns_enabled,dns_domestic,dns_foreign FROM routing WHERE id=1`).
		Scan(&routing.Enabled, &routing.DefaultPolicy, &routing.MergeSubRules, &routing.SubRulePosition, &routing.AllowGeoRules, &routing.RuleSetProxy, &routing.DNSEnabled, &domestic, &foreign)
	routing.DNSDomestic = splitLines(domestic)
	routing.DNSForeign = splitLines(foreign)
	return routing, err
}

func (s *Store) SaveRouting(ctx context.Context, routing Routing) error {
	if !validPolicy(routing.DefaultPolicy) {
		return errors.New("默认策略不在可选策略中")
	}
	if routing.RuleSetProxy != "DIRECT" && routing.RuleSetProxy != GroupSelect {
		return errors.New("规则集下载出口只能是 DIRECT 或节点选择")
	}
	if routing.SubRulePosition < 0 || routing.SubRulePosition > 100000 {
		return errors.New("订阅规则插入位置必须在 0 到 100000 之间")
	}
	return s.mutate(ctx, func(tx *sql.Tx) error {
		if !routing.Enabled {
			var active int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM listeners WHERE mode='rule' AND enabled=1`).Scan(&active); err != nil {
				return err
			}
			if active > 0 {
				return errors.New("仍有启用中的规则监听，请先停用后再关闭规则分流")
			}
		}
		return changed(tx.ExecContext(ctx, `UPDATE routing SET enabled=?,default_policy=?,merge_sub_rules=?,sub_rule_position=?,allow_geo_rules=?,rule_set_proxy=?,dns_enabled=?,dns_domestic=?,dns_foreign=? WHERE id=1`,
			routing.Enabled, routing.DefaultPolicy, routing.MergeSubRules, routing.SubRulePosition, routing.AllowGeoRules, routing.RuleSetProxy, routing.DNSEnabled,
			strings.Join(routing.DNSDomestic, "\n"), strings.Join(routing.DNSForeign, "\n")))
	})
}

func (s *Store) SaveRuleSet(ctx context.Context, set RuleSet) error {
	set.Name = strings.TrimSpace(set.Name)
	set.URL = strings.TrimSpace(set.URL)
	if set.Interval == 0 {
		set.Interval = 86400
	}
	if err := ValidateRuleSet(set); err != nil {
		return err
	}
	return s.mutate(ctx, func(tx *sql.Tx) error {
		var other int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM rule_sets WHERE name=? AND id<>?`, set.Name, set.ID).Scan(&other); err != nil {
			return err
		}
		if other > 0 {
			return errors.New("规则集名称已存在")
		}
		if set.ID == "" {
			_, err := tx.ExecContext(ctx, `INSERT INTO rule_sets(id,name,policy,behavior,format,url,interval,no_resolve,enabled,position,builtin) VALUES(?,?,?,?,?,?,?,?,?,?,0)`,
				newID(), set.Name, set.Policy, set.Behavior, set.Format, set.URL, set.Interval, set.NoResolve, set.Enabled, set.Position)
			return err
		}
		return changed(tx.ExecContext(ctx, `UPDATE rule_sets SET name=?,policy=?,behavior=?,format=?,url=?,interval=?,no_resolve=?,enabled=?,position=? WHERE id=?`,
			set.Name, set.Policy, set.Behavior, set.Format, set.URL, set.Interval, set.NoResolve, set.Enabled, set.Position, set.ID))
	})
}

func (s *Store) DeleteRuleSet(ctx context.Context, id string) error {
	return s.mutate(ctx, func(tx *sql.Tx) error {
		return changed(tx.ExecContext(ctx, `DELETE FROM rule_sets WHERE id=?`, id))
	})
}

func (s *Store) subscriptionProfiles(ctx context.Context) ([]SubscriptionProfile, error) {
	profiles := []SubscriptionProfile{}
	rows, err := s.db.QueryContext(ctx, `SELECT subscription_id,groups,rules,providers,updated_at FROM subscription_profiles`)
	if err != nil {
		return profiles, err
	}
	defer rows.Close()
	for rows.Next() {
		var item SubscriptionProfile
		var groups, rules, providers string
		if err := rows.Scan(&item.SubscriptionID, &groups, &rules, &providers, &item.UpdatedAt); err != nil {
			return profiles, err
		}
		// A profile that no longer decodes is treated as absent: the rest of
		// the configuration must still build.
		json.Unmarshal([]byte(groups), &item.Profile.Groups)
		json.Unmarshal([]byte(rules), &item.Profile.Rules)
		json.Unmarshal([]byte(providers), &item.Profile.Providers)
		profiles = append(profiles, item)
	}
	return profiles, rows.Err()
}

// SaveSubscriptionProfile records the routing half of a subscription. It runs
// inside the same change as the node import so a sync never leaves nodes and
// rules describing different versions of the subscription.
func saveProfile(ctx context.Context, tx *sql.Tx, subscriptionID string, profile importer.Profile) error {
	if profile.Empty() {
		_, err := tx.ExecContext(ctx, `DELETE FROM subscription_profiles WHERE subscription_id=?`, subscriptionID)
		return err
	}
	groups, err := json.Marshal(profile.Groups)
	if err != nil {
		return err
	}
	rules, err := json.Marshal(profile.Rules)
	if err != nil {
		return err
	}
	providers, err := json.Marshal(profile.Providers)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO subscription_profiles(subscription_id,groups,rules,providers,group_count,rule_count,provider_count,updated_at)
		VALUES(?,?,?,?,?,?,?,?)
		ON CONFLICT(subscription_id) DO UPDATE SET groups=excluded.groups,rules=excluded.rules,providers=excluded.providers,
		group_count=excluded.group_count,rule_count=excluded.rule_count,provider_count=excluded.provider_count,updated_at=excluded.updated_at`,
		subscriptionID, string(groups), string(rules), string(providers),
		len(profile.Groups), len(profile.Rules), len(profile.Providers), time.Now().UTC().Format(time.RFC3339))
	return err
}
