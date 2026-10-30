package helps

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestProtocolFromPath(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"/v1/chat/completions", ProtocolChatCompletions},
		{"/v1/responses", ProtocolResponses},
		{"/responses?stream=true", ProtocolResponses},
		{"/v1/messages", ProtocolMessages},
		{"/v1beta/models/gemini-3-pro:generateContent", ProtocolGemini},
		{"/v1internal:streamGenerateContent", ProtocolGemini},
		{"/v1/completions", ProtocolCompletions},
		{"/v1/models", ""},
		{"", ""},
	}
	for _, tc := range cases {
		if got := ProtocolFromPath(tc.path); got != tc.want {
			t.Fatalf("ProtocolFromPath(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

type stubRoundTripper struct{}

func (stubRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("{}")),
		Header:     http.Header{},
	}, nil
}

func clientPathContext(t *testing.T, path string) context.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginCtx.Request = httptest.NewRequest(http.MethodPost, path, nil)
	return context.WithValue(context.Background(), "gin", ginCtx)
}

func doUpstream(t *testing.T, client *http.Client, url string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	_ = resp.Body.Close()
}

// A translated leg: chat-completions client served by the responses upstream.
func TestUsageReporterDerivesUpstreamProtocolFromRoundTrip(t *testing.T) {
	ctx := clientPathContext(t, "/v1/chat/completions")
	reporter := NewExecutorUsageReporter(ctx, nil, "gpt-6.1-sol", nil)
	client := reporter.TrackHTTPClientRoundTripOnly(&http.Client{Transport: stubRoundTripper{}})

	doUpstream(t, client, "http://upstream.example/v1/responses")

	record := reporter.buildRecord(usage.Detail{InputTokens: 10}, false)
	if record.UpstreamProtocol != ProtocolResponses {
		t.Fatalf("upstream protocol = %q, want %q", record.UpstreamProtocol, ProtocolResponses)
	}
	if record.UpstreamPassThrough {
		t.Fatalf("upstream passthrough = true, want false for a translated leg")
	}
}

// A pass-through leg: responses client served by the responses upstream.
func TestUsageReporterMarksPassThroughWhenProtocolsMatch(t *testing.T) {
	ctx := clientPathContext(t, "/v1/responses")
	reporter := NewExecutorUsageReporter(ctx, nil, "gpt-6.1-sol", nil)
	client := reporter.TrackHTTPClientRoundTripOnly(&http.Client{Transport: stubRoundTripper{}})

	doUpstream(t, client, "http://upstream.example/v1/responses")

	record := reporter.buildRecord(usage.Detail{InputTokens: 10}, false)
	if record.UpstreamProtocol != ProtocolResponses {
		t.Fatalf("upstream protocol = %q, want %q", record.UpstreamProtocol, ProtocolResponses)
	}
	if !record.UpstreamPassThrough {
		t.Fatalf("upstream passthrough = false, want true when both legs speak responses")
	}
}

// Without a recorded upstream request the record stays empty instead of guessing.
func TestUsageReporterLeavesUpstreamBlankWithoutRequest(t *testing.T) {
	ctx := clientPathContext(t, "/v1/chat/completions")
	reporter := NewExecutorUsageReporter(ctx, nil, "gpt-6.1-sol", nil)

	record := reporter.buildRecord(usage.Detail{InputTokens: 10}, false)
	if record.UpstreamProtocol != "" || record.UpstreamPassThrough {
		t.Fatalf("upstream = (%q, %v), want empty", record.UpstreamProtocol, record.UpstreamPassThrough)
	}
}

// Token refresh and quota calls share the tracked client; they must not erase the
// inference leg that actually served the response.
func TestUsageReporterIgnoresNonInferenceRequests(t *testing.T) {
	ctx := clientPathContext(t, "/v1/chat/completions")
	reporter := NewExecutorUsageReporter(ctx, nil, "gpt-6.1-sol", nil)
	client := reporter.TrackHTTPClientRoundTripOnly(&http.Client{Transport: stubRoundTripper{}})

	doUpstream(t, client, "http://upstream.example/v1/responses")
	doUpstream(t, client, "https://auth.example/oauth/token")

	record := reporter.buildRecord(usage.Detail{InputTokens: 10}, false)
	if record.UpstreamProtocol != ProtocolResponses {
		t.Fatalf("upstream protocol = %q, want %q", record.UpstreamProtocol, ProtocolResponses)
	}
}

// The plugin executor reports through a shared holder that survives the context
// child the host HTTP bridge runs on.
func TestUpstreamURLStateCrossesDerivedContext(t *testing.T) {
	base := clientPathContext(t, "/v1/chat/completions")
	state := &UpstreamURLState{}
	execCtx := WithUpstreamURLState(base, state)

	reporter := NewExecutorUsageReporter(execCtx, nil, "gpt-6.1-tra", nil)

	// The plugin callback runs on a context derived inside the host bridge.
	bridgeCtx, cancel := context.WithCancel(execCtx)
	defer cancel()
	RecordUpstreamURLString(bridgeCtx, "http://upstream.example/v1/responses")

	if got := state.URL(); got != "http://upstream.example/v1/responses" {
		t.Fatalf("state url = %q", got)
	}
	record := reporter.buildRecord(usage.Detail{InputTokens: 10}, false)
	if record.UpstreamProtocol != ProtocolResponses {
		t.Fatalf("upstream protocol = %q, want %q", record.UpstreamProtocol, ProtocolResponses)
	}
	if record.UpstreamPassThrough {
		t.Fatalf("upstream passthrough = true, want false for a translated leg")
	}
}
