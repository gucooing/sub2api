package handler

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func TestGroupFallbackCursorAdvance(t *testing.T) {
	g1 := &service.Group{ID: 1, Name: "a", Platform: "openai", Status: service.StatusActive, Hydrated: true}
	g2 := &service.Group{ID: 2, Name: "b", Platform: "openai", Status: "disabled", Hydrated: true}
	g3 := &service.Group{ID: 3, Name: "c", Platform: "openai", Status: service.StatusActive, Hydrated: true}

	key := &service.APIKey{
		ID:       1,
		GroupID:  groupFallbackID(1),
		GroupIDs: []int64{1, 2, 3},
		Group:    g1,
		Groups:   []*service.Group{g1, g2, g3},
	}

	cursor := NewGroupFallbackCursor(context.Background(), key, nil, nil)
	if cursor == nil {
		t.Fatal("cursor nil")
	}
	cur := cursor.CurrentAPIKey(context.Background())
	if cur == nil || cur.GroupID == nil || *cur.GroupID != 1 {
		t.Fatalf("start group=%v", cur)
	}

	ok, _ := cursor.Advance(context.Background())
	if !ok {
		t.Fatal("expected advance past disabled g2 to g3")
	}
	cur = cursor.CurrentAPIKey(context.Background())
	if cur == nil || cur.GroupID == nil || *cur.GroupID != 3 {
		t.Fatalf("after advance group=%v", cur.GroupID)
	}

	ok, _ = cursor.Advance(context.Background())
	if ok {
		t.Fatal("expected chain exhausted")
	}
}

func TestGroupFallbackCursorSkipsLeadingDisabled(t *testing.T) {
	g1 := &service.Group{ID: 1, Status: "disabled", Hydrated: true}
	g2 := &service.Group{ID: 2, Status: service.StatusActive, Hydrated: true}
	key := &service.APIKey{
		GroupID:  groupFallbackID(1),
		GroupIDs: []int64{1, 2},
		Group:    g1,
		Groups:   []*service.Group{g1, g2},
	}
	cursor := NewGroupFallbackCursor(context.Background(), key, nil, nil)
	cur := cursor.CurrentAPIKey(context.Background())
	if cur == nil || cur.GroupID == nil || *cur.GroupID != 2 {
		t.Fatalf("expected start on first usable group 2, got %v", cur)
	}
}

func groupFallbackID(v int64) *int64 { return &v }
