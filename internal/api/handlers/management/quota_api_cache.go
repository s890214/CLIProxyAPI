package management

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// 配额类上游请求的统一缓存。
//
// 面板（配额页、认证文件页）与 Keeper 的额度查询最终都走 CPA 的
// /v8/management/requests/api-call。在这里按「凭证 + 方法 + URL + 请求体」缓存响应，
// 三个页面就自然共用同一份数据，不再各查各的；后台自动刷新任务按学到的请求模板
// 定期重放，把缓存维持新鲜。手动刷新带 X-CPA-Quota-Force: 1 穿透缓存真查上游。
const (
	quotaAPICacheFileName    = "quota-api-cache.json"
	quotaAPICacheForceHeader = "X-CPA-Quota-Force"
	quotaAPICacheMaxBody     = 1 << 20
	quotaAPICacheMaxEntries  = 500
)

// quotaAPIURLMarkers 限定可缓存的端点，避免把任意 api-call（可能有副作用）纳入重放。
var quotaAPIURLMarkers = []string{
	"v1internal:retrieveUserQuotaSummary",
	"v1internal:retrieveUserQuota",
	"v1internal:loadCodeAssist",
	"/api/oauth/usage",
	"/api/oauth/profile",
	"backend-api/wham/usage",
	"/coding/v1/usages",
	"/v1/billing",
}

func isQuotaAPIURL(rawURL string) bool {
	lower := strings.ToLower(rawURL)
	for _, marker := range quotaAPIURLMarkers {
		if strings.Contains(lower, strings.ToLower(marker)) {
			return true
		}
	}
	return false
}

// quotaAPICacheEntry 同时保存「请求模板」与「最近一次响应」，供缓存命中与后台重放使用。
type quotaAPICacheEntry struct {
	AuthIndex  string              `json:"auth_index"`
	Method     string              `json:"method"`
	URL        string              `json:"url"`
	Header     map[string]string   `json:"header,omitempty"`
	Data       string              `json:"data,omitempty"`
	Status     int                 `json:"status"`
	RespHeader map[string][]string `json:"resp_header,omitempty"`
	Body       string              `json:"body"`
	FetchedAt  time.Time           `json:"fetched_at"`
}

func quotaAPICacheKey(authIndex, method, rawURL, data string) string {
	sum := sha256.Sum256([]byte(data))
	return strings.Join([]string{
		strings.TrimSpace(authIndex),
		strings.ToUpper(strings.TrimSpace(method)),
		strings.TrimSpace(rawURL),
		hex.EncodeToString(sum[:8]),
	}, "\x00")
}

func quotaAPICachePath() string {
	return filepath.Join(quotaDataDir(), quotaAPICacheFileName)
}

// sanitizeQuotaHeaders 只保留可以安全落盘的请求头：占位符 $TOKEN$ 或非 Bearer 常量。
func sanitizeQuotaHeaders(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for key, value := range in {
		if len(value) > 4096 {
			continue
		}
		if strings.Contains(strings.ToLower(value), "bearer ") && !strings.Contains(value, "$TOKEN$") {
			continue
		}
		out[key] = value
	}
	return out
}

// lookupQuotaAPICache 返回缓存响应。缓存不设过期时间：只有后台自动刷新轮次
// 或手动强制刷新会覆盖它，因此刷新浏览器、隔天再进页面都还是同一份数据。
func (h *Handler) lookupQuotaAPICache(authIndex, method, rawURL, data string) (apiCallResponse, bool) {
	if h == nil || strings.TrimSpace(authIndex) == "" {
		return apiCallResponse{}, false
	}
	key := quotaAPICacheKey(authIndex, method, rawURL, data)
	h.quotaAutoRefresh.mu.RLock()
	entry, ok := h.quotaAutoRefresh.apiCache[key]
	h.quotaAutoRefresh.mu.RUnlock()
	if !ok {
		return apiCallResponse{}, false
	}
	if entry.Status < 200 || entry.Status >= 300 {
		return apiCallResponse{}, false
	}
	return apiCallResponse{StatusCode: entry.Status, Header: entry.RespHeader, Body: entry.Body}, true
}

