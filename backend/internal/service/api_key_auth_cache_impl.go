package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/dgraph-io/ristretto"
)

const apiKeyAuthSnapshotVersion = 17 // v17: key-scoped group unavailability + sticky pin

// DefaultKeyGroupUnavailabilityTTL is how long this key skips a group after zero-available.
const DefaultKeyGroupUnavailabilityTTL = 2 * time.Minute

type apiKeyAuthCacheConfig struct {
	l1Size        int
	l1TTL         time.Duration
	l2TTL         time.Duration
	negativeTTL   time.Duration
	jitterPercent int
	singleflight  bool
}

func newAPIKeyAuthCacheConfig(cfg *config.Config) apiKeyAuthCacheConfig {
	if cfg == nil {
		return apiKeyAuthCacheConfig{}
	}
	auth := cfg.APIKeyAuth
	return apiKeyAuthCacheConfig{
		l1Size:        auth.L1Size,
		l1TTL:         time.Duration(auth.L1TTLSeconds) * time.Second,
		l2TTL:         time.Duration(auth.L2TTLSeconds) * time.Second,
		negativeTTL:   time.Duration(auth.NegativeTTLSeconds) * time.Second,
		jitterPercent: auth.JitterPercent,
		singleflight:  auth.Singleflight,
	}
}

func (c apiKeyAuthCacheConfig) l1Enabled() bool {
	return c.l1Size > 0 && c.l1TTL > 0
}

func (c apiKeyAuthCacheConfig) l2Enabled() bool {
	return c.l2TTL > 0
}

func (c apiKeyAuthCacheConfig) negativeEnabled() bool {
	return c.negativeTTL > 0
}

// jitterTTL 为缓存 TTL 添加抖动，避免多个请求在同一时刻同时过期触发集中回源。
// 这里直接使用 rand/v2 的顶层函数：并发安全，无需全局互斥锁。
func (c apiKeyAuthCacheConfig) jitterTTL(ttl time.Duration) time.Duration {
	if ttl <= 0 {
		return ttl
	}
	if c.jitterPercent <= 0 {
		return ttl
	}
	percent := c.jitterPercent
	if percent > 100 {
		percent = 100
	}
	delta := float64(percent) / 100
	randVal := rand.Float64()
	factor := 1 - delta + randVal*(2*delta)
	if factor <= 0 {
		return ttl
	}
	return time.Duration(float64(ttl) * factor)
}

func (s *APIKeyService) initAuthCache(cfg *config.Config) {
	s.authCfg = newAPIKeyAuthCacheConfig(cfg)
	if !s.authCfg.l1Enabled() {
		return
	}
	cache, err := ristretto.NewCache(&ristretto.Config{
		NumCounters: int64(s.authCfg.l1Size) * 10,
		MaxCost:     int64(s.authCfg.l1Size),
		BufferItems: 64,
	})
	if err != nil {
		return
	}
	s.authCacheL1 = cache
}

// StartAuthCacheInvalidationSubscriber starts the Pub/Sub subscriber for L1 cache invalidation.
// This should be called after the service is fully initialized.
func (s *APIKeyService) StartAuthCacheInvalidationSubscriber(ctx context.Context) {
	if s.cache == nil || s.authCacheL1 == nil {
		return
	}
	if err := s.cache.SubscribeAuthCacheInvalidation(ctx, func(cacheKey string) {
		s.authCacheL1.Del(cacheKey)
	}); err != nil {
		// Log but don't fail - L1 cache will still work, just without cross-instance invalidation
		slog.Warn("failed to start auth cache invalidation subscriber", "error", err)
	}
}

func (s *APIKeyService) authCacheKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func (s *APIKeyService) getAuthCacheEntry(ctx context.Context, cacheKey string) (*APIKeyAuthCacheEntry, bool) {
	if s.authCacheL1 != nil {
		if val, ok := s.authCacheL1.Get(cacheKey); ok {
			if entry, ok := val.(*APIKeyAuthCacheEntry); ok {
				return entry, true
			}
		}
	}
	if s.cache == nil || !s.authCfg.l2Enabled() {
		return nil, false
	}
	entry, err := s.cache.GetAuthCache(ctx, cacheKey)
	if err != nil {
		return nil, false
	}
	s.setAuthCacheL1(cacheKey, entry)
	return entry, true
}

