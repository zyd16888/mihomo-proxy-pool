package manager

import "encoding/json"

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
}

type Listener struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Port    int    `json:"port"`
	NodeID  string `json:"nodeId"`
	Enabled bool   `json:"enabled"`
}

type State struct {
	Nodes           []Node         `json:"nodes"`
	Subscriptions   []Subscription `json:"subscriptions"`
	Listeners       []Listener     `json:"listeners"`
	Revision        int64          `json:"revision"`
	AppliedRevision int64          `json:"appliedRevision"`
	LastError       string         `json:"lastError"`
	AppliedAt       string         `json:"appliedAt"`
}

type ApplyResult struct {
	Applied  bool   `json:"applied"`
	Revision int64  `json:"revision"`
	Error    string `json:"error,omitempty"`
}
