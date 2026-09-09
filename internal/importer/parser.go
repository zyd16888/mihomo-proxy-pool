package importer

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

type Proxy struct {
	Node  ImportNode
	Proxy map[string]any
}

func (n *ImportNode) UnmarshalJSON(data []byte) error {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	normalized := normalizeGenericValue(raw)
	rawMap, ok := normalized.(map[string]any)
	if !ok {
		return errors.New("invalid import node")
	}
	n.Raw = rawMap
	n.Name = strings.TrimSpace(stringValue(rawMap["name"]))
	n.Type = strings.TrimSpace(stringValue(rawMap["type"]))
	n.Server = strings.TrimSpace(stringValue(rawMap["server"]))
	n.Protocol = strings.TrimSpace(stringValue(rawMap["protocol"]))
	n.RawPort = intValue(rawMap["port"])
	return nil
}

func Parse(req ImportRequest) ([]Proxy, string, error) {
	if strings.TrimSpace(req.Raw) != "" {
		return parseRawProxyConfigsWithFormat(req.Raw)
	}
	if len(req.Nodes) > 0 {
		items, err := validateProxies(buildResolvedImportProxies(req.Nodes))
		return items, "JSON 节点数组", err
	}
	return nil, "", errors.New("subscriptionUrl、raw 或 nodes 至少需要一个")
}

func parseRawProxyConfigsWithFormat(raw string) ([]Proxy, string, error) {
	return parseRawStrict(strings.TrimSpace(raw), true)
}
func parseRawStrict(raw string, allowBase64 bool) ([]Proxy, string, error) {
	if raw == "" {
		return nil, "", errors.New("导入内容不能为空")
	}
	if strings.HasPrefix(raw, "[") || strings.HasPrefix(raw, "{") {
		items, err := parseJSONProxyConfigs(raw)
		return items, "JSON 节点数组", err
	}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(raw), &doc); err == nil {
		if _, ok := doc["proxies"]; ok {
			items, err := parseClashProxyConfigs(raw)
			return items, "Clash YAML", err
		}
	}
	if strings.Contains(raw, "://") {
		items, err := parseURIProxyConfigs(raw)
		return items, "URI 节点列表", err
	}
	if allowBase64 {
		if decoded, err := decodeBase64Any(strings.Join(strings.Fields(raw), "")); err == nil {
			return parseRawStrict(decoded, false)
		}
	}
	return nil, "", errors.New("未识别到有效节点；请提供 Clash YAML、JSON、URI 或 Base64 内容")
}

func parseJSONProxyConfigs(raw string) ([]Proxy, error) {
	var wrapped struct {
		Nodes []ImportNode `json:"nodes"`
	}
	if err := json.Unmarshal([]byte(raw), &wrapped); err == nil && len(wrapped.Nodes) > 0 {
		return validateProxies(buildResolvedImportProxies(wrapped.Nodes))
	}

	var direct []ImportNode
	if err := json.Unmarshal([]byte(raw), &direct); err == nil && len(direct) > 0 {
		return validateProxies(buildResolvedImportProxies(direct))
	}
	return nil, errors.New("not json proxy list")
}

func parseClashProxyConfigs(raw string) ([]Proxy, error) {
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(raw), &doc); err != nil {
		return nil, err
	}
	normalized, ok := normalizeGenericValue(doc).(map[string]any)
	if !ok {
		return nil, errors.New("invalid yaml document")
	}
	proxiesValue, ok := normalized["proxies"]
	if !ok {
		return nil, errors.New("no proxies found")
	}
	list, ok := proxiesValue.([]any)
	if !ok {
		return nil, errors.New("no proxies found")
	}
	items := make([]Proxy, 0, len(list))
	for _, entry := range list {
		proxy, ok := normalizeGenericValue(entry).(map[string]any)
		if !ok {
			return nil, errors.New("订阅包含无效节点对象，未导入任何节点")
		}
		node := importNodeFromProxyMap(proxy)
		if issue, invalid := validateImportNode(node); invalid {
			return nil, fmt.Errorf("节点 %s: %s", issue.Input, issue.Reason)
		}
		items = append(items, Proxy{
			Node:  node,
			Proxy: cloneProxyConfig(proxy),
		})
	}
	if len(items) == 0 {
		return nil, errors.New("no proxies found")
	}
	return validateProxies(normalizeResolvedImportProxies(items))
}

