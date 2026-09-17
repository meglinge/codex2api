package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

// 绑定上的豁免名单：签名验证通过且用户在名单里才跳过 Prompt 检查。
func TestPromptFilterExemptUserSkipsChecksOnlyWhenSigned(t *testing.T) {
	const secret = "exempt-user-secret-0123456789abcdef"
	cfg := promptGuardTestConfig()
	cfg.Enabled = true
	handler := newPromptFilterBindingTestHandler(t, cfg, []database.PromptFilterNewAPIBinding{{
		APIKeyID: 101, PlatformCode: "gateway-a", Secret: secret, Enabled: true,
		ExemptUserIDs: []string{"42"},
	}})
	body := []byte(`{"model":"gpt-5.5","input":"hi"}`)

	exempt, _ := signedNewAPIPolicyContextWithSecret(t, "req-exempt-1", newAPIIdentity{UserID: "42", ClientIP: "203.0.113.9"}, "/v1/responses", body, secret)
	// body 还没进上下文时验不了签名，此时不能豁免；这也是它不能进配置缓存的原因。
	if got := handler.promptFilterConfigForRequest(exempt); !got.Enabled {
		t.Fatal("before the ingress body is captured the request must still be checked")
	}
	handler.capturePromptRequestIngress(exempt, body)
	if got := handler.promptFilterConfigForRequest(exempt); got.Enabled {
		t.Fatal("a signed request from an exempt user must skip prompt checks")
	}
	if got := handler.promptFilterConfigForRequest(exempt); got.Enabled || !got.Advanced.NewAPI.Enabled {
		t.Fatalf("exemption must be stable across calls and keep identity verification on: %+v", got)
	}

	other, _ := signedNewAPIPolicyContextWithSecret(t, "req-exempt-2", newAPIIdentity{UserID: "43", ClientIP: "203.0.113.9"}, "/v1/responses", body, secret)
	handler.capturePromptRequestIngress(other, body)
	if got := handler.promptFilterConfigForRequest(other); !got.Enabled {
		t.Fatal("a signed request from a non-exempt user must be checked")
	}

	// 名单里的用户 ID 但没有签名：拿不到可信身份，照常检查。
	unsigned, _ := gin.CreateTestContext(httptest.NewRecorder())
	unsigned.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	unsigned.Request.Header.Set("X-NewAPI-User-ID", "42")
	unsigned.Set(contextAPIKeyID, int64(101))
	handler.capturePromptRequestIngress(unsigned, body)
	if got := handler.promptFilterConfigForRequest(unsigned); !got.Enabled {
		t.Fatal("an unsigned request must never be exempt, even if it claims an exempt user id")
	}

	// 签名对但密钥错（另一个平台的密钥）：验签失败，不豁免。
	forged, _ := signedNewAPIPolicyContextWithSecret(t, "req-exempt-3", newAPIIdentity{UserID: "42", ClientIP: "203.0.113.9"}, "/v1/responses", body, "some-other-platform-secret-0123456789")
	handler.capturePromptRequestIngress(forged, body)
	if got := handler.promptFilterConfigForRequest(forged); !got.Enabled {
		t.Fatal("a request signed with the wrong secret must not be exempt")
	}
}

// WebSocket 回合复用握手时验过的身份：上下文里已有身份就不需要 body。
func TestPromptFilterExemptUserReusesVerifiedIdentity(t *testing.T) {
	cfg := promptGuardTestConfig()
	cfg.Enabled = true
	handler := newPromptFilterBindingTestHandler(t, cfg, []database.PromptFilterNewAPIBinding{{
		APIKeyID: 101, PlatformCode: "gateway-a", Secret: "exempt-user-secret-0123456789abcdef", Enabled: true,
		ExemptUserIDs: []string{"42"},
	}})

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	c.Set(contextAPIKeyID, int64(101))
	c.Set(newAPIIdentityContextKey, verifiedNewAPIIdentityContext{Identity: newAPIIdentity{UserID: "42"}, APIKeyID: 101, Platform: "gateway-a"})
	if got := handler.promptFilterConfigForRequest(c); got.Enabled {
		t.Fatal("an already-verified exempt identity must skip prompt checks without a body")
	}

	// 另一个 Key 验过的身份不能借给这个 Key。
	foreign, _ := gin.CreateTestContext(httptest.NewRecorder())
	foreign.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	foreign.Set(contextAPIKeyID, int64(101))
	foreign.Set(newAPIIdentityContextKey, verifiedNewAPIIdentityContext{Identity: newAPIIdentity{UserID: "42"}, APIKeyID: 202, Platform: "gateway-b"})
	if got := handler.promptFilterConfigForRequest(foreign); !got.Enabled {
		t.Fatal("an identity verified for another api key must not exempt this one")
	}
}

// 绑定关闭或名单为空时，豁免逻辑完全不介入。
func TestPromptFilterExemptUserIgnoredWithoutList(t *testing.T) {
	cfg := promptGuardTestConfig()
	cfg.Enabled = true
	handler := newPromptFilterBindingTestHandler(t, cfg, []database.PromptFilterNewAPIBinding{{
		APIKeyID: 101, PlatformCode: "gateway-a", Secret: "exempt-user-secret-0123456789abcdef", Enabled: false,
		ExemptUserIDs: []string{"42"},
	}})
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	c.Set(contextAPIKeyID, int64(101))
	c.Set(newAPIIdentityContextKey, verifiedNewAPIIdentityContext{Identity: newAPIIdentity{UserID: "42"}, APIKeyID: 101, Platform: "gateway-a"})
	if got := handler.promptFilterConfigForRequest(c); !got.Enabled {
		t.Fatal("a disabled binding must not grant exemptions")
	}
	if !promptFilterBindingExemptsUser(database.PromptFilterNewAPIBinding{ExemptUserIDs: []string{"42"}}, " 42 ") {
		t.Fatal("user id comparison must ignore surrounding whitespace")
	}
	if promptFilterBindingExemptsUser(database.PromptFilterNewAPIBinding{ExemptUserIDs: []string{""}}, "") {
		t.Fatal("an empty user id must never match")
	}
}