func (s *APIKeyService) setAuthCacheL1(cacheKey string, entry *APIKeyAuthCacheEntry) {
	if s.authCacheL1 == nil || entry == nil {
		return
	}
	ttl := s.authCfg.l1TTL
	if entry.NotFound && s.authCfg.negativeTTL > 0 && s.authCfg.negativeTTL < ttl {
		ttl = s.authCfg.negativeTTL
	}
	ttl = s.authCfg.jitterTTL(ttl)
	_ = s.authCacheL1.SetWithTTL(cacheKey, entry, 1, ttl)
}

func (s *APIKeyService) setAuthCacheEntry(ctx context.Context, cacheKey string, entry *APIKeyAuthCacheEntry, ttl time.Duration) {
	if entry == nil {
		return
	}
	s.setAuthCacheL1(cacheKey, entry)
	if s.cache == nil || !s.authCfg.l2Enabled() {
		return
	}
	_ = s.cache.SetAuthCache(ctx, cacheKey, entry, s.authCfg.jitterTTL(ttl))
}

func (s *APIKeyService) deleteAuthCache(ctx context.Context, cacheKey string) {
	if s.authCacheL1 != nil {
		s.authCacheL1.Del(cacheKey)
	}
	if s.cache == nil {
		return
	}
	_ = s.cache.DeleteAuthCache(ctx, cacheKey)
	// Publish invalidation message to other instances
	_ = s.cache.PublishAuthCacheInvalidation(ctx, cacheKey)
}

func (s *APIKeyService) loadAuthCacheEntry(ctx context.Context, key, cacheKey string) (*APIKeyAuthCacheEntry, error) {
	apiKey, err := s.apiKeyRepo.GetByKeyForAuth(ctx, key)
	if err != nil {
		if errors.Is(err, ErrAPIKeyNotFound) {
			entry := &APIKeyAuthCacheEntry{NotFound: true}
			if s.authCfg.negativeEnabled() {
				s.setAuthCacheEntry(ctx, cacheKey, entry, s.authCfg.negativeTTL)
			}
			return entry, nil
		}
		return nil, fmt.Errorf("get api key: %w", err)
	}
	apiKey.Key = key
	snapshot := s.snapshotFromAPIKey(ctx, apiKey)
	if snapshot == nil {
		return nil, fmt.Errorf("get api key: %w", ErrAPIKeyNotFound)
	}
	entry := &APIKeyAuthCacheEntry{Snapshot: snapshot}
	s.setAuthCacheEntry(ctx, cacheKey, entry, s.authCfg.l2TTL)
	return entry, nil
}

func (s *APIKeyService) applyAuthCacheEntry(key string, entry *APIKeyAuthCacheEntry) (*APIKey, bool, error) {
	if entry == nil {
		return nil, false, nil
	}
	if entry.NotFound {
		return nil, true, ErrAPIKeyNotFound
	}
	if entry.Snapshot == nil {
		return nil, false, nil
	}
	if entry.Snapshot.Version != apiKeyAuthSnapshotVersion {
		return nil, false, nil
	}
	return s.snapshotToAPIKey(key, entry.Snapshot), true, nil
}