func parseURIProxyConfigs(raw string) ([]Proxy, error) {
	lines := strings.Split(raw, "\n")
	items := make([]Proxy, 0, len(lines))
	for _, line := range lines {
		text := strings.TrimSpace(strings.TrimRight(line, "\r"))
		if text == "" {
			continue
		}
		proxy, err := parseURIProxyConfig(text)
		if err != nil {
			return nil, fmt.Errorf("订阅包含无法解析的节点: %w", err)
		}
		node := importNodeFromProxyMap(proxy)
		if issue, invalid := validateImportNode(node); invalid {
			return nil, fmt.Errorf("节点 %s: %s", issue.Input, issue.Reason)
		}
		items = append(items, Proxy{
			Node:  node,
			Proxy: proxy,
		})
	}
	if len(items) == 0 {
		return nil, errors.New("no proxies found")
	}
	return validateProxies(normalizeResolvedImportProxies(items))
}

func buildResolvedImportProxies(nodes []ImportNode) []Proxy {
	items := make([]Proxy, 0, len(nodes))
	for _, node := range nodes {
		items = append(items, Proxy{
			Node:  node,
			Proxy: buildProxyConfigFromImportNode(node),
		})
	}
	return normalizeResolvedImportProxies(items)
}

func buildProxyConfigFromImportNode(node ImportNode) map[string]any {
	proxy := cloneProxyConfig(node.Raw)
	if proxy == nil {
		proxy = map[string]any{}
	}
	if strings.TrimSpace(node.Name) != "" {
		proxy["name"] = strings.TrimSpace(node.Name)
	}
	if strings.TrimSpace(node.Type) != "" {
		proxy["type"] = strings.TrimSpace(node.Type)
	}
	if strings.TrimSpace(node.Server) != "" {
		proxy["server"] = strings.TrimSpace(node.Server)
	}
	if node.RawPort > 0 {
		proxy["port"] = node.RawPort
	}
	if strings.TrimSpace(node.Protocol) != "" && strings.TrimSpace(stringValue(proxy["type"])) == "" {
		proxy["type"] = strings.TrimSpace(node.Protocol)
	}
	return proxy
}

func normalizeResolvedImportProxies(parsed []Proxy) []Proxy {
	out := make([]Proxy, 0, len(parsed))
	for _, item := range parsed {
		node := item.Node
		proxy := cloneProxyConfig(item.Proxy)
		if proxy == nil {
			proxy = map[string]any{}
		}

		name := strings.TrimSpace(node.Name)
		if name == "" {
			name = strings.TrimSpace(stringValue(proxy["name"]))
		}
		if name == "" {
			name = fmt.Sprintf("%s@%s:%d", node.Type, node.Server, node.RawPort)
		}

		node.Name = name
		if node.Type == "" {
			node.Type = strings.TrimSpace(stringValue(proxy["type"]))
		}
		if node.Protocol == "" {
			node.Protocol = strings.TrimSpace(stringValue(proxy["protocol"]))
		}
		if node.Protocol == "" {
			node.Protocol = strings.TrimSpace(stringValue(proxy["type"]))
		}
		if node.Server == "" {
			node.Server = strings.TrimSpace(stringValue(proxy["server"]))
		}
		if node.RawPort <= 0 || node.RawPort > 65535 {
			node.RawPort = intValue(proxy["port"])
		}

		proxy["name"] = name
		if node.Type != "" {
			proxy["type"] = node.Type
		}
		if node.Server != "" {
			proxy["server"] = node.Server
		}
		if node.RawPort > 0 {
			proxy["port"] = node.RawPort
		}
		out = append(out, Proxy{
			Node:  node,
			Proxy: proxy,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].Node.Name) < strings.ToLower(out[j].Node.Name)
	})
	return out
}

func importNodeFromProxyMap(proxy map[string]any) ImportNode {
	return ImportNode{
		Name:     strings.TrimSpace(stringValue(proxy["name"])),
		Type:     strings.TrimSpace(stringValue(proxy["type"])),
		Server:   strings.TrimSpace(stringValue(proxy["server"])),
		RawPort:  intValue(proxy["port"]),
		Protocol: strings.TrimSpace(stringValue(proxy["protocol"])),
		Raw:      cloneProxyConfig(proxy),
	}
}

func cloneProxyConfig(proxy map[string]any) map[string]any {
	if proxy == nil {
		return nil
	}
	cloned, _ := normalizeGenericValue(proxy).(map[string]any)
	return cloned
}

func marshalProxyConfigJSON(proxy map[string]any) (string, error) {
	data, err := json.Marshal(normalizeGenericValue(proxy))
	if err != nil {
		return "", fmt.Errorf("marshal proxy config failed: %w", err)
	}
	return string(data), nil
}

func normalizeGenericValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			out[key] = normalizeGenericValue(item)
		}
		return out
	case map[any]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			out[stringValue(key)] = normalizeGenericValue(item)
		}
		return out
	case []any:
		out := make([]any, 0, len(typed))
		for _, item := range typed {
			out = append(out, normalizeGenericValue(item))
		}
		return out
	default:
		return typed
	}
}

func stringValue(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case fmt.Stringer:
		return typed.String()
	case json.Number:
		return typed.String()
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", typed)
	}
}

func intValue(value any) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int8:
		return int(typed)
	case int16:
		return int(typed)
	case int32:
		return int(typed)
	case int64:
		return int(typed)
	case uint:
		return int(typed)
	case uint8:
		return int(typed)
	case uint16:
		return int(typed)
	case uint32:
		return int(typed)
	case uint64:
		return int(typed)
	case float32:
		return int(typed)
	case float64:
		return int(typed)
	case json.Number:
		n, _ := typed.Int64()
		return int(n)
	case string:
		n, _ := strconv.Atoi(strings.TrimSpace(typed))
		return n
	default:
		return 0
	}
}

func parseURIProxyConfig(line string) (map[string]any, error) {
	if strings.HasPrefix(line, "vmess://") {
		return parseVMESSProxyConfig(line)
	}
	u, err := url.Parse(line)
	if err != nil {
		return nil, errors.New("节点链接格式不正确")
	}
	switch u.Scheme {
	case "trojan", "vless", "ss":
		return parseStandardURIProxyConfig(u)
	default:
		return nil, errors.New("暂不支持的节点协议")
	}
}

func parseStandardURIProxyConfig(u *url.URL) (map[string]any, error) {
	name := u.Fragment // url.Parse has already decoded percent escapes; '+' is literal here.
	host := u.Hostname()
	portStr := u.Port()
	query, queryErr := url.ParseQuery(u.RawQuery)
	if queryErr != nil {
		return nil, errors.New("节点链接参数编码无效，请重新复制完整链接")
	}

	proxy := map[string]any{
		"name":   strings.TrimSpace(name),
		"type":   u.Scheme,
		"server": host,
	}

	if u.Scheme == "ss" && host == "" && u.Opaque != "" {
		opaque := strings.Split(u.Opaque, "?")[0]
		decoded, err := decodeBase64Any(opaque)
		if err == nil {
			parts := strings.SplitN(decoded, "@", 2)
			if len(parts) == 2 {
				creds := parts[0]
				endpoint := parts[1]
				if method, password, ok := strings.Cut(creds, ":"); ok {
					proxy["cipher"] = method
					proxy["password"] = password
				}
				if hostPart, portPart, ok := strings.Cut(endpoint, ":"); ok {
					host = hostPart
					portStr = portPart
				}
			}
		}
	}

	port, err := strconv.Atoi(strings.TrimSpace(portStr))
	if err != nil || strings.TrimSpace(host) == "" {
		return nil, errors.New("节点链接缺少服务地址或端口")
	}

	proxy["server"] = strings.TrimSpace(host)
	proxy["port"] = port

	switch u.Scheme {
	case "trojan":
		if user := u.User.Username(); user != "" {
			proxy["password"] = user
		}
	case "vless":
		if user := u.User.Username(); user != "" {
			proxy["uuid"] = user
		}
	case "ss":
		if userInfo := u.User.String(); userInfo != "" {
			decoded := userInfo
			if !strings.Contains(decoded, ":") {
				if raw, err := decodeBase64Any(decoded); err == nil {
					decoded = raw
				}
			}
			if method, password, ok := strings.Cut(decoded, ":"); ok {
				proxy["cipher"] = method
				proxy["password"] = password
			}
		}
	}

	if sni := strings.TrimSpace(query.Get("sni")); sni != "" {
		proxy["sni"] = sni
		if u.Scheme == "vless" {
			proxy["servername"] = sni
		}
	}
	if insecure := strings.TrimSpace(query.Get("allowInsecure")); insecure != "" {
		lower := strings.ToLower(insecure)
		proxy["skip-cert-verify"] = lower == "1" || lower == "true"
	}
	if network := strings.TrimSpace(query.Get("type")); network != "" {
		proxy["network"] = network
		switch network {
		case "ws":
			wsOpts := map[string]any{"path": query.Get("path")}
			hostHeader := strings.TrimSpace(query.Get("host"))
			if hostHeader == "" {
				hostHeader = strings.TrimSpace(query.Get("peer"))
			}
			if hostHeader != "" {
				wsOpts["headers"] = map[string]any{"Host": hostHeader}
			}
			proxy["ws-opts"] = wsOpts
		case "grpc":
			proxy["grpc-opts"] = map[string]any{
				"grpc-service-name": strings.TrimSpace(query.Get("serviceName")),
			}
		}
	}
	if u.Scheme != "http" {
		proxy["udp"] = true
	}
	if u.Scheme == "vless" {
		if err := applyVLESSOptions(proxy, query); err != nil {
			return nil, err
		}
	}
	return proxy, nil
}

