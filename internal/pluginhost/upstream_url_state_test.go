package pluginhost

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestHostHTTPClientRecordsUpstreamURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(server.Close)

	client := New().newHTTPClient(nil).(*hostHTTPClient)
	state := &helps.UpstreamURLState{}
	client.upstreamURL = state

	ctx := cliproxyexecutor.WithUpstreamAttemptTracker(context.Background())
	target := server.URL + "/v1/chat/completions"
	if _, errDo := client.Do(ctx, pluginapi.HTTPRequest{URL: target}); errDo != nil {
		t.Fatalf("Do: %v", errDo)
	}
	if got := state.URL(); got != target {
		t.Fatalf("state.URL() = %q, want %q", got, target)
	}

	// A later request on the same execution (retry) overwrites the value.
	target2 := server.URL + "/v1/responses"
	if _, errDo := client.Do(ctx, pluginapi.HTTPRequest{URL: target2}); errDo != nil {
		t.Fatalf("Do second: %v", errDo)
	}
	if got := state.URL(); got != target2 {
		t.Fatalf("state.URL() after retry = %q, want %q", got, target2)
	}
}

func TestBuildExecutorRequestAttachesUpstreamURLState(t *testing.T) {
	state := &helps.UpstreamURLState{}
	req := buildExecutorRequest(New(), "plugin-provider", nil, cliproxyexecutor.Request{}, cliproxyexecutor.Options{}, state)
	client, ok := req.HTTPClient.(*hostHTTPClient)
	if !ok {
		t.Fatalf("HTTPClient type = %T, want *hostHTTPClient", req.HTTPClient)
	}
	if client.upstreamURL != state {
		t.Fatal("upstream URL state not attached to the per-execution client")
	}
	// CountTokens and other callers pass no state and must stay nil-safe.
	if client := buildExecutorRequest(New(), "p", nil, cliproxyexecutor.Request{}, cliproxyexecutor.Options{}).HTTPClient.(*hostHTTPClient); client.upstreamURL != nil {
		t.Fatal("unexpected state on a request built without one")
	}
}
