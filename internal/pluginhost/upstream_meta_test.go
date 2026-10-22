package pluginhost

import "testing"

func TestTakeUpstreamMetadata(t *testing.T) {
	protocol, passThrough := takeUpstreamMetadata(map[string]any{
		"upstream_protocol":     "chat-completions",
		"upstream_passthrough":  true,
		"unrelated_plugin_data": "x",
	})
	if protocol != "chat-completions" || !passThrough {
		t.Fatalf("protocol=%q passThrough=%t", protocol, passThrough)
	}
}

func TestTakeUpstreamMetadataAbsent(t *testing.T) {
	if protocol, passThrough := takeUpstreamMetadata(nil); protocol != "" || passThrough {
		t.Fatalf("nil metadata: %q %t", protocol, passThrough)
	}
	if protocol, passThrough := takeUpstreamMetadata(map[string]any{"other": 1}); protocol != "" || passThrough {
		t.Fatalf("unrelated metadata: %q %t", protocol, passThrough)
	}
}
