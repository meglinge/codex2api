package promptfilter

import (
	"reflect"
	"strings"
	"testing"
)

func TestScopeConfigCoversAPIKey(t *testing.T) {
	unrestricted := ScopeConfig{}
	if !unrestricted.CoversAPIKey(nil) || !unrestricted.CoversAPIKey([]int64{9}) {
		t.Fatal("an empty scope must cover every key")
	}

	scope := ScopeConfig{APIKeyGroupIDs: []int64{7, 8}}
	if !scope.CoversAPIKey([]int64{3, 8}) {
		t.Fatal("a key bound to one of the scoped groups must be covered")
	}
	if scope.CoversAPIKey([]int64{3, 4}) {
		t.Fatal("a key bound only to other groups must not be covered")
	}
	if scope.CoversAPIKey(nil) {
		t.Fatal("an unbound key must follow include_unbound_keys (false here)")
	}
	scope.IncludeUnboundKeys = true
	if !scope.CoversAPIKey(nil) {
		t.Fatal("an unbound key must be covered when include_unbound_keys is on")
	}
}

func TestNormalizeAdvancedConfigScope(t *testing.T) {
	cfg := DefaultAdvancedConfig()
	cfg.Scope = ScopeConfig{APIKeyGroupIDs: []int64{9, 0, 7, -1, 9, 7}}
	got := NormalizeAdvancedConfig(cfg).Scope
	if !reflect.DeepEqual(got.APIKeyGroupIDs, []int64{7, 9}) {
		t.Fatalf("group ids = %v, want sorted, deduped, positive only", got.APIKeyGroupIDs)
	}

	defaults := NormalizeAdvancedConfig(DefaultAdvancedConfig()).Scope
	if defaults.APIKeyGroupIDs == nil || len(defaults.APIKeyGroupIDs) != 0 || !defaults.IncludeUnboundKeys {
		t.Fatalf("defaults = %+v, want [] and include_unbound_keys=true", defaults)
	}
	if !strings.Contains(MarshalAdvancedConfig(DefaultAdvancedConfig()), `"scope":{"api_key_group_ids":[],"include_unbound_keys":true}`) {
		t.Fatalf("marshalled defaults = %s", MarshalAdvancedConfig(DefaultAdvancedConfig()))
	}
}

// 老文档没有 scope 字段时按「所有 Key 都检查」处理；补丁只改分组列表时
// include_unbound_keys 要保住。
func TestAdvancedConfigDocumentScopeRoundTrip(t *testing.T) {
	legacy, err := ParseAdvancedConfigDocument(`{"risk":{"enabled":true}}`)
	if err != nil {
		t.Fatal(err)
	}
	if legacy.Effective.Scope.Restricted() || !legacy.Effective.Scope.IncludeUnboundKeys {
		t.Fatalf("legacy scope = %+v", legacy.Effective.Scope)
	}

	patched, err := MergeAdvancedConfigDocument(legacy.Raw, `{"scope":{"api_key_group_ids":[3,1],"include_unbound_keys":false}}`)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(patched.Effective.Scope.APIKeyGroupIDs, []int64{1, 3}) || patched.Effective.Scope.IncludeUnboundKeys {
		t.Fatalf("patched scope = %+v", patched.Effective.Scope)
	}
	if !patched.Effective.Risk.Enabled {
		t.Fatal("merging scope must not disturb sibling sections")
	}

	groupsOnly, err := MergeAdvancedConfigDocument(patched.Raw, `{"scope":{"api_key_group_ids":[5]}}`)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(groupsOnly.Effective.Scope.APIKeyGroupIDs, []int64{5}) || groupsOnly.Effective.Scope.IncludeUnboundKeys {
		t.Fatalf("groups-only patch = %+v, want include_unbound_keys preserved as false", groupsOnly.Effective.Scope)
	}
}
