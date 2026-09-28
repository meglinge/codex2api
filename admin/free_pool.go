package admin

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/codex2api/database"
)

type freePoolAccountRequest struct {
	Name        string          `json:"name"`
	Status      string          `json:"status"`
	ProxyURL    string          `json:"proxy_url"`
	Credentials json.RawMessage `json:"credentials"`
}

type freePoolStatusRequest struct {
	Status string `json:"status"`
}

type freePoolCandidateRequest struct {
	SourceAccountID int64  `json:"source_account_id"`
	State           string `json:"state"`
	CFLB            string `json:"cflb"`
	OAILB           string `json:"oailb"`
	SourceGateway   string `json:"source_gateway"`
	SourceColo      string `json:"source_colo"`
	IssuedAt        int64  `json:"issued_at"`
	HardExpiresAt   int64  `json:"hard_expires_at"`
}

type freePoolProbeRequest struct {
	TicketID  int64  `json:"ticket_id"`
	Stage     string `json:"stage"`
	Status    string `json:"status"`
	NewState  *bool  `json:"new_state"`
	ErrorCode string `json:"error_code"`
}

func (h *Handler) registerFreePoolRoutes(api gin.IRoutes) {
	api.POST("/free-pool/accounts", h.CreateFreePoolAccount)
	api.POST("/free-pool/accounts/import", h.ImportFreePoolAccounts)
	api.GET("/free-pool/accounts", h.ListFreePoolAccounts)
	api.PATCH("/free-pool/accounts/:id", h.UpdateFreePoolAccountStatus)
	api.POST("/free-pool/accounts/status", h.SetAllFreePoolAccountStatus)
	api.DELETE("/free-pool/accounts/:id", h.DeleteFreePoolAccount)
	api.POST("/free-pool/tickets", h.CreateFreePoolCandidateTicket)
	api.GET("/free-pool/tickets", h.ListFreePoolTickets)
	api.POST("/free-pool/probes", h.AppendFreePoolProbeObservation)
	api.GET("/free-pool/tickets/:id/probes", h.GetFreePoolProbeSummary)
	api.GET("/free-pool/first-use-stats", h.GetFreePoolFirstUseStats)
	api.GET("/free-pool/settings", h.GetFreePoolSettings)
	api.PUT("/free-pool/settings", h.UpdateFreePoolSettings)
	api.POST("/free-pool/claim-records/clear", h.ClearFreePoolClaimRecords)
}

func (h *Handler) ClearFreePoolClaimRecords(c *gin.Context) {
	counts, err := h.db.ClearFreePoolClaimRecords(c.Request.Context(), time.Now())
	if err != nil {
		writeFreePoolError(c, err)
		return
	}
	c.JSON(http.StatusOK, counts)
}

func (h *Handler) GetFreePoolFirstUseStats(c *gin.Context) {
	hours := 24
	if raw := strings.TrimSpace(c.Query("hours")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 168 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid free pool request"})
			return
		}
		hours = parsed
	}
	stats, err := h.db.FreePoolFirstUseStats(c.Request.Context(), time.Now().Add(-time.Duration(hours)*time.Hour))
	if err != nil {
		writeFreePoolError(c, err)
		return
	}
	c.JSON(http.StatusOK, stats)
}

func (h *Handler) GetFreePoolSettings(c *gin.Context) {
	settings, err := h.db.GetFreePoolMintSettings(c.Request.Context())
	if err != nil {
		writeFreePoolError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"mint_workers": settings.Workers, "max_mint_workers": database.FreePoolMintWorkersMax, "proxy_template": settings.ProxyTemplate, "countries": settings.Countries, "validation_attempts": settings.ValidationAttempts, "max_validation_attempts": database.FreePoolValidationAttemptsMax, "mint_failure_strikes": settings.MintFailureStrikes, "max_mint_failure_strikes": database.FreePoolMintStrikesMax, "exclusive_tickets": settings.ExclusiveTickets, "first_token_timeout_seconds": settings.FirstTokenTimeoutS, "first_token_strikes": settings.FirstTokenStrikes, "max_first_token_strikes": database.FreePoolFirstTokenStrikesMax, "switch_rest_strikes": settings.SwitchRestStrikes, "max_switch_rest_strikes": database.FreePoolSwitchRestMax, "spare_delay_seconds": settings.SpareDelayS, "max_spare_delay_seconds": database.FreePoolSpareDelayMax, "probe_concurrency": settings.ProbeConcurrency, "max_probe_concurrency": database.FreePoolProbeConcurrencyMax})
}

