package database

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestNormalizePromptFilterExemptUserIDs(t *testing.T) {
	got := NormalizePromptFilterExemptUserIDs([]string{" 1024 ", "", "2048", "1024", strings.Repeat("x", MaxPromptFilterExemptUserIDLength+1), "\t"})
	if !reflect.DeepEqual(got, []string{"1024", "2048"}) {
		t.Fatalf("normalized = %#v", got)
	}
	if got := NormalizePromptFilterExemptUserIDs(nil); got == nil || len(got) != 0 {
		t.Fatalf("nil input must normalise to an empty, non-nil slice: %#v", got)
	}
	many := make([]string, 0, MaxPromptFilterExemptUserIDs+5)
	for i := 0; i < MaxPromptFilterExemptUserIDs+5; i++ {
		many = append(many, "u"+string(rune('a'+i%26))+strings.Repeat("0", i/26+1))
	}
	if got := NormalizePromptFilterExemptUserIDs(many); len(got) != MaxPromptFilterExemptUserIDs {
		t.Fatalf("cap = %d, want %d", len(got), MaxPromptFilterExemptUserIDs)
	}
	if got := decodePromptFilterExemptUserIDs("not json"); got == nil || len(got) != 0 {
		t.Fatalf("corrupt column must decode to an empty list, got %#v", got)
	}
}

// 豁免名单随绑定落库、回读、更新，老行（列缺省 '[]'）回读为空名单。
func TestPromptFilterNewAPIBindingExemptUserIDsRoundTripSQLite(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "bindings-exempt.sqlite"))
	if err != nil {
		t.Fatalf("New sqlite: %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	apiKeyID, err := db.InsertAPIKey(ctx, "gateway-exempt", "sk-gateway-exempt-test")
	if err != nil {
		t.Fatalf("InsertAPIKey: %v", err)
	}
	binding := &PromptFilterNewAPIBinding{
		APIKeyID: apiKeyID, PlatformCode: "gateway-exempt", PlatformName: "豁免测试",
		Secret: "01234567890123456789012345678901", Enabled: true,
		ExemptUserIDs: []string{" 42 ", "42", "alice"},
	}
	if err := db.CreatePromptFilterNewAPIBinding(ctx, binding); err != nil {
		t.Fatalf("Create binding: %v", err)
	}
	got, err := db.GetPromptFilterNewAPIBinding(ctx, apiKeyID)
	if err != nil {
		t.Fatalf("Get binding: %v", err)
	}
	if !reflect.DeepEqual(got.ExemptUserIDs, []string{"42", "alice"}) {
		t.Fatalf("exempt after create = %#v", got.ExemptUserIDs)
	}

	got.ExemptUserIDs = []string{"bob"}
	if err := db.UpdatePromptFilterNewAPIBinding(ctx, got); err != nil {
		t.Fatalf("Update binding: %v", err)
	}
	list, err := db.ListPromptFilterNewAPIBindings(ctx)
	if err != nil {
		t.Fatalf("List bindings: %v", err)
	}
	if len(list) != 1 || !reflect.DeepEqual(list[0].ExemptUserIDs, []string{"bob"}) {
		t.Fatalf("exempt after update = %#v", list)
	}

	got.ExemptUserIDs = nil
	if err := db.UpdatePromptFilterNewAPIBinding(ctx, got); err != nil {
		t.Fatalf("Update binding (clear): %v", err)
	}
	cleared, err := db.GetPromptFilterNewAPIBinding(ctx, apiKeyID)
	if err != nil {
		t.Fatalf("Get binding: %v", err)
	}
	if cleared.ExemptUserIDs == nil || len(cleared.ExemptUserIDs) != 0 {
		t.Fatalf("cleared list must read back as [] not nil: %#v", cleared.ExemptUserIDs)
	}
}
