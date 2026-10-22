package redisqueue

import "testing"

func TestUpstreamProtocolFromURL(t *testing.T) {
	cases := map[string]string{
		"https://opencode.ai/inference/go/v1/chat/completions": "chat-completions",
		"https://opencode.ai/inference/go/v1/responses":        "responses",
		"https://api.anthropic.com/v1/messages":                "messages",
		"https://example.com/v1/chat/completions?x=1":          "chat-completions",
		"https://example.com/health":                           "",
		"":                                                     "",
	}
	for raw, want := range cases {
		if got := upstreamProtocolFromURL(raw); got != want {
			t.Fatalf("upstreamProtocolFromURL(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestProtocolFromEndpoint(t *testing.T) {
	cases := map[string]string{
		"POST /v1/chat/completions": "chat-completions",
		"POST /v1/responses":        "responses",
		"POST /v1/messages":         "messages",
		"POST /v1/models":           "",
		"":                          "",
	}
	for endpoint, want := range cases {
		if got := protocolFromEndpoint(endpoint); got != want {
			t.Fatalf("protocolFromEndpoint(%q) = %q, want %q", endpoint, got, want)
		}
	}
}