// storeQuotaAPICache 写入/更新缓存并落盘。
func (h *Handler) storeQuotaAPICache(entry quotaAPICacheEntry) {
	if h == nil || strings.TrimSpace(entry.AuthIndex) == "" || !isQuotaAPIURL(entry.URL) {
		return
	}
	if entry.Status < 200 || entry.Status >= 300 {
		return
	}
	if len(entry.Body) > quotaAPICacheMaxBody {
		return
	}
	entry.FetchedAt = time.Now().UTC()
	entry.Header = sanitizeQuotaHeaders(entry.Header)
	key := quotaAPICacheKey(entry.AuthIndex, entry.Method, entry.URL, entry.Data)

	h.quotaAutoRefresh.mu.Lock()
	if h.quotaAutoRefresh.apiCache == nil {
		h.quotaAutoRefresh.apiCache = make(map[string]quotaAPICacheEntry)
	}
	h.quotaAutoRefresh.apiCache[key] = entry
	if len(h.quotaAutoRefresh.apiCache) > quotaAPICacheMaxEntries {
		// 简单裁剪：删掉最早的一条，保持文件不无限增长。
		var oldestKey string
		var oldest time.Time
		for k, v := range h.quotaAutoRefresh.apiCache {
			if oldestKey == "" || v.FetchedAt.Before(oldest) {
				oldestKey, oldest = k, v.FetchedAt
			}
		}
		delete(h.quotaAutoRefresh.apiCache, oldestKey)
	}
	snapshot := make(map[string]quotaAPICacheEntry, len(h.quotaAutoRefresh.apiCache))
	for k, v := range h.quotaAutoRefresh.apiCache {
		snapshot[k] = v
	}
	h.quotaAutoRefresh.mu.Unlock()

	h.saveQuotaAPICache(snapshot)
}

func (h *Handler) saveQuotaAPICache(entries map[string]quotaAPICacheEntry) {
	path := quotaAPICachePath()
	if errMkdir := os.MkdirAll(filepath.Dir(path), 0o755); errMkdir != nil {
		return
	}
	payload, errMarshal := json.MarshalIndent(entries, "", "  ")
	if errMarshal != nil {
		return
	}
	tmp := path + ".tmp"
	if errWrite := os.WriteFile(tmp, append(payload, '\n'), 0o644); errWrite != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

// loadQuotaAPICache 在启动时恢复缓存，保证容器重启后页面仍然秒开。
func (h *Handler) loadQuotaAPICache() {
	raw, errRead := os.ReadFile(quotaAPICachePath())
	if errRead != nil {
		return
	}
	var parsed map[string]quotaAPICacheEntry
	if errUnmarshal := json.Unmarshal(raw, &parsed); errUnmarshal != nil {
		log.Warnf("quota api cache: failed to parse %s: %v", quotaAPICachePath(), errUnmarshal)
		return
	}
	h.quotaAutoRefresh.mu.Lock()
	h.quotaAutoRefresh.apiCache = parsed
	h.quotaAutoRefresh.mu.Unlock()
	log.Infof("quota api cache: restored %d entr(ies) from disk", len(parsed))
}

// quotaAPICacheEntriesFor 返回某个凭证已学到的请求模板（每个端点只保留最新一条）。
func (h *Handler) quotaAPICacheEntriesFor(authIndex string) []quotaAPICacheEntry {
	authIndex = strings.TrimSpace(authIndex)
	if authIndex == "" {
		return nil
	}
	h.quotaAutoRefresh.mu.RLock()
	defer h.quotaAutoRefresh.mu.RUnlock()
	out := make([]quotaAPICacheEntry, 0, 4)
	for _, entry := range h.quotaAutoRefresh.apiCache {
		if entry.AuthIndex == authIndex {
			out = append(out, entry)
		}
	}
	return out
}

// callAPICallInternal 用进程内的 gin 上下文执行一次管理 API 的 api-call，
// 复用与面板完全相同的取 token / 代理 / 错误处理逻辑，不经过网络。
func (h *Handler) callAPICallInternal(ctx context.Context, body apiCallRequest) (apiCallResponse, error) {
	payload, errMarshal := json.Marshal(body)
	if errMarshal != nil {
		return apiCallResponse{}, errMarshal
	}
	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	request := httptest.NewRequest(http.MethodPost, "/v8/management/requests/api-call", bytes.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(quotaAPICacheForceHeader, "1")
	ginContext.Request = request.WithContext(ctx)
	h.APICall(ginContext)
	if recorder.Code != http.StatusOK {
		return apiCallResponse{}, fmt.Errorf("api-call failed with status %d", recorder.Code)
	}
	var response apiCallResponse
	if errUnmarshal := json.Unmarshal(recorder.Body.Bytes(), &response); errUnmarshal != nil {
		return apiCallResponse{}, errUnmarshal
	}
	return response, nil
}

// warmQuotaAPICache 重放某个凭证已学到的配额请求，刷新缓存。
func (h *Handler) warmQuotaAPICache(ctx context.Context, authIndex string) string {
	entries := h.quotaAPICacheEntriesFor(authIndex)
	if len(entries) == 0 {
		return ""
	}
	firstErr := ""
	for _, entry := range entries {
		index := authIndex
		body := apiCallRequest{
			AuthIndexSnake: &index,
			Method:         entry.Method,
			URL:            entry.URL,
			Header:         entry.Header,
			Data:           entry.Data,
		}
		response, errCall := h.callAPICallInternal(ctx, body)
		if errCall != nil {
			if firstErr == "" {
				firstErr = errCall.Error()
			}
			continue
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			if firstErr == "" {
				firstErr = fmt.Sprintf("upstream returned %d for %s", response.StatusCode, entry.URL)
			}
		}
	}
	return firstErr
}
