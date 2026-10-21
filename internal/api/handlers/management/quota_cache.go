package management

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	log "github.com/sirupsen/logrus"
)

const (
	// quotaDefaultDataDir matches the plugin data directory inside the image.
	quotaDefaultDataDir = "/CLIProxyAPI/data"
	// quotaSnapshotsFileName holds the newest plugin quota snapshot per credential.
	quotaSnapshotsFileName = "quota-snapshots.json"
)

// quotaSnapshot is one credential's most recent quota result.
type quotaSnapshot struct {
	PluginID  string                       `json:"plugin_id"`
	AuthIndex string                       `json:"auth_index"`
	Provider  string                       `json:"provider"`
	FetchedAt time.Time                    `json:"fetched_at"`
	Error     string                       `json:"error,omitempty"`
	Quota     pluginapi.QuotaFetchResponse `json:"quota"`
}

// quotaAutoRefreshState holds the persisted quota caches served by the management API.
// The historical name is kept to avoid touching every cache helper; the scheduled
// refresher itself now lives in Keeper.
type quotaAutoRefreshState struct {
	mu        sync.RWMutex
	snapshots map[string]quotaSnapshot
	// apiCache 缓存所有 provider 的配额类 api-call 响应（见 quota_api_cache.go）。
	apiCache map[string]quotaAPICacheEntry
}

// quotaDataDir resolves the directory holding quota caches.
func quotaDataDir() string {
	if custom := strings.TrimSpace(os.Getenv("QUOTA_AUTO_REFRESH_CONFIG")); custom != "" {
		return filepath.Dir(custom)
	}
	dataDir := strings.TrimSpace(os.Getenv("CPA_DATA_DIR"))
	if dataDir == "" {
		dataDir = quotaDefaultDataDir
	}
	return dataDir
}

// LoadQuotaCaches restores persisted snapshots and api-call responses at startup.
func (h *Handler) LoadQuotaCaches() {
	if h == nil {
		return
	}
	h.loadQuotaSnapshotsCache()
	h.loadQuotaAPICache()
}

// cachedQuotaSnapshotFor returns the cached snapshot for a credential. It is served
// regardless of whether any refresher is running: a page load must not become an
// upstream query just because the cache is cold.
func (h *Handler) cachedQuotaSnapshotFor(authIndex, provider, pluginID string) (quotaSnapshot, bool) {
	if h == nil {
		return quotaSnapshot{}, false
	}
	key := strings.TrimSpace(authIndex)
	if key == "" {
		return quotaSnapshot{}, false
	}
	h.quotaAutoRefresh.mu.RLock()
	defer h.quotaAutoRefresh.mu.RUnlock()
	snapshot, ok := h.quotaAutoRefresh.snapshots[key]
	if !ok || snapshot.Error != "" {
		return quotaSnapshot{}, false
	}
	if pluginID != "" && snapshot.PluginID != pluginID {
		return quotaSnapshot{}, false
	}
	if provider != "" && !strings.EqualFold(snapshot.Provider, provider) {
		return quotaSnapshot{}, false
	}
	return snapshot, true
}

// storeQuotaSnapshot 把一次成功的手动刷新结果写回共享快照，让其他页面/重启后读到同一份数据。
func (h *Handler) storeQuotaSnapshot(snapshot quotaSnapshot) {
	if h == nil || strings.TrimSpace(snapshot.AuthIndex) == "" {
		return
	}
	snapshot.FetchedAt = time.Now().UTC()
	h.quotaAutoRefresh.mu.Lock()
	if h.quotaAutoRefresh.snapshots == nil {
		h.quotaAutoRefresh.snapshots = make(map[string]quotaSnapshot)
	}
	h.quotaAutoRefresh.snapshots[snapshot.AuthIndex] = snapshot
	fresh := make(map[string]quotaSnapshot, len(h.quotaAutoRefresh.snapshots))
	for key, value := range h.quotaAutoRefresh.snapshots {
		fresh[key] = value
	}
	h.quotaAutoRefresh.mu.Unlock()
	saveQuotaSnapshotsCache(fresh)
}

// quotaSnapshotsPath resolves the file holding the newest snapshots (survives restarts).
func quotaSnapshotsPath() string {
	return filepath.Join(quotaDataDir(), quotaSnapshotsFileName)
}

// saveQuotaSnapshotsCache writes the newest snapshots atomically (overwrite, no history).
func saveQuotaSnapshotsCache(snapshots map[string]quotaSnapshot) {
	path := quotaSnapshotsPath()
	if errMkdir := os.MkdirAll(filepath.Dir(path), 0o755); errMkdir != nil {
		return
	}
	payload, errMarshal := json.Marshal(snapshots)
	if errMarshal != nil {
		return
	}
	tmp := path + ".tmp"
	if errWrite := os.WriteFile(tmp, append(payload, '\n'), 0o644); errWrite == nil {
		os.Rename(tmp, path)
	}
}

// loadQuotaSnapshotsCache restores snapshots saved by a previous run.
func (h *Handler) loadQuotaSnapshotsCache() {
	data, errRead := os.ReadFile(quotaSnapshotsPath())
	if errRead != nil {
		return
	}
	var parsed map[string]quotaSnapshot
	if errUnmarshal := json.Unmarshal(data, &parsed); errUnmarshal != nil {
		return
	}
	h.quotaAutoRefresh.mu.Lock()
	h.quotaAutoRefresh.snapshots = parsed
	h.quotaAutoRefresh.mu.Unlock()
	log.Infof("quota cache: restored %d snapshot(s) from disk", len(parsed))
}
