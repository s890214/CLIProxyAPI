package management

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	log "github.com/sirupsen/logrus"
)

const (
	quotaAutoRefreshDefaultInterval = 5 * time.Minute
	quotaAutoRefreshMinInterval     = time.Minute
	quotaAutoRefreshMaxInterval     = 60 * time.Minute
	// quotaAutoRefreshFetchTimeout bounds a single credential's upstream quota call so a
	// hung upstream cannot stall the whole sweep.
	quotaAutoRefreshFetchTimeout = 30 * time.Second
	// quotaAutoRefreshConfigFile is the standalone settings file. It deliberately lives
	// outside config.yaml so toggling switches never rewrites the main configuration.
	quotaAutoRefreshConfigFile = "quota-auto-refresh.json"
	// quotaAutoRefreshDefaultDataDir matches the plugin data directory inside the image.
	quotaAutoRefreshDefaultDataDir = "/CLIProxyAPI/data"
)

// quotaAutoRefreshSettings is persisted as a standalone JSON document.
type quotaAutoRefreshSettings struct {
	// Enabled is the global switch. When false the per-credential switches are ignored.
	Enabled bool `json:"enabled"`
	// Interval accepts Go duration syntax (5m, 90s). Clamped to 1m-60m at runtime.
	Interval string `json:"interval,omitempty"`
	// DisabledIndices lists auth indexes excluded from automatic refresh.
	DisabledIndices []string  `json:"disabled_indices,omitempty"`
	UpdatedAt       time.Time `json:"updated_at,omitempty"`
}

// quotaSnapshot is one credential's most recent background quota result.
type quotaSnapshot struct {
	PluginID  string                       `json:"plugin_id"`
	AuthIndex string                       `json:"auth_index"`
	Provider  string                       `json:"provider"`
	FetchedAt time.Time                    `json:"fetched_at"`
	Error     string                       `json:"error,omitempty"`
	Quota     pluginapi.QuotaFetchResponse `json:"quota"`
}

// quotaAutoRefreshState holds the in-memory settings and snapshots for the refresher.
type quotaAutoRefreshState struct {
	mu        sync.RWMutex
	settings  quotaAutoRefreshSettings
	snapshots map[string]quotaSnapshot
	// apiCache 缓存所有 provider 的配额类 api-call 响应（见 quota_api_cache.go）。
	apiCache  map[string]quotaAPICacheEntry
	lastRun   time.Time
	lastError string
	skipped   bool
}

// quotaAutoRefreshPath resolves where the settings file lives.
func quotaAutoRefreshPath() string {
	if custom := strings.TrimSpace(os.Getenv("QUOTA_AUTO_REFRESH_CONFIG")); custom != "" {
		return custom
	}
	dataDir := strings.TrimSpace(os.Getenv("CPA_DATA_DIR"))
	if dataDir == "" {
		dataDir = quotaAutoRefreshDefaultDataDir
	}
	return filepath.Join(dataDir, quotaAutoRefreshConfigFile)
}

// loadQuotaAutoRefreshSettings reads the settings file, falling back to defaults when it
// is missing or unreadable.
func loadQuotaAutoRefreshSettings() quotaAutoRefreshSettings {
	settings := quotaAutoRefreshSettings{Enabled: false, Interval: quotaAutoRefreshDefaultInterval.String()}
	raw, errRead := os.ReadFile(quotaAutoRefreshPath())
	if errRead != nil {
		return settings
	}
	var parsed quotaAutoRefreshSettings
	if errUnmarshal := json.Unmarshal(raw, &parsed); errUnmarshal != nil {
		log.Warnf("quota auto refresh: failed to parse %s: %v", quotaAutoRefreshPath(), errUnmarshal)
		return settings
	}
	if strings.TrimSpace(parsed.Interval) == "" {
		parsed.Interval = quotaAutoRefreshDefaultInterval.String()
	}
	return parsed
}

