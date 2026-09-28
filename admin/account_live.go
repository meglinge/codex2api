package admin

import (
	"net/http"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

type accountLiveItem struct {
	CodexTurnStateStatus *proxy.CodexTurnStateStatus         `json:"codex_turn_state_status,omitempty"`
	ActiveRequests       int64                               `json:"active_requests"`
	OccupiedRequests     int64                               `json:"occupied_requests"`
	TicketStatus         []proxy.FreePoolAccountTicketStatus `json:"ticket_status,omitempty"`
	HealthBuckets        []database.AccountHealthBucket      `json:"health_buckets,omitempty"`
}

// GetAccountLiveState returns request-local runtime counters for the visible
// account page. Scheduler counters use in-memory atomics; Turn-State templates
// are read from the database without rebuilding the paged snapshot.
func (h *Handler) GetAccountLiveState(c *gin.Context) {
	ids, err := parseAccountListIDs(c.Query("ids"))
	if err != nil {
		writeError(c, http.StatusBadRequest, "ids 参数无效")
		return
	}
	if len(ids) > accountListPageMax {
		writeError(c, http.StatusBadRequest, "ids 最多允许 500 个")
		return
	}

	var buckets map[int64][]database.AccountHealthBucket
	var spares map[int64]string
	if h.db != nil {
		buckets, _ = h.db.GetAccountsHealthBucketsByIDs(c.Request.Context(), ids, time.Now(), accountHealthBlockCount, accountHealthBlockMinutes*time.Minute)
		spares, _ = h.db.FreePoolSparePhases(c.Request.Context(), ids)
	}
	live := make(map[int64]accountLiveItem, len(ids))
	for _, id := range ids {
		account := h.store.FindByID(id)
		if account == nil {
			continue
		}
		live[id] = accountLiveItem{
			CodexTurnStateStatus: proxy.GetCodexTurnStateStatus(account),
			ActiveRequests:       account.GetActiveRequests(),
			OccupiedRequests:     account.GetOccupiedRequests(),
			TicketStatus:         proxy.FreePoolAccountTicketStatuses(id, time.Now(), spares[id]),
			HealthBuckets:        buckets[id],
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"accounts":                    live,
		"session_slot_buffer_enabled": h.store.SessionSlotBufferEnabled(),
		"free_pool":                   proxy.FreePoolLive(h.store.Accounts()),
	})
}