func groupToAuthSnapshot(g *Group) *APIKeyAuthGroupSnapshot {
	if g == nil {
		return nil
	}
	return &APIKeyAuthGroupSnapshot{
		ID:                              g.ID,
		Name:                            g.Name,
		Platform:                        g.Platform,
		IsExclusive:                     g.IsExclusive,
		Status:                          g.Status,
		SubscriptionType:                g.SubscriptionType,
		RateMultiplier:                  g.RateMultiplier,
		DailyLimitUSD:                   g.DailyLimitUSD,
		WeeklyLimitUSD:                  g.WeeklyLimitUSD,
		MonthlyLimitUSD:                 g.MonthlyLimitUSD,
		AllowImageGeneration:            g.AllowImageGeneration,
		AllowBatchImageGeneration:       g.AllowBatchImageGeneration,
		ImageRateIndependent:            g.ImageRateIndependent,
		ImageRateMultiplier:             g.ImageRateMultiplier,
		ImagePrice1K:                    g.ImagePrice1K,
		ImagePrice2K:                    g.ImagePrice2K,
		ImagePrice4K:                    g.ImagePrice4K,
		VideoRateIndependent:            g.VideoRateIndependent,
		VideoRateMultiplier:             g.VideoRateMultiplier,
		VideoPrice480P:                  g.VideoPrice480P,
		VideoPrice720P:                  g.VideoPrice720P,
		VideoPrice1080P:                 g.VideoPrice1080P,
		WebSearchPricePerCall:           g.WebSearchPricePerCall,
		ClaudeCodeOnly:                  g.ClaudeCodeOnly,
		FallbackGroupID:                 g.FallbackGroupID,
		FallbackGroupIDOnInvalidRequest: g.FallbackGroupIDOnInvalidRequest,
		ModelRouting:                    g.ModelRouting,
		ModelRoutingEnabled:             g.ModelRoutingEnabled,
		MCPXMLInject:                    g.MCPXMLInject,
		SupportedModelScopes:            g.SupportedModelScopes,
		AllowMessagesDispatch:           g.AllowMessagesDispatch,
		DefaultMappedModel:              g.DefaultMappedModel,
		MessagesDispatchModelConfig:     g.MessagesDispatchModelConfig,
		ModelsListConfig:                g.ModelsListConfig,
		RPMLimit:                        g.RPMLimit,
		PeakRateEnabled:                 g.PeakRateEnabled,
		PeakStart:                       g.PeakStart,
		PeakEnd:                         g.PeakEnd,
		PeakRateMultiplier:              g.PeakRateMultiplier,
	}
}

func authGroupSnapshotToGroup(g *APIKeyAuthGroupSnapshot) *Group {
	if g == nil {
		return nil
	}
	return &Group{
		ID:                              g.ID,
		Name:                            g.Name,
		Platform:                        g.Platform,
		IsExclusive:                     g.IsExclusive,
		Status:                          g.Status,
		Hydrated:                        true,
		SubscriptionType:                g.SubscriptionType,
		RateMultiplier:                  g.RateMultiplier,
		DailyLimitUSD:                   g.DailyLimitUSD,
		WeeklyLimitUSD:                  g.WeeklyLimitUSD,
		MonthlyLimitUSD:                 g.MonthlyLimitUSD,
		AllowImageGeneration:            g.AllowImageGeneration,
		AllowBatchImageGeneration:       g.AllowBatchImageGeneration,
		ImageRateIndependent:            g.ImageRateIndependent,
		ImageRateMultiplier:             g.ImageRateMultiplier,
		ImagePrice1K:                    g.ImagePrice1K,
		ImagePrice2K:                    g.ImagePrice2K,
		ImagePrice4K:                    g.ImagePrice4K,
		VideoRateIndependent:            g.VideoRateIndependent,
		VideoRateMultiplier:             g.VideoRateMultiplier,
		VideoPrice480P:                  g.VideoPrice480P,
		VideoPrice720P:                  g.VideoPrice720P,
		VideoPrice1080P:                 g.VideoPrice1080P,
		WebSearchPricePerCall:           g.WebSearchPricePerCall,
		ClaudeCodeOnly:                  g.ClaudeCodeOnly,
		FallbackGroupID:                 g.FallbackGroupID,
		FallbackGroupIDOnInvalidRequest: g.FallbackGroupIDOnInvalidRequest,
		ModelRouting:                    g.ModelRouting,
		ModelRoutingEnabled:             g.ModelRoutingEnabled,
		MCPXMLInject:                    g.MCPXMLInject,
		SupportedModelScopes:            g.SupportedModelScopes,
		AllowMessagesDispatch:           g.AllowMessagesDispatch,
		DefaultMappedModel:              g.DefaultMappedModel,
		MessagesDispatchModelConfig:     g.MessagesDispatchModelConfig,
		ModelsListConfig:                g.ModelsListConfig,
		RPMLimit:                        g.RPMLimit,
		PeakRateEnabled:                 g.PeakRateEnabled,
		PeakStart:                       g.PeakStart,
		PeakEnd:                         g.PeakEnd,
		PeakRateMultiplier:              g.PeakRateMultiplier,
	}
}

