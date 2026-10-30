package pluginhost

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
)

// The plugin executor reports the upstream leg through the host HTTP bridge,
// which runs on a context derived from the callback context. This test pins the
// property the whole design depends on: the URL holder attached to the executor
// context is still reachable from the operation context the bridge uses.
func TestCallbackOperationContextCarriesUpstreamURLState(t *testing.T) {
	h := New()
	state := &helps.UpstreamURLState{}
	execCtx := helps.WithUpstreamURLState(context.Background(), state)

	callbackID, closeCallback := h.openCallbackContextForPlugin(execCtx, "demo-plugin")
	defer closeCallback()
	if callbackID == "" {
		t.Fatal("openCallbackContextForPlugin returned an empty callback id")
	}

	handle, err := h.acquireHostHTTPOperation(execCtx, callbackID, "")
	if err != nil {
		t.Fatalf("acquireHostHTTPOperation: %v", err)
	}
	defer handle.finish()

	if got := helps.UpstreamURLStateFromContext(handle.ctx); got != state {
		t.Fatal("operation context lost the upstream URL state attached to the executor context")
	}

	helps.RecordUpstreamURLString(handle.ctx, "http://upstream.example/v1/responses")
	if got := state.URL(); got != "http://upstream.example/v1/responses" {
		t.Fatalf("state url = %q, want the recorded upstream url", got)
	}
}

// Without an attached holder the bridge must stay silent instead of panicking.
func TestCallbackOperationContextWithoutStateIsSafe(t *testing.T) {
	h := New()
	callbackID, closeCallback := h.openCallbackContextForPlugin(context.Background(), "demo-plugin")
	defer closeCallback()

	handle, err := h.acquireHostHTTPOperation(context.Background(), callbackID, "")
	if err != nil {
		t.Fatalf("acquireHostHTTPOperation: %v", err)
	}
	defer handle.finish()

	if got := helps.UpstreamURLStateFromContext(handle.ctx); got != nil {
		t.Fatalf("state = %#v, want nil", got)
	}
	helps.RecordUpstreamURLString(handle.ctx, "http://upstream.example/v1/responses")
}
