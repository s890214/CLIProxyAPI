package redisqueue

import (
	"net/url"
	"strings"
)

// upstreamProtocolFromURL maps an upstream request URL to the wire protocol it
// used. Empty when the URL is unknown or the path is not a model endpoint.
func upstreamProtocolFromURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return protocolFromPath(parsed.Path)
}

// protocolFromEndpoint maps the inbound endpoint ("POST /v1/responses") to the
// same protocol vocabulary so pass-through can be decided by comparison.
func protocolFromEndpoint(endpoint string) string {
	fields := strings.Fields(endpoint)
	if len(fields) == 0 {
		return ""
	}
	return protocolFromPath(fields[len(fields)-1])
}

func protocolFromPath(path string) string {
	cleaned := strings.ToLower(strings.TrimSpace(path))
	cleaned = strings.TrimRight(cleaned, "/")
	switch {
	case strings.HasSuffix(cleaned, "/chat/completions"):
		return "chat-completions"
	case strings.HasSuffix(cleaned, "/responses"):
		return "responses"
	case strings.HasSuffix(cleaned, "/messages"):
		return "messages"
	}
	return ""
}
