package importer

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/url"
	"strings"
)

func firstQuery(q url.Values, keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(q.Get(key)); value != "" {
			return value
		}
	}
	return ""
}

// A share URI must retain its transport/security contract. In particular a
// Reality link is not equivalent to a plain VLESS proxy with the same UUID.
func applyVLESSOptions(proxy map[string]any, q url.Values) error {
	if strings.TrimSpace(stringValue(proxy["uuid"])) == "" {
		return errors.New("VLESS 链接缺少 UUID")
	}
	if encryption := strings.TrimSpace(q.Get("encryption")); encryption != "" && encryption != "none" {
		return errors.New("此 VLESS 加密参数尚未支持，请使用完整 Mihomo YAML/JSON 配置")
	}
	security := strings.ToLower(strings.TrimSpace(q.Get("security")))
	if security == "" && (q.Get("tls") == "1" || strings.EqualFold(q.Get("tls"), "true")) {
		security = "tls"
	}
	switch security {
	case "", "none":
	case "tls", "reality":
		proxy["tls"] = true
	default:
		return errors.New("不支持的 VLESS security，请使用完整 Mihomo YAML/JSON 配置")
	}
	delete(proxy, "sni")
	if name := firstQuery(q, "sni", "serverName", "servername", "peer"); name != "" {
		proxy["servername"] = name
	}
	if fp := strings.TrimSpace(q.Get("fp")); fp != "" {
		proxy["client-fingerprint"] = fp
	}
	if alpn := strings.TrimSpace(q.Get("alpn")); alpn != "" {
		values := []string{}
		for _, value := range strings.Split(alpn, ",") {
			if v := strings.TrimSpace(value); v != "" {
				values = append(values, v)
			}
		}
		if len(values) == 0 {
			return errors.New("VLESS ALPN 列表为空")
		}
		proxy["alpn"] = values
	}
	if flow := strings.TrimSpace(q.Get("flow")); flow != "" {
		if flow == "xtls-rprx-vision-udp443" {
			flow = "xtls-rprx-vision"
		}
		if flow != "xtls-rprx-vision" {
			return errors.New("不支持的 VLESS flow，请使用完整 Mihomo YAML/JSON 配置")
		}
		if security != "tls" && security != "reality" {
			return errors.New("VLESS Vision 需要 TLS 或 Reality")
		}
		proxy["flow"] = flow
	}
	if security == "reality" {
		key := firstQuery(q, "pbk", "publicKey")
		raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(key, "="))
		if err != nil || len(raw) != 32 {
			return errors.New("Reality 链接缺少有效的 pbk 公钥，请重新复制完整链接")
		}
		sid := firstQuery(q, "sid", "shortId")
		short, err := hex.DecodeString(sid)
		if err != nil || len(short) > 8 {
			return errors.New("Reality short-id 必须为不超过 16 位的偶数长度十六进制字符串")
		}
		proxy["reality-opts"] = map[string]any{"public-key": strings.TrimRight(key, "="), "short-id": sid}
		if proxy["client-fingerprint"] == nil {
			proxy["client-fingerprint"] = "chrome"
		}
	}
	if encoding := firstQuery(q, "packetEncoding", "packet-encoding"); encoding != "" {
		switch encoding {
		case "xudp", "packetaddr":
			proxy["packet-encoding"] = encoding
		case "none":
		default:
			return errors.New("不支持的 VLESS UDP 包编码")
		}
	}
	if value := firstQuery(q, "allowInsecure", "insecure"); value != "" {
		switch strings.ToLower(value) {
		case "1", "true":
			proxy["skip-cert-verify"] = true
		case "0", "false":
			proxy["skip-cert-verify"] = false
		default:
			return errors.New("VLESS insecure 参数必须为布尔值")
		}
	}
	network := strings.ToLower(firstQuery(q, "type", "network"))
	switch network {
	case "", "tcp":
		delete(proxy, "network")
	case "ws":
		proxy["network"] = "ws"
	case "grpc":
		proxy["network"] = "grpc"
	default:
		return errors.New("此 VLESS URI 传输类型尚未支持，请使用完整 Mihomo YAML/JSON 配置")
	}
	// The primary URI converter supplies standard WS/gRPC fields. Also support
	// links using network= instead of type= without silently dropping their options.
	if network == "ws" {
		opts := map[string]any{"path": q.Get("path")}
		if host := q.Get("host"); host != "" {
			opts["headers"] = map[string]any{"Host": host}
		}
		proxy["ws-opts"] = opts
	}
	if network == "grpc" {
		proxy["grpc-opts"] = map[string]any{"grpc-service-name": q.Get("serviceName")}
	}
	return nil
}
