package helps

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
)

// Wire protocol names recorded on usage events. They mirror the API families CPA
// serves on both the client-facing and the upstream leg.
const (
	ProtocolChatCompletions = "chat-completions"
	ProtocolResponses       = "responses"
	ProtocolMessages        = "messages"
	ProtocolGemini          = "gemini"
	ProtocolCompletions     = "completions"
)

// ProtocolFromPath maps a request path (client-facing or upstream) to the wire
// protocol spoken on that leg. Unknown paths yield an empty string so callers can
// tell "not determined" from a resolved protocol.
func ProtocolFromPath(path string) string {
	normalized := strings.ToLower(strings.TrimSpace(path))
	if normalized == "" {
		return ""
	}
	if cut := strings.IndexAny(normalized, "?#"); cut >= 0 {
		normalized = normalized[:cut]
	}
	switch {
	case strings.Contains(normalized, "/chat/completions"):
		return ProtocolChatCompletions
	case strings.Contains(normalized, "/responses"):
		return ProtocolResponses
	case strings.Contains(normalized, "/messages"):
		return ProtocolMessages
	case strings.Contains(normalized, "generatecontent"):
		return ProtocolGemini
	case strings.Contains(normalized, "/completions"):
		return ProtocolCompletions
	}
	return ""
}

// ClientRequestPath returns the inbound request path of the execution that owns
// ctx. It reads the gin context the API layer already stores on ctx, which is the
// same source requestExecutionMetadata uses for the request_path metadata key.
func ClientRequestPath(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	ginCtx, ok := ctx.Value("gin").(*gin.Context)
	if !ok || ginCtx == nil || ginCtx.Request == nil {
		return ""
	}
	if fullPath := strings.TrimSpace(ginCtx.FullPath()); fullPath != "" {
		return fullPath
	}
	if ginCtx.Request.URL != nil {
		return strings.TrimSpace(ginCtx.Request.URL.Path)
	}
	return ""
}

// UpstreamURLState carries the concrete upstream request URL of one execution.
// Native executors record it through the reporter's round tripper; plugin
// executors record it from the host HTTP callback, which runs on a different
// goroutine chain than the executor call, hence the shared holder.
type UpstreamURLState struct {
	mu  sync.RWMutex
	url string
}

// Set records the upstream URL of the most recent attempt. Retries overwrite it,
// matching the last leg that actually served the response.
func (s *UpstreamURLState) Set(url string) {
	if s == nil {
		return
	}
	url = strings.TrimSpace(url)
	if url == "" {
		return
	}
	s.mu.Lock()
	s.url = url
	s.mu.Unlock()
}

// URL returns the recorded upstream URL, or an empty string when none was seen.
func (s *UpstreamURLState) URL() string {
	if s == nil {
		return ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.url
}

type upstreamURLStateKey struct{}

// WithUpstreamURLState attaches a per-execution upstream URL holder to ctx. The
// plugin executor path relies on this: its HTTP bridge cannot see the reporter.
func WithUpstreamURLState(ctx context.Context, state *UpstreamURLState) context.Context {
	if state == nil {
		return ctx
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, upstreamURLStateKey{}, state)
}

// UpstreamURLStateFromContext returns the holder attached by WithUpstreamURLState.
func UpstreamURLStateFromContext(ctx context.Context) *UpstreamURLState {
	if ctx == nil {
		return nil
	}
	state, _ := ctx.Value(upstreamURLStateKey{}).(*UpstreamURLState)
	return state
}

// RecordUpstreamURL stores the upstream URL of an HTTP request into the holder
// carried by its context, when present. Callers use it from any layer that sees
// the outgoing request (round tripper, plugin host bridge).
func RecordUpstreamURL(ctx context.Context, req *http.Request) {
	if req == nil || req.URL == nil {
		return
	}
	recordInferenceURL(UpstreamURLStateFromContext(ctx), req.URL.Path, req.URL.String())
}

// RecordUpstreamURLString stores a raw upstream URL into the holder carried by
// ctx. The plugin host bridge sees the plugin's outgoing request as a decoded
// struct rather than an *http.Request, so it passes the URL directly.
func RecordUpstreamURLString(ctx context.Context, rawURL string) {
	recordInferenceURL(UpstreamURLStateFromContext(ctx), urlPathOf(rawURL), rawURL)
}

// recordInferenceURL keeps the holder to inference endpoints only. The same HTTP
// client also serves token refresh, catalog and quota calls whose paths carry no
// protocol; letting them through would erase the leg that served the response.
func recordInferenceURL(state *UpstreamURLState, path, rawURL string) {
	if state == nil {
		return
	}
	if ProtocolFromPath(path) == "" && ProtocolFromPath(rawURL) == "" {
		return
	}
	state.Set(rawURL)
}

func urlPathOf(rawURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return rawURL
	}
	return parsed.Path
}
