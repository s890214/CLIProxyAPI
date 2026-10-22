package helps

import (
	"context"
	"strings"
	"sync"
)

// UpstreamURLState carries the concrete upstream request URL of one execution.
// The plugin HTTP bridge fills it while the executor usage record is built by
// the same goroutine chain, so a pointer in the context is enough: deriving a
// new context inside the bridge would not reach the reporter.
type UpstreamURLState struct {
	mu  sync.RWMutex
	url string
}

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

func (s *UpstreamURLState) URL() string {
	if s == nil {
		return ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.url
}

type upstreamURLStateKey struct{}

// WithUpstreamURLState attaches a mutable holder for the exact upstream URL.
func WithUpstreamURLState(ctx context.Context) (context.Context, *UpstreamURLState) {
	state := &UpstreamURLState{}
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, upstreamURLStateKey{}, state), state
}

// UpstreamURLStateFromContext returns the holder set by WithUpstreamURLState.
func UpstreamURLStateFromContext(ctx context.Context) *UpstreamURLState {
	if ctx == nil {
		return nil
	}
	state, _ := ctx.Value(upstreamURLStateKey{}).(*UpstreamURLState)
	return state
}

// NoteUpstreamURL records the URL of the upstream request about to be issued.
func NoteUpstreamURL(ctx context.Context, url string) {
	UpstreamURLStateFromContext(ctx).Set(url)
}
