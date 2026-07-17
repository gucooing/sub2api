package handler

import (
	"context"
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// GroupFallbackCursor walks an API key's ordered group_ids chain for multi-group failover.
// Prefer group[0]; when unavailable / no accounts / account failover exhausted, Advance to next.
type GroupFallbackCursor struct {
	BaseAPIKey *service.APIKey
	GroupIDs   []int64
	Index      int
	// Groups maps group ID -> loaded group (optional preload from auth cache).
	Groups map[int64]*service.Group
	// ResolveGroup loads a group by ID when not preloaded.
	ResolveGroup func(ctx context.Context, groupID int64) (*service.Group, error)
	// IsGroupUsable returns whether the group may be used for this key/user.
	// If nil, only active status is checked.
	IsGroupUsable func(ctx context.Context, apiKey *service.APIKey, group *service.Group) bool
}

// NewGroupFallbackCursor builds a cursor from an authenticated API key.
// Skips leading unusable groups so CurrentAPIKey is immediately usable when possible.
func NewGroupFallbackCursor(
	ctx context.Context,
	apiKey *service.APIKey,
	resolveGroup func(ctx context.Context, groupID int64) (*service.Group, error),
	isUsable func(ctx context.Context, apiKey *service.APIKey, group *service.Group) bool,
) *GroupFallbackCursor {
	if apiKey == nil {
		return nil
	}
	ids := service.NormalizeGroupIDs(apiKey.EffectiveGroupIDs())
	c := &GroupFallbackCursor{
		BaseAPIKey:    apiKey,
		GroupIDs:      ids,
		Index:         0,
		Groups:        make(map[int64]*service.Group),
		ResolveGroup:  resolveGroup,
		IsGroupUsable: isUsable,
	}
	// Seed from preloaded Groups / primary Group.
	for _, g := range apiKey.Groups {
		if g != nil && g.ID > 0 {
			c.Groups[g.ID] = g
		}
	}
	if apiKey.Group != nil && apiKey.Group.ID > 0 {
		c.Groups[apiKey.Group.ID] = apiKey.Group
	}
	// Position at first usable group.
	if len(ids) == 0 {
		return c
	}
	for i := range ids {
		c.Index = i
		if g := c.groupAt(ctx, i); g != nil && c.usable(ctx, g) {
			return c
		}
	}
	// None usable; leave Index at last so Advance fails cleanly.
	c.Index = len(ids) - 1
	return c
}

func (c *GroupFallbackCursor) usable(ctx context.Context, group *service.Group) bool {
	if group == nil {
		return false
	}
	if c.IsGroupUsable != nil {
		return c.IsGroupUsable(ctx, c.BaseAPIKey, group)
	}
	return group.IsActive() && !equalsFoldDeleted(group.Status)
}

func equalsFoldDeleted(status string) bool {
	switch status {
	case "deleted", "Deleted", "DELETED":
		return true
	default:
		return false
	}
}

func (c *GroupFallbackCursor) groupAt(ctx context.Context, idx int) *service.Group {
	if c == nil || idx < 0 || idx >= len(c.GroupIDs) {
		return nil
	}
	id := c.GroupIDs[idx]
	if g, ok := c.Groups[id]; ok && g != nil {
		return g
	}
	if c.ResolveGroup == nil {
		return nil
	}
	g, err := c.ResolveGroup(ctx, id)
	if err != nil || g == nil {
		return nil
	}
	c.Groups[id] = g
	return g
}

// CurrentGroup returns the group at the cursor (may be nil if unresolved).
func (c *GroupFallbackCursor) CurrentGroup(ctx context.Context) *service.Group {
	if c == nil {
		return nil
	}
	return c.groupAt(ctx, c.Index)
}

// CurrentAPIKey returns a clone bound to the current group (or base key if no groups).
func (c *GroupFallbackCursor) CurrentAPIKey(ctx context.Context) *service.APIKey {
	if c == nil || c.BaseAPIKey == nil {
		return nil
	}
	if len(c.GroupIDs) == 0 {
		return c.BaseAPIKey
	}
	group := c.groupAt(ctx, c.Index)
	if group == nil {
		return c.BaseAPIKey
	}
	return cloneAPIKeyWithGroup(c.BaseAPIKey, group)
}

// HasNext reports whether a later group exists after the current index.
func (c *GroupFallbackCursor) HasNext() bool {
	if c == nil {
		return false
	}
	return c.Index+1 < len(c.GroupIDs)
}

// Advance moves to the next usable group. Returns false when the chain is exhausted.
func (c *GroupFallbackCursor) Advance(ctx context.Context) (bool, string) {
	if c == nil || len(c.GroupIDs) == 0 {
		return false, "no fallback groups"
	}
	for i := c.Index + 1; i < len(c.GroupIDs); i++ {
		g := c.groupAt(ctx, i)
		if g == nil {
			continue
		}
		if !c.usable(ctx, g) {
			continue
		}
		c.Index = i
		return true, fmt.Sprintf("advanced to group_id=%d", g.ID)
	}
	return false, "fallback group chain exhausted"
}

// Len returns the number of groups in the chain.
func (c *GroupFallbackCursor) Len() int {
	if c == nil {
		return 0
	}
	return len(c.GroupIDs)
}