// saveQuotaAutoRefreshSettings writes the settings file atomically.
func saveQuotaAutoRefreshSettings(settings quotaAutoRefreshSettings) error {
	path := quotaAutoRefreshPath()
	if errMkdir := os.MkdirAll(filepath.Dir(path), 0o755); errMkdir != nil {
		return errMkdir
	}
	settings.UpdatedAt = time.Now().UTC()
	payload, errMarshal := json.MarshalIndent(settings, "", "  ")
	if errMarshal != nil {
		return errMarshal
	}
	tmp := path + ".tmp"
	if errWrite := os.WriteFile(tmp, append(payload, '\n'), 0o644); errWrite != nil {
		return errWrite
	}
	return os.Rename(tmp, path)
}

// StartQuotaAutoRefresh launches the background quota refresher and loads persisted settings.
func (h *Handler) StartQuotaAutoRefresh(ctx context.Context) {
	if h == nil || ctx == nil {
		return
	}
	settings := loadQuotaAutoRefreshSettings()
	h.quotaAutoRefresh.mu.Lock()
	h.quotaAutoRefresh.settings = settings
	if h.quotaAutoRefresh.snapshots == nil {
		h.quotaAutoRefresh.snapshots = make(map[string]quotaSnapshot)
	}
	h.quotaAutoRefresh.mu.Unlock()
	// Restore the last snapshots so a container restart doesn't blank the panel.
	h.loadQuotaSnapshotsCache()
	h.loadQuotaAPICache()
	go h.quotaAutoRefreshLoop(ctx)
}

func (h *Handler) quotaAutoRefreshLoop(ctx context.Context) {
	// Give the plugin host a moment to finish registration before the first sweep.
	if !h.sleepQuotaAutoRefresh(ctx, 15*time.Second) {
		return
	}
	for {
		h.runQuotaAutoRefreshOnce(ctx, false)
		if !h.sleepQuotaAutoRefresh(ctx, h.quotaAutoRefreshInterval()) {
			return
		}
	}
}

