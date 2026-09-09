package importer

import (
	"encoding/base64"
	"reflect"
	"strings"
	"testing"
)

func realityTestURI() string {
	key := base64.RawURLEncoding.EncodeToString([]byte("01234567890123456789012345678901"))
	return "vless://00000000-0000-4000-8000-000000000001@example.invalid:443?security=reality&type=tcp&sni=www.example.com&fp=chrome&pbk=" + key + "&sid=0123456789abcdef&flow=xtls-rprx-vision&alpn=h2%2Chttp%2F1.1&packetEncoding=xudp#Reality%20Vision"
}

func TestVLESSRealityVisionParametersSurviveImport(t *testing.T) {
	for _, raw := range []string{realityTestURI(), base64.StdEncoding.EncodeToString([]byte(realityTestURI()))} {
		items, _, err := Parse(ImportRequest{Raw: raw})
		if err != nil {
			t.Fatal(err)
		}
		p := items[0].Proxy
		for key, want := range map[string]any{"tls": true, "flow": "xtls-rprx-vision", "servername": "www.example.com", "client-fingerprint": "chrome", "packet-encoding": "xudp", "name": "Reality Vision"} {
			if p[key] != want {
				t.Fatalf("%s: got %v, want %v", key, p[key], want)
			}
		}
		reality, ok := p["reality-opts"].(map[string]any)
		if !ok || reality["short-id"] != "0123456789abcdef" || reality["public-key"] == "" {
			t.Fatal("lost Reality options")
		}
		if !reflect.DeepEqual(p["alpn"], []string{"h2", "http/1.1"}) {
			t.Fatal("lost ALPN")
		}
	}
}

func TestVLESSTLSWebSocketAndGRPC(t *testing.T) {
	for _, transport := range []string{"ws", "grpc"} {
		raw := "vless://00000000-0000-4000-8000-000000000001@example.invalid:443?security=tls&network=" + transport + "&path=%2Fproxy%3Ftoken%3Ddemo&host=cdn.example.com&serviceName=my-service&sni=tls.example.com&fp=firefox&alpn=h2%2Chttp%2F1.1#A+B%2520C"
		items, _, err := Parse(ImportRequest{Raw: raw})
		if err != nil {
			t.Fatal(err)
		}
		p := items[0].Proxy
		if p["name"] != "A+B%20C" || p["tls"] != true || p["network"] != transport || p["client-fingerprint"] != "firefox" {
			t.Fatal("transport/security fields lost")
		}
		if transport == "ws" {
			opts := p["ws-opts"].(map[string]any)
			if opts["path"] != "/proxy?token=demo" || opts["headers"].(map[string]any)["Host"] != "cdn.example.com" {
				t.Fatal("WS options lost")
			}
		} else if p["grpc-opts"].(map[string]any)["grpc-service-name"] != "my-service" {
			t.Fatal("gRPC options lost")
		}
	}
}

func TestVLESSRealityDefaultsAndPlainCompatibility(t *testing.T) {
	raw := strings.ReplaceAll(realityTestURI(), "&fp=chrome", "")
	raw = strings.ReplaceAll(raw, "&sid=0123456789abcdef", "")
	items, _, err := Parse(ImportRequest{Raw: raw})
	if err != nil {
		t.Fatal(err)
	}
	if items[0].Proxy["client-fingerprint"] != "chrome" {
		t.Fatal("Reality requires a usable default fingerprint")
	}
	plain := "vless://00000000-0000-4000-8000-000000000001@example.invalid:80?security=none#Plain"
	items, _, err = Parse(ImportRequest{Raw: plain})
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := items[0].Proxy["tls"]; exists {
		t.Fatal("plain VLESS unexpectedly enabled TLS")
	}
}

func TestVLESSInvalidCriticalOptionsAreNotSilentlyDropped(t *testing.T) {
	base := realityTestURI()
	for _, raw := range []string{
		strings.Replace(base, "&pbk=", "&missing-pbk=", 1),
		strings.Replace(base, "sid=0123456789abcdef", "sid=xyz", 1),
		strings.Replace(base, "security=reality", "security=unsupported", 1),
		strings.Replace(base, "type=tcp", "type=unsupported", 1),
		strings.Replace(base, "flow=xtls-rprx-vision", "flow=unknown", 1),
		strings.Replace(base, "security=reality", "security=none", 1),
		strings.Replace(base, "fp=chrome", "fp=%XX", 1),
	} {
		if _, _, err := Parse(ImportRequest{Raw: raw}); err == nil {
			t.Fatal("invalid critical option was discarded")
		} else if strings.Contains(err.Error(), "00000000-0000") {
			t.Fatal("error exposed node credential")
		}
	}
}
