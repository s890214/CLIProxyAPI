package pluginhost

import (
	"net/http"
	"testing"
)

func TestTakeUpstreamMetaHeaders(t *testing.T) {
	headers := http.Header{}
	headers.Set(upstreamProtocolHeader, "chat-completions")
	headers.Set(upstreamPassThroughHeader, "1")
	headers.Set("Content-Type", "application/json")

	protocol, passThrough := takeUpstreamMetaHeaders(headers)
	if protocol != "chat-completions" || !passThrough {
		t.Fatalf("protocol=%q passThrough=%t", protocol, passThrough)
	}
	if headers.Get(upstreamProtocolHeader) != "" || headers.Get(upstreamPassThroughHeader) != "" {
		t.Fatalf("internal headers leaked: %v", headers)
	}
	if headers.Get("Content-Type") != "application/json" {
		t.Fatalf("upstream header dropped: %v", headers)
	}
}

func TestTakeUpstreamMetaHeadersAbsent(t *testing.T) {
	protocol, passThrough := takeUpstreamMetaHeaders(http.Header{"X-Test": {"1"}})
	if protocol != "" || passThrough {
		t.Fatalf("protocol=%q passThrough=%t", protocol, passThrough)
	}
	if p, b := takeUpstreamMetaHeaders(nil); p != "" || b {
		t.Fatalf("nil headers: %q %t", p, b)
	}
}