func (h *Handler) UpdateFreePoolSettings(c *gin.Context) {
	var request struct {
		MintWorkers        int      `json:"mint_workers"`
		ProxyTemplate      string   `json:"proxy_template"`
		Countries          []string `json:"countries"`
		ValidationAttempts int      `json:"validation_attempts"`
		MintFailureStrikes int      `json:"mint_failure_strikes"`
		ExclusiveTickets   bool     `json:"exclusive_tickets"`
		FirstTokenTimeoutS int      `json:"first_token_timeout_seconds"`
		FirstTokenStrikes  int      `json:"first_token_strikes"`
		SwitchRestStrikes  int      `json:"switch_rest_strikes"`
		SpareDelayS        *int     `json:"spare_delay_seconds"`
		ProbeConcurrency   *int     `json:"probe_concurrency"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid free pool request"})
		return
	}
	settings := database.FreePoolMintSettings{Workers: request.MintWorkers, ProxyTemplate: request.ProxyTemplate, Countries: request.Countries, ValidationAttempts: request.ValidationAttempts, MintFailureStrikes: request.MintFailureStrikes, ExclusiveTickets: request.ExclusiveTickets, FirstTokenTimeoutS: request.FirstTokenTimeoutS, FirstTokenStrikes: request.FirstTokenStrikes, SwitchRestStrikes: request.SwitchRestStrikes}
	if request.ProbeConcurrency == nil {
		current, err := h.db.GetFreePoolMintSettings(c.Request.Context())
		if err != nil {
			writeFreePoolError(c, err)
			return
		}
		settings.ProbeConcurrency = current.ProbeConcurrency
	} else {
		settings.ProbeConcurrency = *request.ProbeConcurrency
	}
	if request.SpareDelayS == nil {
		current, err := h.db.GetFreePoolMintSettings(c.Request.Context())
		if err != nil {
			writeFreePoolError(c, err)
			return
		}
		settings.SpareDelayS = current.SpareDelayS
	} else {
		settings.SpareDelayS = *request.SpareDelayS
	}
	if err := h.db.SetFreePoolMintSettings(c.Request.Context(), settings); err != nil {
		writeFreePoolError(c, err)
		return
	}
	saved, err := h.db.GetFreePoolMintSettings(c.Request.Context())
	if err != nil {
		writeFreePoolError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"mint_workers": saved.Workers, "proxy_template": saved.ProxyTemplate, "countries": saved.Countries, "validation_attempts": saved.ValidationAttempts, "mint_failure_strikes": saved.MintFailureStrikes, "exclusive_tickets": saved.ExclusiveTickets, "first_token_timeout_seconds": saved.FirstTokenTimeoutS, "first_token_strikes": saved.FirstTokenStrikes, "switch_rest_strikes": saved.SwitchRestStrikes, "spare_delay_seconds": saved.SpareDelayS, "probe_concurrency": saved.ProbeConcurrency})
}

func (h *Handler) CreateFreePoolAccount(c *gin.Context) {
	var request freePoolAccountRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid free pool request"})
		return
	}
	id, err := h.db.CreateFreePoolAccount(c.Request.Context(), database.FreePoolAccountInput{
		Name: request.Name, Status: request.Status, ProxyURL: request.ProxyURL, Credentials: request.Credentials,
	})
	if err != nil {
		writeFreePoolError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"id": id})
}

func (h *Handler) ListFreePoolAccounts(c *gin.Context) {
	limit, beforeID, ok := freePoolPageQuery(c)
	if !ok {
		return
	}
	page, err := h.db.ListFreePoolAccounts(c.Request.Context(), limit, beforeID, strings.TrimSpace(c.Query("status")))
	if err != nil {
		writeFreePoolError(c, err)
		return
	}
	c.JSON(http.StatusOK, page)
}

func (h *Handler) SetAllFreePoolAccountStatus(c *gin.Context) {
	var request freePoolStatusRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid free pool request"})
		return
	}
	updated, err := h.db.SetAllFreePoolAccountStatus(c.Request.Context(), request.Status)
	if err != nil {
		writeFreePoolError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": request.Status, "updated": updated})
}

func (h *Handler) UpdateFreePoolAccountStatus(c *gin.Context) {
	id, ok := freePoolPathID(c)
	if !ok {
		return
	}
	var request freePoolStatusRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid free pool request"})
		return
	}
	if err := h.db.SetFreePoolAccountStatus(c.Request.Context(), id, request.Status); err != nil {
		writeFreePoolError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"id": id, "status": request.Status})
}

func (h *Handler) DeleteFreePoolAccount(c *gin.Context) {
	id, ok := freePoolPathID(c)
	if !ok {
		return
	}
	if err := h.db.DeleteFreePoolAccount(c.Request.Context(), id); err != nil {
		writeFreePoolError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *Handler) CreateFreePoolCandidateTicket(c *gin.Context) {
	var request freePoolCandidateRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid free pool request"})
		return
	}
	id, err := h.db.InsertFreePoolCandidateTicket(c.Request.Context(), database.FreePoolCandidateInput{
		SourceAccountID: request.SourceAccountID,
		State:           request.State,
		Pair:            database.FreePoolCookiePair{CFLB: request.CFLB, OAILB: request.OAILB},
		SourceGateway:   request.SourceGateway,
		SourceColo:      request.SourceColo,
		IssuedAt:        request.IssuedAt,
		HardExpiresAt:   request.HardExpiresAt,
	})
	if err != nil {
		writeFreePoolError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"id": id})
}

func (h *Handler) ListFreePoolTickets(c *gin.Context) {
	limit, beforeID, ok := freePoolPageQuery(c)
	if !ok {
		return
	}
	sourceID, err := freePoolOptionalQueryID(c, "source_account_id")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid free pool request"})
		return
	}
	page, err := h.db.ListFreePoolTickets(c.Request.Context(), limit, beforeID, sourceID, strings.TrimSpace(c.Query("status")))
	if err != nil {
		writeFreePoolError(c, err)
		return
	}
	c.JSON(http.StatusOK, page)
}

func (h *Handler) AppendFreePoolProbeObservation(c *gin.Context) {
	var request freePoolProbeRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid free pool request"})
		return
	}
	id, err := h.db.AppendFreePoolProbeObservation(c.Request.Context(), database.FreePoolProbeInput{
		TicketID: request.TicketID, Stage: request.Stage, Status: request.Status, NewState: request.NewState, ErrorCode: request.ErrorCode,
	})
	if err != nil {
		writeFreePoolError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"id": id})
}

func (h *Handler) GetFreePoolProbeSummary(c *gin.Context) {
	id, ok := freePoolPathID(c)
	if !ok {
		return
	}
	summary, err := h.db.GetFreePoolProbeSummary(c.Request.Context(), id)
	if err != nil {
		writeFreePoolError(c, err)
		return
	}
	c.JSON(http.StatusOK, summary)
}

func freePoolPageQuery(c *gin.Context) (int, int64, bool) {
	limit := 0
	if raw := c.Query("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid free pool request"})
			return 0, 0, false
		}
		limit = parsed
	}
	beforeID, err := freePoolOptionalQueryID(c, "before_id")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid free pool request"})
		return 0, 0, false
	}
	return limit, beforeID, true
}

func freePoolOptionalQueryID(c *gin.Context, name string) (int64, error) {
	raw := c.Query(name)
	if raw == "" {
		return 0, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 0 {
		return 0, database.ErrFreePoolInvalid
	}
	return value, nil
}

func freePoolPathID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid free pool request"})
		return 0, false
	}
	return id, true
}

func writeFreePoolError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, database.ErrFreePoolUnsupported):
		c.JSON(http.StatusNotImplemented, gin.H{"error": "free pool requires sqlite or postgres"})
	case errors.Is(err, database.ErrFreePoolInvalid):
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid free pool request"})
	case errors.Is(err, sql.ErrNoRows):
		c.JSON(http.StatusNotFound, gin.H{"error": "free pool record not found"})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "free pool request failed"})
	}
}