// sleepQuotaAutoRefresh waits for d and reports whether the context is still alive.
func (h *Handler) sleepQuotaAutoRefresh(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// MarkRequestActivity records that a proxied API request just happened. Called from a
// middleware for /v1 traffic only, so opening the dashboard does not count as activity.
func (h *Handler) MarkRequestActivity() {
	if h == nil {
		return
	}
	h.lastRequestAt.Store(time.Now().Unix())
}

// quotaAutoRefreshSettingsSnapshot returns a copy of the current settings.
func (h *Handler) quotaAutoRefreshSettingsSnapshot() quotaAutoRefreshSettings {
	h.quotaAutoRefresh.mu.RLock()
	defer h.quotaAutoRefresh.mu.RUnlock()
	return h.quotaAutoRefresh.settings
}

// quotaAutoRefreshInterval resolves the configured interval, clamped to a sane range.
func (h *Handler) quotaAutoRefreshInterval() time.Duration {
	raw := strings.TrimSpace(h.quotaAutoRefreshSettingsSnapshot().Interval)
	if raw == "" {
		return quotaAutoRefreshDefaultInterval
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil || parsed <= 0 {
		return quotaAutoRefreshDefaultInterval
	}
	if parsed < quotaAutoRefreshMinInterval {
		return quotaAutoRefreshMinInterval
	}
	if parsed > quotaAutoRefreshMaxInterval {
		return quotaAutoRefreshMaxInterval
	}
	return parsed
}

// runQuotaAutoRefreshOnce sweeps every credential that has a quota-capable plugin.
// manual=true bypasses both the activity check and the global switch gate is still honored.
func (h *Handler) runQuotaAutoRefreshOnce(ctx context.Context, manual bool) {
	settings := h.quotaAutoRefreshSettingsSnapshot()
	if !settings.Enabled {
		return
	}

	h.quotaAutoRefresh.mu.RLock()
	lastRun := h.quotaAutoRefresh.lastRun
	h.quotaAutoRefresh.mu.RUnlock()

	// Activity gate: skip the sweep when no proxied request arrived since the last run.
	// Manual refreshes always go through.
	if !manual && !lastRun.IsZero() {
		if h.lastRequestAt.Load() <= lastRun.Unix() {
			h.quotaAutoRefresh.mu.Lock()
			h.quotaAutoRefresh.skipped = true
			h.quotaAutoRefresh.lastError = ""
			h.quotaAutoRefresh.mu.Unlock()
			log.Debug("quota auto refresh skipped: no request activity since last run")
			return
		}
	}

	h.mu.Lock()
	host := h.pluginHost
	manager := h.authManager
	h.mu.Unlock()

	if manager == nil {
		h.recordQuotaAutoRefreshRun("auth manager unavailable")
		return
	}

	// 插件不是必需的：没有配额插件时，原生 provider 仍然通过 api-call 缓存预热。
	pluginIDs := map[string]string{}
	if host != nil {
		pluginIDs = host.QuotaProviderPluginIDs(ctx)
	}

	disabled := make(map[string]struct{}, len(settings.DisabledIndices))
	for _, idx := range settings.DisabledIndices {
		if key := strings.TrimSpace(idx); key != "" {
			disabled[key] = struct{}{}
		}
	}

	fresh := make(map[string]quotaSnapshot)
	var firstErr string

	for _, auth := range manager.List() {
		if auth == nil {
			continue
		}
		auth.EnsureIndex()
		index := strings.TrimSpace(auth.Index)
		if index == "" {
			continue
		}
		if _, skip := disabled[index]; skip {
			continue
		}
		pluginID := pluginIDs[strings.ToLower(strings.TrimSpace(auth.Provider))]
		if pluginID != "" {
			snapshot := quotaSnapshot{
				PluginID:  pluginID,
				AuthIndex: index,
				Provider:  auth.Provider,
				FetchedAt: time.Now().UTC(),
			}

			fetchCtx, cancel := context.WithTimeout(ctx, quotaAutoRefreshFetchTimeout)
			resp, handled, errFetch := host.FetchQuotaByPlugin(fetchCtx, pluginID, pluginapi.QuotaFetchRequest{
				AuthIndex:  auth.Index,
				AuthID:     auth.ID,
				Provider:   auth.Provider,
				Metadata:   auth.Metadata,
				Attributes: auth.Attributes,
			})
			cancel()

			switch {
			case !handled:
				snapshot.Error = "quota provider not found for plugin"
			case errFetch != nil:
				snapshot.Error = errFetch.Error()
				if firstErr == "" {
					firstErr = errFetch.Error()
				}
			default:
				snapshot.Quota = resp
			}
			fresh[index] = snapshot
		}

		// 非插件 provider（antigravity/claude/codex/kimi/xai/gemini 等）走
		// api-call 缓存：重放该凭证已学到的配额请求，保持与其他页面同一份数据。
		if errWarm := h.warmQuotaAPICache(ctx, index); errWarm != "" && firstErr == "" {
			firstErr = errWarm
		}
	}

	h.quotaAutoRefresh.mu.Lock()
	h.quotaAutoRefresh.snapshots = fresh
	h.quotaAutoRefresh.lastRun = time.Now().UTC()
	h.quotaAutoRefresh.skipped = false
	h.quotaAutoRefresh.lastError = firstErr
	h.quotaAutoRefresh.mu.Unlock()

	saveQuotaSnapshotsCache(fresh)
	log.Infof("quota auto refresh finished: %d credential(s), manual=%t", len(fresh), manual)
}

func (h *Handler) recordQuotaAutoRefreshRun(errMsg string) {
	h.quotaAutoRefresh.mu.Lock()
	h.quotaAutoRefresh.lastRun = time.Now().UTC()
	h.quotaAutoRefresh.lastError = errMsg
	h.quotaAutoRefresh.mu.Unlock()
}

// GetQuotaAutoRefresh handles GET /quota/auto-refresh.
func (h *Handler) GetQuotaAutoRefresh(c *gin.Context) {
	cfg := h.quotaAutoRefreshSettingsSnapshot()
	// Ensure JSON serializes as [] rather than null for an empty list.
	disabledIndices := cfg.DisabledIndices
	if disabledIndices == nil {
		disabledIndices = []string{}
	}

	h.quotaAutoRefresh.mu.RLock()
	snapshots := make([]quotaSnapshot, 0, len(h.quotaAutoRefresh.snapshots))
	for _, snapshot := range h.quotaAutoRefresh.snapshots {
		snapshots = append(snapshots, snapshot)
	}
	lastRun := h.quotaAutoRefresh.lastRun
	lastError := h.quotaAutoRefresh.lastError
	skipped := h.quotaAutoRefresh.skipped
	h.quotaAutoRefresh.mu.RUnlock()

	c.JSON(http.StatusOK, gin.H{
		"enabled":           cfg.Enabled,
		"interval":          h.quotaAutoRefreshInterval().String(),
		"disabled_indices":  disabledIndices,
		"last_run":          lastRun,
		"last_error":        lastError,
		"skipped":           skipped,
		"last_request_at":   h.lastRequestAt.Load(),
		"activity_required": true,
		"config_path":       quotaAutoRefreshPath(),
		"snapshots":         snapshots,
	})
}

// PutQuotaAutoRefresh handles PUT/PATCH /quota/auto-refresh.
// Omitted fields keep their current value.
func (h *Handler) PutQuotaAutoRefresh(c *gin.Context) {
	var body struct {
		Enabled         *bool     `json:"enabled"`
		Interval        *string   `json:"interval"`
		DisabledIndices *[]string `json:"disabled_indices"`
	}
	if errBind := c.ShouldBindJSON(&body); errBind != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	next := h.quotaAutoRefreshSettingsSnapshot()
	if body.Enabled != nil {
		next.Enabled = *body.Enabled
	}
	if body.Interval != nil {
		raw := strings.TrimSpace(*body.Interval)
		if raw != "" {
			parsed, errParse := time.ParseDuration(raw)
			if errParse != nil || parsed <= 0 {
				c.JSON(http.StatusBadRequest, gin.H{"error": "interval must be a positive duration such as 5m"})
				return
			}
		}
		next.Interval = raw
	}
	if body.DisabledIndices != nil {
		cleaned := make([]string, 0, len(*body.DisabledIndices))
		seen := make(map[string]struct{}, len(*body.DisabledIndices))
		for _, idx := range *body.DisabledIndices {
			key := strings.TrimSpace(idx)
			if key == "" {
				continue
			}
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			cleaned = append(cleaned, key)
		}
		next.DisabledIndices = cleaned
	}

	if errSave := saveQuotaAutoRefreshSettings(next); errSave != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save settings: " + errSave.Error()})
		return
	}

	h.quotaAutoRefresh.mu.Lock()
	h.quotaAutoRefresh.settings = next
	h.quotaAutoRefresh.mu.Unlock()

	h.GetQuotaAutoRefresh(c)
}

// PostQuotaAutoRefreshNow handles POST /quota/auto-refresh/now.
// It forces a sweep regardless of the activity gate (used by the manual refresh button).
func (h *Handler) PostQuotaAutoRefreshNow(c *gin.Context) {
	go h.runQuotaAutoRefreshOnce(context.Background(), true)
	c.JSON(http.StatusAccepted, gin.H{"status": "started"})
}

// cachedQuotaSnapshotFor returns the background snapshot for a credential when the
// auto refresher is enabled and holds a successful, matching result. Matching is by
// auth index (unique per credential), optionally narrowed by provider/plugin.
func (h *Handler) cachedQuotaSnapshotFor(authIndex, provider, pluginID string) (quotaSnapshot, bool) {
	if h == nil {
		return quotaSnapshot{}, false
	}
	settings := h.quotaAutoRefreshSettingsSnapshot()
	if !settings.Enabled {
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


// quotaSnapshotsPath resolves the file holding the newest snapshots (survives restarts).
func quotaSnapshotsPath() string {
	return strings.TrimSuffix(quotaAutoRefreshPath(), filepath.Base(quotaAutoRefreshPath())) + "quota-snapshots.json"
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
	log.Infof("quota auto refresh: restored %d snapshot(s) from disk", len(parsed))
}