func parseVMESSProxyConfig(line string) (map[string]any, error) {
	payload, err := decodeBase64Any(strings.TrimPrefix(line, "vmess://"))
	if err != nil {
		return nil, errors.New("vmess 节点内容不是有效的 Base64")
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(payload), &raw); err != nil {
		return nil, errors.New("vmess 节点内容不是有效的 JSON")
	}
	add := strings.TrimSpace(stringValue(raw["add"]))
	port := intValue(raw["port"])
	if add == "" || port <= 0 {
		return nil, errors.New("vmess 节点缺少服务地址或端口")
	}
	proxy := map[string]any{
		"name":    strings.TrimSpace(stringValue(raw["ps"])),
		"type":    "vmess",
		"server":  add,
		"port":    port,
		"uuid":    strings.TrimSpace(stringValue(raw["id"])),
		"alterId": intValue(raw["aid"]),
	}
	cipher := strings.TrimSpace(stringValue(raw["scy"]))
	if cipher == "" {
		cipher = "auto"
	}
	proxy["cipher"] = cipher
	if strings.EqualFold(strings.TrimSpace(stringValue(raw["tls"])), "tls") {
		proxy["tls"] = true
	}
	network := strings.TrimSpace(stringValue(raw["net"]))
	if network != "" {
		proxy["network"] = network
	}
	if network == "ws" {
		wsOpts := map[string]any{"path": strings.TrimSpace(stringValue(raw["path"]))}
		host := strings.TrimSpace(stringValue(raw["host"]))
		if host != "" {
			wsOpts["headers"] = map[string]any{"Host": host}
		}
		proxy["ws-opts"] = wsOpts
	}
	return proxy, nil
}

type ImportRequest struct {
	Raw   string       `json:"raw"`
	Nodes []ImportNode `json:"nodes"`
}
type ImportNode struct {
	Name     string         `json:"name"`
	Type     string         `json:"type"`
	Server   string         `json:"server"`
	RawPort  int            `json:"port"`
	Protocol string         `json:"protocol"`
	Raw      map[string]any `json:"-"`
}
type ImportPreviewIssue struct {
	Input  string
	Reason string
}

func decodeBase64Any(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimRight(raw, "=")
	encodings := []*base64.Encoding{
		base64.RawStdEncoding,
		base64.RawURLEncoding,
		base64.RawStdEncoding,
		base64.RawURLEncoding,
	}
	for _, enc := range encodings {
		decoded, err := enc.DecodeString(raw)
		if err == nil {
			return string(decoded), nil
		}
	}
	return "", errors.New("不是有效的 Base64 内容")
}

func validateImportNode(node ImportNode) (ImportPreviewIssue, bool) {
	input := strings.TrimSpace(node.Name)
	if input == "" {
		input = trimPreviewInput(node.Server)
	}
	if input == "" {
		input = "未命名节点"
	}
	if strings.TrimSpace(node.Server) == "" {
		return ImportPreviewIssue{Input: input, Reason: "缺少服务地址"}, true
	}
	if node.RawPort <= 0 || node.RawPort > 65535 {
		return ImportPreviewIssue{Input: input, Reason: "缺少远程端口"}, true
	}
	protocol := strings.TrimSpace(node.Protocol)
	if protocol == "" {
		protocol = strings.TrimSpace(node.Type)
	}
	if protocol == "" {
		return ImportPreviewIssue{Input: input, Reason: "缺少节点协议"}, true
	}
	return ImportPreviewIssue{}, false
}

func trimPreviewInput(value string) string {
	value = strings.TrimSpace(value)
	runes := []rune(value)
	if len(runes) <= 48 {
		return value
	}
	return string(runes[:48]) + "..."
}

func validateProxies(items []Proxy) ([]Proxy, error) {
	if len(items) == 0 {
		return nil, errors.New("没有可导入的节点")
	}
	names := map[string]bool{}
	for _, item := range items {
		if issue, bad := validateImportNode(item.Node); bad {
			return nil, fmt.Errorf("节点 %s: %s", issue.Input, issue.Reason)
		}
		if names[item.Node.Name] {
			return nil, fmt.Errorf("节点名称重复: %s，请先确保同一来源名称唯一", item.Node.Name)
		}
		names[item.Node.Name] = true
	}
	return items, nil
}
