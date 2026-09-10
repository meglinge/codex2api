package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestRecordOutboundAndInboundCodexTurnState(t *testing.T) {
	ctx := withCodexTurnStateAudit(context.Background())
	RecordOutboundCodexTurnState(ctx, " sent-blob ")
	RecordInboundCodexTurnState(ctx, "")
	RecordInboundCodexTurnState(ctx, " returned-blob ")

	recorder := httptestRequestWithContext(ctx)
	input := &database.UsageLogInput{}
	populateCodexTurnStateMetaFromRequest(recorder, input)
	if input.OutboundCodexTurnState != "sent-blob" {
		t.Fatalf("outbound = %q, want sent-blob", input.OutboundCodexTurnState)
	}
	if input.InboundCodexTurnState != "returned-blob" {
		t.Fatalf("inbound = %q, want returned-blob", input.InboundCodexTurnState)
	}
}

func TestResetCodexTurnStateAuditClearsBothSides(t *testing.T) {
	ctx := withCodexTurnStateAudit(context.Background())
	RecordOutboundCodexTurnState(ctx, "sent")
	RecordInboundCodexTurnState(ctx, "returned")
	resetCodexTurnStateAudit(ctx)

	recorder := httptestRequestWithContext(ctx)
	input := &database.UsageLogInput{OutboundCodexTurnState: "stale", InboundCodexTurnState: "stale"}
	populateCodexTurnStateMetaFromRequest(recorder, input)
	if input.OutboundCodexTurnState != "" || input.InboundCodexTurnState != "" {
		t.Fatalf("after reset outbound=%q inbound=%q", input.OutboundCodexTurnState, input.InboundCodexTurnState)
	}
}

func TestRecordInboundCodexTurnStateFromMetadataEvent(t *testing.T) {
	ctx := withCodexTurnStateAudit(context.Background())
	recordInboundCodexTurnStateFromEvent(ctx, []byte(`{"type":"codex.response.metadata","headers":{"x-codex-turn-state":"ws-blob"}}`))
	recorder := httptestRequestWithContext(ctx)
	input := &database.UsageLogInput{}
	populateCodexTurnStateMetaFromRequest(recorder, input)
	if input.InboundCodexTurnState != "ws-blob" {
		t.Fatalf("inbound = %q, want ws-blob", input.InboundCodexTurnState)
	}
}

func httptestRequestWithContext(ctx context.Context) *gin.Context {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "/v1/responses", nil)
	c.Request = req
	return c
}
