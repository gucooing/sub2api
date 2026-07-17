package handler

import (
	"context"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// markKeyGroupNoAvailableAccounts records that THIS key saw zero available accounts
// in groupID. Other keys are unaffected. Only call when selection failed with an
// empty failed-account set (true zero-available) — never after per-account failover
// / retry exhaustion.
//
// The current request still returns the no-account error; the next request will
// skip this group via auth-time SelectPrimaryGroupForKey.
func markKeyGroupNoAvailableAccounts(
	c *gin.Context,
	apiKeyService *service.APIKeyService,
	apiKey *service.APIKey,
	reason string,
	log *zap.Logger,
) {
	if apiKey == nil || apiKey.GroupID == nil || *apiKey.GroupID <= 0 {
		return
	}
	groupID := *apiKey.GroupID
	if apiKeyService != nil {
		apiKeyService.MarkKeyGroupNoAvailableAccounts(contextOrBackground(c), apiKey, groupID, reason)
	} else {
		// Fallback: in-memory only (still helps same process if cache not wired).
		apiKey.MarkGroupUnavailableForKey(groupID, service.DefaultKeyGroupUnavailabilityTTL)
	}
	if log != nil {
		log.Warn("gateway.key_group_marked_unavailable",
			zap.Int64("api_key_id", apiKey.ID),
			zap.Int64("group_id", groupID),
			zap.String("reason", reason),
		)
	}
}

func contextOrBackground(c *gin.Context) context.Context {
	if c != nil && c.Request != nil {
		return c.Request.Context()
	}
	return context.Background()
}

// pinKeyGroupForSticky keeps multi-group selection on groupID while a sticky
// session is bound. Cleared when sticky detaches or pin group becomes unusable.
func pinKeyGroupForSticky(
	c *gin.Context,
	apiKeyService *service.APIKeyService,
	apiKey *service.APIKey,
	groupID int64,
) {
	if apiKey == nil || groupID <= 0 || apiKeyService == nil {
		return
	}
	if apiKey.PinnedGroupID != nil && *apiKey.PinnedGroupID == groupID {
		return
	}
	apiKeyService.PinKeyGroup(contextOrBackground(c), apiKey, groupID)
}

// clearKeyGroupPinBestEffort drops sticky pin when session cannot be honored.
func clearKeyGroupPinBestEffort(
	c *gin.Context,
	apiKeyService *service.APIKeyService,
	apiKey *service.APIKey,
) {
	if apiKey == nil || apiKeyService == nil || apiKey.PinnedGroupID == nil {
		return
	}
	apiKeyService.ClearKeyGroupPin(contextOrBackground(c), apiKey)
}
