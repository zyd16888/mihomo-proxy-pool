package importer

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestFormatsPreserveFullProxyConfig(t *testing.T) {
	yamlText := "proxies:\n  - name: Japan\n    type: ss\n    server: example.net\n    port: 443\n    cipher: aes-128-gcm\n    password: secret\n"
	for _, raw := range []string{yamlText, base64.StdEncoding.EncodeToString([]byte(yamlText)), base64.RawStdEncoding.EncodeToString([]byte(yamlText)), `[{"name":"Japan","type":"ss","server":"example.net","port":443,"cipher":"aes-128-gcm","password":"secret"}]`, `ss://YWVzLTEyOC1nY206c2VjcmV0@example.net:443#Japan`} {
		items, _, err := Parse(ImportRequest{Raw: raw})
		if err != nil {
			t.Fatal(err)
		}
		if len(items) != 1 || items[0].Proxy["password"] != "secret" || items[0].Node.RawPort != 443 {
			t.Fatalf("lost proxy config: %+v", items)
		}
	}
}

func TestRejectPartialInvalidImport(t *testing.T) {
	for _, raw := range []string{
		`[{"name":"good","type":"http","server":"localhost","port":8080},{"name":"bad","type":"ss"}]`,
		"proxies:\n  - {name: good, type: http, server: localhost, port: 8080}\n  - {name: bad, type: ss}\n",
		"trojan://password@example.net:443#good\nunknown://bad",
		`[{"name":"same","type":"http","server":"localhost","port":8080},{"name":"same","type":"http","server":"localhost","port":8081}]`,
		"proxies: []", `[{"name":"bad-port","type":"http","server":"localhost","port":70000}]`,
	} {
		if _, _, err := Parse(ImportRequest{Raw: raw}); err == nil {
			t.Fatalf("accepted incomplete import: %s", raw)
		}
	}
}

func TestUnsupportedFormatErrorDoesNotEchoSecrets(t *testing.T) {
	_, _, err := Parse(ImportRequest{Raw: "unknown://very-secret-password@host:443"})
	if err == nil || strings.Contains(err.Error(), "very-secret") {
		t.Fatalf("unexpected error: %v", err)
	}
}
