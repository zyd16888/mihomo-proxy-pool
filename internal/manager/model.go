package manager

import (
	"encoding/json"

	"mihomo-proxy/internal/importer"
)

type Node struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	SourceID  string          `json:"sourceId"`
	Protocol  string          `json:"protocol"`
	Server    string          `json:"server"`
	Port      int             `json:"port"`
	Enabled   bool            `json:"enabled"`
	Available bool            `json:"available"`
	Config    json.RawMessage `json:"-"`
}

type Subscription struct {
	ID        string             `json:"id"`
	Name      string             `json:"name"`
	URL       string             `json:"url"`
	UpdatedAt string             `json:"updatedAt"`
	Usage     *SubscriptionUsage `json:"usage,omitempty"`
	Profile   *ProfileSummary    `json:"profile,omitempty"`
}

// ProfileSummary reports what the subscription contributed to rule routing.
// It is a stored count, not a live evaluation of the current configuration.
type ProfileSummary struct {
	Groups    int    `json:"groups"`
	Rules     int    `json:"rules"`
	Providers int    `json:"providers"`
	UpdatedAt string `json:"updatedAt"`
}

const (
	ListenerModeNode = "node"
	ListenerModeRule = "rule"
)

type Listener struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Port int    `json:"port"`
	// Mode is ListenerModeNode for a port pinned to one outbound, or
	// ListenerModeRule for a port that resolves its outbound per request.
	Mode    string `json:"mode"`
	NodeID  string `json:"nodeId"`
	Enabled bool   `json:"enabled"`
}

func (l Listener) RuleMode() bool { return l.Mode == ListenerModeRule }

// RuleSet is one remote rule provider plus the policy its matches resolve to.
// Position orders it against the other sets and against merged subscription
// rules, because in Clash the first matching rule wins.
type RuleSet struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Policy    string `json:"policy"`
	Behavior  string `json:"behavior"`
	Format    string `json:"format"`
	URL       string `json:"url"`
	Interval  int    `json:"interval"`
	NoResolve bool   `json:"noResolve"`
	Enabled   bool   `json:"enabled"`
	Position  int    `json:"position"`
	Builtin   bool   `json:"builtin"`
}

// Routing holds the settings shared by every rule listener. Rule listeners
// exist independently of these settings; disabling routing only removes the
// generated groups, rules and DNS block.
type Routing struct {
	Enabled         bool     `json:"enabled"`
	DefaultPolicy   string   `json:"defaultPolicy"`
	MergeSubRules   bool     `json:"mergeSubRules"`
	SubRulePosition int      `json:"subRulePosition"`
	AllowGeoRules   bool     `json:"allowGeoRules"`
	RuleSetProxy    string   `json:"ruleSetProxy"`
	DNSEnabled      bool     `json:"dnsEnabled"`
	DNSDomestic     []string `json:"dnsDomestic"`
	DNSForeign      []string `json:"dnsForeign"`
}

// SubscriptionProfile is the stored routing half of a subscription, kept so a
// configuration rebuild never depends on the subscription being reachable.
type SubscriptionProfile struct {
	SubscriptionID string           `json:"subscriptionId"`
	Profile        importer.Profile `json:"profile"`
	UpdatedAt      string           `json:"updatedAt"`
}

type State struct {
	ProxyGroups   []ProxyGroup          `json:"proxyGroups"`
	Selections    map[string]string     `json:"selections"`
	Nodes         []Node                `json:"nodes"`
	Subscriptions []Subscription        `json:"subscriptions"`
	Listeners     []Listener            `json:"listeners"`
	RuleSets      []RuleSet             `json:"ruleSets"`
	Routing       Routing               `json:"routing"`
	Profiles      []SubscriptionProfile `json:"-"`
	// ProbePort is filled in by the manager, not the store: it is a deployment
	// setting rather than saved configuration.
	ProbePort       int    `json:"-"`
	Revision        int64  `json:"revision"`
	AppliedRevision int64  `json:"appliedRevision"`
	LastError       string `json:"lastError"`
	AppliedAt       string `json:"appliedAt"`
}

type ApplyResult struct {
	Applied  bool   `json:"applied"`
	Revision int64  `json:"revision"`
	Error    string `json:"error,omitempty"`
}
