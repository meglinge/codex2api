package proxy

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// 未转换的握手工作区停用错误必须以 503 池级错误返回,绝不能 500 + 原始握手细节
// 漏给下游。
func TestErrorToGinResponseHandshakeDeactivatedBecomes503(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	ErrorToGinResponse(c, errors.New(`websocket handshake failed: websocket: bad handshake (HTTP 402 Payment Required); Cf-Ray=x: {"detail":{"code":"deactivated_workspace"}}`))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body = %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("Retry-After missing")
	}
	body := rec.Body.String()
	if !strings.Contains(body, "account_pool_deactivated") {
		t.Fatalf("body = %s", body)
	}
	// 文案须附上游原始错误体,但握手内部细节(Cf-Ray 等)不外漏。
	if !strings.Contains(body, "deactivated_workspace") {
		t.Fatalf("upstream detail missing: %s", body)
	}
	if strings.Contains(body, "websocket handshake failed") || strings.Contains(body, "Cf-Ray") {
		t.Fatalf("raw handshake detail leaked: %s", body)
	}

	// 普通错误仍走 500 fallback
	rec = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(rec)
	ErrorToGinResponse(c, errors.New("dial tcp timeout"))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("generic error status = %d, want 500", rec.Code)
	}
}

func TestErrorToGinResponseHidesCodexTurnStateRefresh(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cases := []error{
		fmt.Errorf("刷新 X-Codex-Turn-State 失败: 智力校验未通过: Fernet 密文 176 字节（降智），期望 160"),
		opaqueCodexTurnStateRefreshError(fmt.Errorf("智力校验未通过: Fernet 密文 176 字节（降智），期望 160")),
	}
	for i, err := range cases {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		ErrorToGinResponse(c, err)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("case %d status = %d, want 503; body = %s", i, rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		if !strings.Contains(body, ErrorCodeNoAvailableAccount) {
			t.Fatalf("case %d body = %s", i, body)
		}
		for _, leaked := range []string{"Fernet", "智力", "X-Codex-Turn-State", "176"} {
			if strings.Contains(body, leaked) {
				t.Fatalf("case %d leaked %q: %s", i, leaked, body)
			}
		}
	}
}
