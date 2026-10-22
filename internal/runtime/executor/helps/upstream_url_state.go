package helps

import (
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
