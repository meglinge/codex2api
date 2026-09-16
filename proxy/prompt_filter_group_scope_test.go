package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/codex2api/database"
	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
)

// Prompt 检查的分组范围：只有绑定了圈定分组的 API Key 才检查，未绑定分组的 Key
// 按 include_unbound_keys 决定；没圈定分组时一切照旧。
func TestPromptFilterScopeFollowsAPIKeyGroups(t *testing.T) {
	cfg := promptGuardTestConfig()
	cfg.Enabled = true
	cfg.Advanced.Scope = promptfilter.ScopeConfig{APIKeyGroupIDs: []int64{7}, IncludeUnboundKeys: false}
	handler := newPromptFilterBindingTestHandler(t, cfg, []database.PromptFilterNewAPIBinding{
		{APIKeyID: 505, PlatformCode: "prompt-off", Secret: "prompt-off-secret", Enabled: true, PromptFilterScope: database.PromptFilterScopeOff},
	})
	handler.store.SetAPIKeyAllowedGroups(101, []int64{7, 8})
	handler.store.SetAPIKeyAllowedGroups(202, []int64{9})
	handler.store.SetAPIKeyAllowedGroups(505, []int64{7})

	requestConfig := func(apiKeyID int64) promptfilter.Config {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		c.Set(contextAPIKeyID, apiKeyID)
		return handler.promptFilterConfigForRequest(c)
	}

	if got := requestConfig(101); !got.Enabled {
		t.Fatal("a key bound to the scoped group must be checked")
	}
	if got := requestConfig(202); got.Enabled {
		t.Fatal("a key bound only to another group must not be checked")
	}
	if got := requestConfig(303); got.Enabled {
		t.Fatal("an unbound key must not be checked while include_unbound_keys is off")
	}
	// 范围内的 Key 仍受 NewAPI 绑定的 off 收窄。
	if got := requestConfig(505); got.Enabled || !got.Advanced.NewAPI.Enabled {
		t.Fatalf("binding scope=off must still disable checks inside the group scope: %+v", got)
	}

	cfg.Advanced.Scope.IncludeUnboundKeys = true
	handler.store.SetPromptFilterConfig(cfg)
	if got := requestConfig(303); !got.Enabled {
		t.Fatal("an unbound key must be checked once include_unbound_keys is on")
	}
	if got := requestConfig(202); got.Enabled {
		t.Fatal("include_unbound_keys must not pull in keys bound to other groups")
	}

	cfg.Advanced.Scope = promptfilter.ScopeConfig{}
	handler.store.SetPromptFilterConfig(cfg)
	for _, id := range []int64{101, 202, 303} {
		if got := requestConfig(id); !got.Enabled {
			t.Fatalf("key %d: an empty scope must check every key", id)
		}
	}
}

// 没有 API Key 身份的请求（内部调用）按未绑定分组处理。
func TestPromptFilterScopeWithoutAPIKeyIdentity(t *testing.T) {
	cfg := promptGuardTestConfig()
	cfg.Enabled = true
	cfg.Advanced.Scope = promptfilter.ScopeConfig{APIKeyGroupIDs: []int64{7}, IncludeUnboundKeys: false}
	handler := newPromptFilterBindingTestHandler(t, cfg, nil)

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	if got := handler.promptFilterConfigForRequest(c); got.Enabled {
		t.Fatal("an identity-less request counts as unbound and must follow include_unbound_keys")
	}

	// 只有鉴权中间件存的行、没有 ID 时，退回行里的分组。
	row, _ := gin.CreateTestContext(httptest.NewRecorder())
	row.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	row.Set(contextAPIKeyRow, &database.APIKeyRow{ID: 0, AllowedGroupIDs: []int64{7}})
	if got := handler.promptFilterConfigForRequest(row); !got.Enabled {
		t.Fatal("the auth row's allowed groups must be honoured when no key id is set")
	}
}