func (s *APIKeyService) snapshotFromAPIKey(ctx context.Context, apiKey *APIKey) *APIKeyAuthSnapshot {
	if apiKey == nil || apiKey.User == nil {
		return nil
	}
	groupIDs := apiKey.EffectiveGroupIDs()
	snapshot := &APIKeyAuthSnapshot{
		Version:           apiKeyAuthSnapshotVersion,
		APIKeyID:          apiKey.ID,
		UserID:            apiKey.UserID,
		GroupID:           apiKey.GroupID,
		GroupIDs:          append([]int64(nil), groupIDs...),
		UnavailableGroups: copyGroupUnavailableMap(apiKey.GroupUnavailableUntil),
		PinnedGroupID:     copyInt64Ptr(apiKey.PinnedGroupID),
		Name:              apiKey.Name,
		Status:            apiKey.Status,
		IPWhitelist:       apiKey.IPWhitelist,
		IPBlacklist:       apiKey.IPBlacklist,
		Quota:             apiKey.Quota,
		QuotaUsed:         apiKey.QuotaUsed,
		ExpiresAt:         apiKey.ExpiresAt,
		RateLimit5h:       apiKey.RateLimit5h,
		RateLimit1d:       apiKey.RateLimit1d,
		RateLimit7d:       apiKey.RateLimit7d,
		User: APIKeyAuthUserSnapshot{
			ID:                         apiKey.User.ID,
			Status:                     apiKey.User.Status,
			Role:                       apiKey.User.Role,
			Balance:                    apiKey.User.Balance,
			Concurrency:                apiKey.User.Concurrency,
			AllowedGroups:              apiKey.User.AllowedGroups,
			Email:                      apiKey.User.Email,
			Username:                   apiKey.User.Username,
			BalanceNotifyEnabled:       apiKey.User.BalanceNotifyEnabled,
			BalanceNotifyThresholdType: apiKey.User.BalanceNotifyThresholdType,
			BalanceNotifyThreshold:     apiKey.User.BalanceNotifyThreshold,
			BalanceNotifyExtraEmails:   apiKey.User.BalanceNotifyExtraEmails,
			TotalRecharged:             apiKey.User.TotalRecharged,
			RPMLimit:                   apiKey.User.RPMLimit,
		},
	}

	// 填充 (user, group) RPM override —— snapshot 构建时查一次 DB，后续请求零 DB 往返。
	if apiKey.GroupID != nil && *apiKey.GroupID > 0 && s.userGroupRateRepo != nil {
		override, err := s.userGroupRateRepo.GetRPMOverrideByUserAndGroup(ctx, apiKey.UserID, *apiKey.GroupID)
		if err == nil && override != nil {
			snapshot.User.UserGroupRPMOverride = override
		}
		// 查询失败或无 override 时留 nil，checkRPM 会回退到 DB 查询
	}
	// Hydrate ordered Groups from GroupIDs when not already preloaded (auth path).
	ids := apiKey.EffectiveGroupIDs()
	if len(apiKey.Groups) == 0 && len(ids) > 0 && s.groupRepo != nil {
		hydrated := make([]*Group, 0, len(ids))
		for _, id := range ids {
			if apiKey.Group != nil && apiKey.Group.ID == id {
				hydrated = append(hydrated, apiKey.Group)
				continue
			}
			g, err := s.groupRepo.GetByID(ctx, id)
			if err != nil || g == nil {
				continue
			}
			hydrated = append(hydrated, g)
		}
		apiKey.Groups = hydrated
		if apiKey.Group == nil && len(hydrated) > 0 {
			apiKey.Group = hydrated[0]
			gid := hydrated[0].ID
			apiKey.GroupID = &gid
		}
	}

	// Prefer preloaded ordered Groups; fall back to primary Group.
	if len(apiKey.Groups) > 0 {
		snapshot.Groups = make([]APIKeyAuthGroupSnapshot, 0, len(apiKey.Groups))
		for _, g := range apiKey.Groups {
			if snap := groupToAuthSnapshot(g); snap != nil {
				snapshot.Groups = append(snapshot.Groups, *snap)
			}
		}
		if snapshot.Group == nil && len(snapshot.Groups) > 0 {
			g0 := snapshot.Groups[0]
			snapshot.Group = &g0
		}
	} else if apiKey.Group != nil {
		snapshot.Group = groupToAuthSnapshot(apiKey.Group)
		if snapshot.Group != nil {
			snapshot.Groups = []APIKeyAuthGroupSnapshot{*snapshot.Group}
		}
	}
	return snapshot
}

