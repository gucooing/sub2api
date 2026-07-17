package service

import "testing"

func TestNormalizeGroupIDs(t *testing.T) {
	got := NormalizeGroupIDs([]int64{3, 0, 3, -1, 2, 2, 1})
	want := []int64{3, 2, 1}
	if len(got) != len(want) {
		t.Fatalf("len=%d want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
}

func TestAPIKeyApplyAndEffectiveGroupIDs(t *testing.T) {
	k := &APIKey{}
	k.ApplyGroupIDs([]int64{10, 20})
	if k.GroupID == nil || *k.GroupID != 10 {
		t.Fatalf("primary group_id=%v", k.GroupID)
	}
	if len(k.GroupIDs) != 2 || k.GroupIDs[1] != 20 {
		t.Fatalf("group_ids=%v", k.GroupIDs)
	}

	legacy := &APIKey{GroupID: apiKeyGroupID(99)}
	ids := legacy.EffectiveGroupIDs()
	if len(ids) != 1 || ids[0] != 99 {
		t.Fatalf("legacy effective=%v", ids)
	}

	empty := &APIKey{}
	empty.ApplyGroupIDs(nil)
	if empty.GroupID != nil || len(empty.GroupIDs) != 0 {
		t.Fatalf("empty apply failed: %#v %#v", empty.GroupID, empty.GroupIDs)
	}
}

func TestResolveCreateGroupIDs(t *testing.T) {
	// prefers group_ids
	ids := resolveCreateGroupIDs(CreateAPIKeyRequest{
		GroupID:  apiKeyGroupID(1),
		GroupIDs: []int64{5, 6},
	})
	if len(ids) != 2 || ids[0] != 5 {
		t.Fatalf("prefer group_ids: %v", ids)
	}
	// legacy group_id
	ids = resolveCreateGroupIDs(CreateAPIKeyRequest{GroupID: apiKeyGroupID(7)})
	if len(ids) != 1 || ids[0] != 7 {
		t.Fatalf("legacy: %v", ids)
	}
}

func TestResolveUpdateGroupIDs(t *testing.T) {
	ids, changed := resolveUpdateGroupIDs(UpdateAPIKeyRequest{GroupID: apiKeyGroupID(3)})
	if !changed || len(ids) != 1 || ids[0] != 3 {
		t.Fatalf("legacy update: %v %v", ids, changed)
	}
	ids, changed = resolveUpdateGroupIDs(UpdateAPIKeyRequest{SetGroupIDs: true, GroupIDs: []int64{1, 1, 2}})
	if !changed || len(ids) != 2 || ids[0] != 1 || ids[1] != 2 {
		t.Fatalf("set group_ids: %v %v", ids, changed)
	}
	ids, changed = resolveUpdateGroupIDs(UpdateAPIKeyRequest{})
	if changed {
		t.Fatalf("no change expected: %v", ids)
	}
}

func apiKeyGroupID(v int64) *int64 { return &v }