func (s *APIKeyService) snapshotToAPIKey(key string, snapshot *APIKeyAuthSnapshot) *APIKey {
	if snapshot == nil {
		return nil
	}
	groupIDs := append([]int64(nil), snapshot.GroupIDs...)
	if len(groupIDs) == 0 && snapshot.GroupID != nil && *snapshot.GroupID > 0 {
		groupIDs = []int64{*snapshot.GroupID}
	}
	apiKey := &APIKey{
		ID:                    snapshot.APIKeyID,
		UserID:                snapshot.UserID,
		GroupID:               snapshot.GroupID,
		GroupIDs:              groupIDs,
		GroupUnavailableUntil: pruneGroupUnavailableMap(snapshot.UnavailableGroups),
		PinnedGroupID:         copyInt64Ptr(snapshot.PinnedGroupID),
		Key:                   key,
		Name:                  snapshot.Name,
		Status:                snapshot.Status,
		IPWhitelist:           snapshot.IPWhitelist,
		IPBlacklist:           snapshot.IPBlacklist,
		Quota:                 snapshot.Quota,
		QuotaUsed:             snapshot.QuotaUsed,
		ExpiresAt:             snapshot.ExpiresAt,
		RateLimit5h:           snapshot.RateLimit5h,
		RateLimit1d:           snapshot.RateLimit1d,
		RateLimit7d:           snapshot.RateLimit7d,
		User: &User{
			ID:                         snapshot.User.ID,
			Status:                     snapshot.User.Status,
			Role:                       snapshot.User.Role,
			Balance:                    snapshot.User.Balance,
			Concurrency:                snapshot.User.Concurrency,
			AllowedGroups:              snapshot.User.AllowedGroups,
			Email:                      snapshot.User.Email,
			Username:                   snapshot.User.Username,
			BalanceNotifyEnabled:       snapshot.User.BalanceNotifyEnabled,
			BalanceNotifyThresholdType: snapshot.User.BalanceNotifyThresholdType,
			BalanceNotifyThreshold:     snapshot.User.BalanceNotifyThreshold,
			BalanceNotifyExtraEmails:   snapshot.User.BalanceNotifyExtraEmails,
			TotalRecharged:             snapshot.User.TotalRecharged,
			RPMLimit:                   snapshot.User.RPMLimit,
			UserGroupRPMOverride:       snapshot.User.UserGroupRPMOverride,
		},
	}
	if len(snapshot.Groups) > 0 {
		apiKey.Groups = make([]*Group, 0, len(snapshot.Groups))
		for i := range snapshot.Groups {
			g := authGroupSnapshotToGroup(&snapshot.Groups[i])
			if g != nil {
				apiKey.Groups = append(apiKey.Groups, g)
			}
		}
		if apiKey.Group == nil && len(apiKey.Groups) > 0 {
			apiKey.Group = apiKey.Groups[0]
		}
	} else if snapshot.Group != nil {
		apiKey.Group = authGroupSnapshotToGroup(snapshot.Group)
		if apiKey.Group != nil {
			apiKey.Groups = []*Group{apiKey.Group}
		}
	}
	if len(apiKey.GroupIDs) == 0 && apiKey.Group != nil {
		apiKey.GroupIDs = []int64{apiKey.Group.ID}
	}
	s.compileAPIKeyIPRules(apiKey)
	return apiKey
}

func copyInt64Ptr(p *int64) *int64 {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func copyGroupUnavailableMap(in map[int64]int64) map[int64]int64 {
	return pruneGroupUnavailableMap(in)
}

// pruneGroupUnavailableMap drops expired entries and returns a shallow copy (or nil).
func pruneGroupUnavailableMap(in map[int64]int64) map[int64]int64 {
	if len(in) == 0 {
		return nil
	}
	now := time.Now().Unix()
	out := make(map[int64]int64, len(in))
	for id, until := range in {
		if id <= 0 || until <= now {
			continue
		}
		out[id] = until
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// IsGroupUnavailableForKey reports whether this key currently skips groupID.
func (k *APIKey) IsGroupUnavailableForKey(groupID int64) bool {
	if k == nil || groupID <= 0 || len(k.GroupUnavailableUntil) == 0 {
		return false
	}
	until, ok := k.GroupUnavailableUntil[groupID]
	if !ok {
		return false
	}
	if until <= time.Now().Unix() {
		delete(k.GroupUnavailableUntil, groupID)
		return false
	}
	return true
}

// MarkGroupUnavailableForKey marks groupID unusable for THIS key only (zero available accounts).
// Does not affect other keys or global group state.
func (k *APIKey) MarkGroupUnavailableForKey(groupID int64, ttl time.Duration) {
	if k == nil || groupID <= 0 {
		return
	}
	if ttl <= 0 {
		ttl = DefaultKeyGroupUnavailabilityTTL
	}
	if k.GroupUnavailableUntil == nil {
		k.GroupUnavailableUntil = make(map[int64]int64, 1)
	}
	until := time.Now().Add(ttl).Unix()
	if prev, ok := k.GroupUnavailableUntil[groupID]; ok && prev >= until {
		return
	}
	k.GroupUnavailableUntil[groupID] = until
}

// ClearGroupUnavailableForKey removes a skip mark for this key.
func (k *APIKey) ClearGroupUnavailableForKey(groupID int64) {
	if k == nil || len(k.GroupUnavailableUntil) == 0 || groupID <= 0 {
		return
	}
	delete(k.GroupUnavailableUntil, groupID)
	if len(k.GroupUnavailableUntil) == 0 {
		k.GroupUnavailableUntil = nil
	}
}

// MarkKeyGroupNoAvailableAccounts updates the in-memory key and persists the skip into auth cache.
// Only call when selection found zero available accounts (empty failed-account set).
func (s *APIKeyService) MarkKeyGroupNoAvailableAccounts(ctx context.Context, apiKey *APIKey, groupID int64, reason string) {
	if s == nil || apiKey == nil || groupID <= 0 || apiKey.Key == "" {
		return
	}
	apiKey.MarkGroupUnavailableForKey(groupID, DefaultKeyGroupUnavailabilityTTL)
	s.persistAuthRuntimeState(ctx, apiKey)
	_ = reason
}

// ClearKeyGroupUnavailable clears a key-scoped skip mark and rewrites auth cache.
func (s *APIKeyService) ClearKeyGroupUnavailable(ctx context.Context, apiKey *APIKey, groupID int64) {
	if s == nil || apiKey == nil || groupID <= 0 || apiKey.Key == "" {
		return
	}
	apiKey.ClearGroupUnavailableForKey(groupID)
	s.persistAuthRuntimeState(ctx, apiKey)
}

// PinKeyGroup stores sticky group pin on this key's auth cache.
func (s *APIKeyService) PinKeyGroup(ctx context.Context, apiKey *APIKey, groupID int64) {
	if s == nil || apiKey == nil || groupID <= 0 || apiKey.Key == "" {
		return
	}
	gid := groupID
	apiKey.PinnedGroupID = &gid
	s.persistAuthRuntimeState(ctx, apiKey)
}

// ClearKeyGroupPin removes sticky pin for this key.
func (s *APIKeyService) ClearKeyGroupPin(ctx context.Context, apiKey *APIKey) {
	if s == nil || apiKey == nil || apiKey.Key == "" {
		return
	}
	if apiKey.PinnedGroupID == nil {
		return
	}
	apiKey.PinnedGroupID = nil
	s.persistAuthRuntimeState(ctx, apiKey)
}

// persistAuthRuntimeState rewrites L1/L2 auth cache with current runtime fields
// (unavailable groups / pin) without full DB reload.
func (s *APIKeyService) persistAuthRuntimeState(ctx context.Context, apiKey *APIKey) {
	if s == nil || apiKey == nil || apiKey.Key == "" {
		return
	}
	cacheKey := s.authCacheKey(apiKey.Key)
	// Prefer merging into existing cached snapshot so we don't drop hydrated fields.
	if entry, ok := s.getAuthCacheEntry(ctx, cacheKey); ok && entry != nil && entry.Snapshot != nil {
		entry.Snapshot.UnavailableGroups = copyGroupUnavailableMap(apiKey.GroupUnavailableUntil)
		entry.Snapshot.PinnedGroupID = copyInt64Ptr(apiKey.PinnedGroupID)
		// Keep primary group fields aligned with request-selected group.
		entry.Snapshot.GroupID = apiKey.GroupID
		if apiKey.Group != nil {
			if snap := groupToAuthSnapshot(apiKey.Group); snap != nil {
				entry.Snapshot.Group = snap
			}
		}
		entry.Snapshot.Version = apiKeyAuthSnapshotVersion
		ttl := s.authCfg.l2TTL
		if ttl <= 0 {
			ttl = s.authCfg.l1TTL
		}
		if ttl <= 0 {
			ttl = time.Minute
		}
		s.setAuthCacheEntry(ctx, cacheKey, entry, ttl)
		return
	}
	// No cache entry: build a full snapshot (best-effort).
	snapshot := s.snapshotFromAPIKey(ctx, apiKey)
	if snapshot == nil {
		return
	}
	ttl := s.authCfg.l2TTL
	if ttl <= 0 {
		ttl = s.authCfg.l1TTL
	}
	if ttl <= 0 {
		ttl = time.Minute
	}
	s.setAuthCacheEntry(ctx, cacheKey, &APIKeyAuthCacheEntry{Snapshot: snapshot}, ttl)
}

// SelectPrimaryGroupForKey promotes the first usable group in the ordered chain.
// Skips key-scoped unavailable groups. Honors sticky PinnedGroupID when still usable.
// Does not consult global group state — only this key's auth-cache marks.
func SelectPrimaryGroupForKey(apiKey *APIKey) {
	if apiKey == nil {
		return
	}
	ids := NormalizeGroupIDs(apiKey.EffectiveGroupIDs())
	if len(ids) == 0 {
		return
	}
	byID := make(map[int64]*Group, len(apiKey.Groups)+1)
	for _, g := range apiKey.Groups {
		if g != nil && g.ID > 0 {
			byID[g.ID] = g
		}
	}
	if apiKey.Group != nil && apiKey.Group.ID > 0 {
		byID[apiKey.Group.ID] = apiKey.Group
	}

	// Sticky pin: keep pinned group while still active and not key-unavailable.
	if apiKey.PinnedGroupID != nil {
		pid := *apiKey.PinnedGroupID
		if g := byID[pid]; g != nil && g.IsActive() && !equalsFoldDeletedGroup(g.Status) && !apiKey.IsGroupUnavailableForKey(pid) {
			apiKey.Group = g
			gid := g.ID
			apiKey.GroupID = &gid
			return
		}
		// Pin no longer usable — drop it so ordered selection resumes.
		apiKey.PinnedGroupID = nil
	}

	var firstActive *Group
	for _, id := range ids {
		g := byID[id]
		if g == nil || !g.IsActive() || equalsFoldDeletedGroup(g.Status) {
			continue
		}
		if firstActive == nil {
			firstActive = g
		}
		if apiKey.IsGroupUnavailableForKey(id) {
			continue
		}
		apiKey.Group = g
		gid := g.ID
		apiKey.GroupID = &gid
		return
	}
	// All active groups key-unavailable (or none active): keep first active for error messaging.
	if firstActive != nil {
		apiKey.Group = firstActive
		gid := firstActive.ID
		apiKey.GroupID = &gid
	}
}

func equalsFoldDeletedGroup(status string) bool {
	switch status {
	case "deleted", "Deleted", "DELETED":
		return true
	default:
		return false
	}
}
