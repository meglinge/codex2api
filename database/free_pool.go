package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"time"
)

const FreePoolMintModel = "gpt-5.6-terra"

var (
	ErrFreePoolUnsupported = errors.New("free pool requires SQLite or PostgreSQL")
	ErrFreePoolInvalid     = errors.New("invalid free pool input")
)

type FreePoolTicketStatus string

type FreePoolPage[T any] struct {
	Items        []T    `json:"items"`
	NextBeforeID *int64 `json:"next_before_id"`
}

type FreePoolAccountInput struct {
	Name        string
	Status      string
	ProxyURL    string          `json:"-"`
	Credentials json.RawMessage `json:"-"`
}

type FreePoolAccountSummary struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	Status        string `json:"status"`
	ProxyURL      string `json:"proxy_url"`
	CooldownUntil *int64 `json:"cooldown_until"`
	HasError      bool   `json:"has_error"`
	CreatedAt     int64  `json:"created_at"`
	UpdatedAt     int64  `json:"updated_at"`
}

type FreePoolCookiePair struct {
	CFLB  string `json:"__cflb"`
	OAILB string `json:"__oailb"`
}

type FreePoolCandidateInput struct {
	SourceAccountID int64
	State           string             `json:"-"`
	Pair            FreePoolCookiePair `json:"-"`
	SourceGateway   string
	SourceColo      string
	IssuedAt        int64
	HardExpiresAt   int64
}

type FreePoolTicketSummary struct {
	ID                  int64  `json:"id"`
	SourceAccountID     int64  `json:"source_account_id"`
	MintModel           string `json:"mint_model"`
	SourceGateway       string `json:"source_gateway"`
	SourceColo          string `json:"source_colo"`
	IssuedAt            int64  `json:"issued_at"`
	HardExpiresAt       int64  `json:"hard_expires_at"`
	Status              string `json:"status"`
	HardExpired         bool   `json:"hard_expired"`
	HasQuarantineReason bool   `json:"has_quarantine_reason"`
}

type FreePoolProbeInput struct {
	TicketID  int64
	Stage     string
	Status    string
	NewState  *bool
	ErrorCode string
}

type FreePoolProbeAggregate struct {
	Stage         string `json:"stage"`
	Status        string `json:"status"`
	ErrorCode     string `json:"error_code"`
	Count         int64  `json:"count"`
	NewStateCount int64  `json:"new_state_count"`
	LastCreatedAt int64  `json:"last_created_at"`
}

type FreePoolProbeSummary struct {
	TicketID int64                    `json:"ticket_id"`
	Total    int64                    `json:"total"`
	Items    []FreePoolProbeAggregate `json:"items"`
}

const (
	FreePoolValidationAttemptsDefault = 15
	FreePoolValidationAttemptsMax     = 64
	FreePoolMintWorkersMax            = 1024
	FreePoolMintStrikesDefault        = 3
	FreePoolMintStrikesMax            = 64
	FreePoolFirstTokenTimeoutDefault  = 45
	FreePoolFirstTokenStrikesDefault  = 1
	FreePoolFirstTokenStrikesMax      = 16
	FreePoolSwitchRestDefault         = 8
	FreePoolSwitchRestMax             = 64
	FreePoolSpareDelayDefault         = 30
	FreePoolSpareDelayMax             = 600
	FreePoolSpareMedianSamples        = 10
	FreePoolProbeConcurrencyDefault   = 1
	FreePoolProbeConcurrencyMax       = 16
)

type FreePoolMintSettings struct {
	Workers            int      `json:"mint_workers"`
	ProxyTemplate      string   `json:"proxy_template"`
	Countries          []string `json:"countries"`
	ValidationAttempts int      `json:"validation_attempts"`
	MintFailureStrikes int      `json:"mint_failure_strikes"`
	ExclusiveTickets   bool     `json:"exclusive_tickets"`
	FirstTokenTimeoutS int      `json:"first_token_timeout_seconds"`
	FirstTokenStrikes  int      `json:"first_token_strikes"`
	SwitchRestStrikes  int      `json:"switch_rest_strikes"`
	SpareDelayS        int      `json:"spare_delay_seconds"`
	ProbeConcurrency   int      `json:"probe_concurrency"`
}

func (db *DB) ensureFreePoolSettingsColumns(ctx context.Context) error {
	if err := db.ensureSQLiteColumn(ctx, "free_pool_settings", "proxy_template", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := db.ensureSQLiteColumn(ctx, "free_pool_settings", "countries", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := db.ensureSQLiteColumn(ctx, "free_pool_settings", "validation_attempts", "INTEGER NOT NULL DEFAULT 15"); err != nil {
		return err
	}
	if err := db.ensureSQLiteColumn(ctx, "free_pool_settings", "mint_failure_strikes", "INTEGER NOT NULL DEFAULT 3"); err != nil {
		return err
	}
	if err := db.ensureSQLiteColumn(ctx, "free_pool_settings", "exclusive_tickets", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := db.ensureSQLiteColumn(ctx, "free_pool_settings", "first_token_timeout_seconds", "INTEGER NOT NULL DEFAULT 45"); err != nil {
		return err
	}
	if err := db.ensureSQLiteColumn(ctx, "free_pool_settings", "first_token_strikes", "INTEGER NOT NULL DEFAULT 1"); err != nil {
		return err
	}
	if err := db.ensureSQLiteColumn(ctx, "free_pool_settings", "switch_rest_strikes", "INTEGER NOT NULL DEFAULT 8"); err != nil {
		return err
	}
	if err := db.ensureSQLiteColumn(ctx, "free_pool_settings", "spare_delay_seconds", "INTEGER NOT NULL DEFAULT 30"); err != nil {
		return err
	}
	return db.ensureSQLiteColumn(ctx, "free_pool_settings", "probe_concurrency", "INTEGER NOT NULL DEFAULT 1")
}

func (db *DB) readFreePoolMintStrikes(ctx context.Context) (int, error) {
	var strikes int
	err := db.conn.QueryRowContext(ctx, `SELECT mint_failure_strikes FROM free_pool_settings WHERE id = 1`).Scan(&strikes)
	if errors.Is(err, sql.ErrNoRows) {
		return FreePoolMintStrikesDefault, nil
	}
	if err != nil {
		return 0, err
	}
	if strikes < 1 {
		return FreePoolMintStrikesDefault, nil
	}
	return strikes, nil
}

func (db *DB) GetFreePoolMintSettings(ctx context.Context) (FreePoolMintSettings, error) {
	settings := FreePoolMintSettings{Workers: 2, Countries: []string{}, ValidationAttempts: FreePoolValidationAttemptsDefault, MintFailureStrikes: FreePoolMintStrikesDefault, SpareDelayS: FreePoolSpareDelayDefault, ProbeConcurrency: FreePoolProbeConcurrencyDefault}
	if err := db.requireFreePool(); err != nil {
		return settings, err
	}
	var countries string
	var exclusive int
	err := db.conn.QueryRowContext(ctx, `SELECT mint_workers, proxy_template, countries, validation_attempts, mint_failure_strikes, exclusive_tickets, first_token_timeout_seconds, first_token_strikes, switch_rest_strikes, spare_delay_seconds, probe_concurrency FROM free_pool_settings WHERE id = 1`).Scan(&settings.Workers, &settings.ProxyTemplate, &countries, &settings.ValidationAttempts, &settings.MintFailureStrikes, &exclusive, &settings.FirstTokenTimeoutS, &settings.FirstTokenStrikes, &settings.SwitchRestStrikes, &settings.SpareDelayS, &settings.ProbeConcurrency)
	settings.ExclusiveTickets = exclusive == 1
	if settings.FirstTokenTimeoutS <= 0 {
		settings.FirstTokenTimeoutS = FreePoolFirstTokenTimeoutDefault
	}
	if settings.FirstTokenStrikes < 1 {
		settings.FirstTokenStrikes = FreePoolFirstTokenStrikesDefault
	}
	if settings.MintFailureStrikes < 1 {
		settings.MintFailureStrikes = FreePoolMintStrikesDefault
	}
	if settings.SwitchRestStrikes < 1 {
		settings.SwitchRestStrikes = FreePoolSwitchRestDefault
	}
	if settings.ProbeConcurrency < 1 {
		settings.ProbeConcurrency = FreePoolProbeConcurrencyDefault
	}
	if errors.Is(err, sql.ErrNoRows) {
		return settings, nil
	}
	if err != nil {
		return settings, err
	}
	settings.Countries = NormalizeFreePoolCountries(strings.Split(countries, ","))
	return settings, nil
}

func (db *DB) SetFreePoolMintSettings(ctx context.Context, settings FreePoolMintSettings) error {
	if err := db.requireFreePool(); err != nil {
		return err
	}
	if db.isSQLite() {
		if err := db.ensureFreePoolSettingsColumns(ctx); err != nil {
			return err
		}
	}
	if settings.Workers < 1 || settings.Workers > FreePoolMintWorkersMax || settings.ValidationAttempts < 1 || settings.ValidationAttempts > FreePoolValidationAttemptsMax || settings.MintFailureStrikes < 0 || settings.MintFailureStrikes > FreePoolMintStrikesMax || settings.FirstTokenTimeoutS < 0 || settings.FirstTokenTimeoutS > 600 || settings.FirstTokenStrikes < 0 || settings.FirstTokenStrikes > FreePoolFirstTokenStrikesMax || settings.SwitchRestStrikes < 0 || settings.SwitchRestStrikes > FreePoolSwitchRestMax || settings.SpareDelayS < 0 || settings.SpareDelayS > FreePoolSpareDelayMax || settings.ProbeConcurrency < 0 || settings.ProbeConcurrency > FreePoolProbeConcurrencyMax || len(settings.ProxyTemplate) > 4096 {
		return ErrFreePoolInvalid
	}
	settings.ProxyTemplate = strings.TrimSpace(settings.ProxyTemplate)
	if settings.ProxyTemplate != "" {
		parsed, err := url.Parse(strings.ReplaceAll(settings.ProxyTemplate, "{XX}", "JP"))
		if err != nil || parsed.Hostname() == "" || !freePoolIn(parsed.Scheme, "http", "https", "socks5", "socks5h") {
			return ErrFreePoolInvalid
		}
	}
	settings.Countries = NormalizeFreePoolCountries(settings.Countries)
	if settings.MintFailureStrikes == 0 {
		current, err := db.readFreePoolMintStrikes(ctx)
		if err != nil {
			return err
		}
		settings.MintFailureStrikes = current
	}
	if settings.MintFailureStrikes < 1 || settings.MintFailureStrikes > FreePoolMintStrikesMax {
		return ErrFreePoolInvalid
	}
	if settings.FirstTokenStrikes == 0 || settings.FirstTokenTimeoutS == 0 {
		current, err := db.GetFreePoolMintSettings(ctx)
		if err != nil {
			return err
		}
		if settings.FirstTokenStrikes == 0 {
			settings.FirstTokenStrikes = current.FirstTokenStrikes
		}
		if settings.FirstTokenTimeoutS == 0 {
			settings.FirstTokenTimeoutS = current.FirstTokenTimeoutS
		}
	}
	if settings.SwitchRestStrikes == 0 {
		settings.SwitchRestStrikes = FreePoolSwitchRestDefault
	}
	if settings.ProbeConcurrency == 0 {
		settings.ProbeConcurrency = FreePoolProbeConcurrencyDefault
	}
	if settings.FirstTokenTimeoutS < 5 || settings.FirstTokenTimeoutS > 600 || settings.FirstTokenStrikes < 1 || settings.FirstTokenStrikes > FreePoolFirstTokenStrikesMax || settings.SwitchRestStrikes < 1 || settings.SwitchRestStrikes > FreePoolSwitchRestMax || settings.ProbeConcurrency < 1 || settings.ProbeConcurrency > FreePoolProbeConcurrencyMax {
		return ErrFreePoolInvalid
	}
	return db.withFreePoolWriteTx(ctx, func(tx *sql.Tx) error {
		exclusive := 0
		if settings.ExclusiveTickets {
			exclusive = 1
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO free_pool_settings (id, mint_workers, proxy_template, countries, validation_attempts, mint_failure_strikes, exclusive_tickets, first_token_timeout_seconds, first_token_strikes, switch_rest_strikes, spare_delay_seconds, probe_concurrency) VALUES (1, $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
			ON CONFLICT(id) DO UPDATE SET mint_workers = excluded.mint_workers, proxy_template = excluded.proxy_template, countries = excluded.countries, validation_attempts = excluded.validation_attempts, mint_failure_strikes = excluded.mint_failure_strikes, exclusive_tickets = excluded.exclusive_tickets, first_token_timeout_seconds = excluded.first_token_timeout_seconds, first_token_strikes = excluded.first_token_strikes, switch_rest_strikes = excluded.switch_rest_strikes, spare_delay_seconds = excluded.spare_delay_seconds, probe_concurrency = excluded.probe_concurrency`,
			settings.Workers, settings.ProxyTemplate, strings.Join(settings.Countries, ","), settings.ValidationAttempts, settings.MintFailureStrikes, exclusive, settings.FirstTokenTimeoutS, settings.FirstTokenStrikes, settings.SwitchRestStrikes, settings.SpareDelayS, settings.ProbeConcurrency)
		return err
	})
}

func (db *DB) GetFreePoolMintWorkers(ctx context.Context) (int, error) {
	if err := db.requireFreePool(); err != nil {
		return 0, err
	}
	var workers int
	err := db.conn.QueryRowContext(ctx, `SELECT mint_workers FROM free_pool_settings WHERE id = 1`).Scan(&workers)
	if errors.Is(err, sql.ErrNoRows) {
		return 2, nil
	}
	return workers, err
}

func (db *DB) SetFreePoolMintWorkers(ctx context.Context, workers int) error {
	if err := db.requireFreePool(); err != nil {
		return err
	}
	if workers < 1 || workers > FreePoolMintWorkersMax {
		return ErrFreePoolInvalid
	}
	current, err := db.GetFreePoolMintSettings(ctx)
	if err != nil {
		return err
	}
	current.Workers = workers
	return db.SetFreePoolMintSettings(ctx, current)
}

func (db *DB) accountUseTicketsSQL() string {
	if db.isSQLite() {
		return "COALESCE(use_tickets, 0)"
	}
	return "COALESCE(use_tickets, FALSE)"
}

func freePoolIn(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

func freePoolPageLimit(limit int, beforeID int64) (int, error) {
	if limit == 0 {
		limit = 50
	}
	if limit < 1 || limit > 100 || beforeID < 0 {
		return 0, ErrFreePoolInvalid
	}
	return limit, nil
}

func freePoolSourceLabel(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.' || r == ':':
		default:
			return false
		}
	}
	return true
}

func freePoolProxyDisplay(raw string) string {
	if raw == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "[configured]"
	}
	return parsed.Scheme + "://" + parsed.Host
}

func (db *DB) CreateFreePoolAccount(ctx context.Context, input FreePoolAccountInput) (int64, error) {
	if err := db.requireFreePool(); err != nil {
		return 0, err
	}
	input.Name = strings.TrimSpace(input.Name)
	input.ProxyURL = strings.TrimSpace(input.ProxyURL)
	if input.Status == "" {
		input.Status = "disabled"
	}
	if input.Name == "" || len(input.Name) > 128 || !freePoolIn(input.Status, "active", "disabled") || len(input.ProxyURL) > 4096 || len(input.Credentials) == 0 || len(input.Credentials) > 64*1024 {
		return 0, ErrFreePoolInvalid
	}
	if input.ProxyURL != "" {
		parsed, err := url.Parse(input.ProxyURL)
		if err != nil || parsed.Hostname() == "" || !freePoolIn(parsed.Scheme, "http", "https", "socks5", "socks5h") {
			return 0, ErrFreePoolInvalid
		}
	}
	var credentials map[string]json.RawMessage
	if err := json.Unmarshal(input.Credentials, &credentials); err != nil || len(credentials) == 0 {
		return 0, ErrFreePoolInvalid
	}
	normalized, err := json.Marshal(credentials)
	if err != nil {
		return 0, ErrFreePoolInvalid
	}
	now := time.Now().UTC().UnixMilli()
	var id int64
	err = db.withFreePoolWriteTx(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			INSERT INTO free_pool_accounts (name, status, proxy_url, credentials, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
			input.Name, input.Status, input.ProxyURL, string(normalized), now, now).Scan(&id)
	})
	return id, err
}

func (db *DB) ListFreePoolAccounts(ctx context.Context, limit int, beforeID int64, status string) (FreePoolPage[FreePoolAccountSummary], error) {
	result := FreePoolPage[FreePoolAccountSummary]{Items: []FreePoolAccountSummary{}}
	if err := db.requireFreePool(); err != nil {
		return result, err
	}
	limit, err := freePoolPageLimit(limit, beforeID)
	if err != nil || (status != "" && !freePoolIn(status, "active", "disabled")) {
		if err == nil {
			err = ErrFreePoolInvalid
		}
		return result, err
	}
	rows, err := db.conn.QueryContext(ctx, `
		SELECT id, name, status, proxy_url, cooldown_until, last_error <> '', created_at, updated_at
		FROM free_pool_accounts
		WHERE deleted_at IS NULL AND ($1 = 0 OR id < $1) AND ($2 = '' OR status = $2)
		ORDER BY id DESC LIMIT $3`, beforeID, status, limit+1)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		var item FreePoolAccountSummary
		var cooldown sql.NullInt64
		var rawProxy string
		if err := rows.Scan(&item.ID, &item.Name, &item.Status, &rawProxy, &cooldown, &item.HasError, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return result, err
		}
		item.ProxyURL = freePoolProxyDisplay(rawProxy)
		if cooldown.Valid {
			value := cooldown.Int64
			item.CooldownUntil = &value
		}
		result.Items = append(result.Items, item)
	}
	if err := rows.Err(); err != nil {
		return result, err
	}
	if len(result.Items) > limit {
		result.Items = result.Items[:limit]
		next := result.Items[len(result.Items)-1].ID
		result.NextBeforeID = &next
	}
	return result, nil
}

func (db *DB) SetAllFreePoolAccountStatus(ctx context.Context, status string) (int64, error) {
	if !freePoolIn(status, "active", "disabled") {
		return 0, ErrFreePoolInvalid
	}
	if err := db.requireFreePool(); err != nil {
		return 0, err
	}
	var updated int64
	err := db.withFreePoolWriteTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE free_pool_accounts SET status = $1, updated_at = $2 WHERE deleted_at IS NULL AND status <> $1`, status, time.Now().UTC().UnixMilli())
		if err != nil {
			return err
		}
		updated, err = result.RowsAffected()
		return err
	})
	return updated, err
}

func (db *DB) SetFreePoolAccountStatus(ctx context.Context, id int64, status string) error {
	if id <= 0 || !freePoolIn(status, "active", "disabled") {
		return ErrFreePoolInvalid
	}
	return db.freePoolMutate(ctx, `UPDATE free_pool_accounts SET status = $1, updated_at = $2 WHERE id = $3 AND deleted_at IS NULL`, status, time.Now().UTC().UnixMilli(), id)
}

func (db *DB) DeleteFreePoolAccount(ctx context.Context, id int64) error {
	if id <= 0 {
		return ErrFreePoolInvalid
	}
	now := time.Now().UTC().UnixMilli()
	return db.freePoolMutate(ctx, `UPDATE free_pool_accounts SET status = 'disabled', deleted_at = $1, updated_at = $1 WHERE id = $2 AND deleted_at IS NULL`, now, id)
}

func (db *DB) freePoolMutate(ctx context.Context, query string, args ...any) error {
	if err := db.requireFreePool(); err != nil {
		return err
	}
	return db.withFreePoolWriteTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, query, args...)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if affected == 0 {
			return sql.ErrNoRows
		}
		return nil
	})
}

func (db *DB) InsertFreePoolCandidateTicket(ctx context.Context, input FreePoolCandidateInput) (int64, error) {
	if err := db.requireFreePool(); err != nil {
		return 0, err
	}
	if input.SourceAccountID <= 0 || strings.TrimSpace(input.State) == "" || len(input.State) > 64*1024 || strings.TrimSpace(input.Pair.CFLB) == "" || strings.TrimSpace(input.Pair.OAILB) == "" || !freePoolSourceLabel(input.SourceGateway) || !freePoolSourceLabel(input.SourceColo) || input.IssuedAt <= 0 || input.HardExpiresAt <= input.IssuedAt {
		return 0, ErrFreePoolInvalid
	}
	pair, err := json.Marshal(input.Pair)
	if err != nil {
		return 0, err
	}
	var id int64
	err = db.withFreePoolWriteTx(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			INSERT INTO tickets (source_account_id, mint_model, state, pair, source_gateway, source_colo, issued_at, hard_expires_at, status)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'candidate') RETURNING id`,
			input.SourceAccountID, FreePoolMintModel, input.State, string(pair), input.SourceGateway, input.SourceColo, input.IssuedAt, input.HardExpiresAt).Scan(&id)
	})
	return id, err
}

func (db *DB) ListFreePoolTickets(ctx context.Context, limit int, beforeID, sourceAccountID int64, status string) (FreePoolPage[FreePoolTicketSummary], error) {
	result := FreePoolPage[FreePoolTicketSummary]{Items: []FreePoolTicketSummary{}}
	if err := db.requireFreePool(); err != nil {
		return result, err
	}
	limit, err := freePoolPageLimit(limit, beforeID)
	if err != nil || sourceAccountID < 0 || (status != "" && !freePoolIn(status, "ready", "leased")) {
		if err == nil {
			err = ErrFreePoolInvalid
		}
		return result, err
	}
	rows, err := db.conn.QueryContext(ctx, `
		SELECT id, source_account_id, mint_model, source_gateway, source_colo, issued_at, hard_expires_at, status, hard_expires_at <= $1, quarantine_reason <> ''
		FROM tickets
		WHERE status IN ('ready', 'leased') AND ($2 = 0 OR id < $2) AND ($3 = 0 OR source_account_id = $3) AND ($4 = '' OR status = $4)
		ORDER BY id DESC LIMIT $5`, time.Now().UTC().UnixMilli(), beforeID, sourceAccountID, status, limit+1)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		var item FreePoolTicketSummary
		if err := rows.Scan(&item.ID, &item.SourceAccountID, &item.MintModel, &item.SourceGateway, &item.SourceColo, &item.IssuedAt, &item.HardExpiresAt, &item.Status, &item.HardExpired, &item.HasQuarantineReason); err != nil {
			return result, err
		}
		result.Items = append(result.Items, item)
	}
	if err := rows.Err(); err != nil {
		return result, err
	}
	if len(result.Items) > limit {
		result.Items = result.Items[:limit]
		next := result.Items[len(result.Items)-1].ID
		result.NextBeforeID = &next
	}
	return result, nil
}

func (db *DB) AppendFreePoolProbeObservation(ctx context.Context, input FreePoolProbeInput) (int64, error) {
	if err := db.requireFreePool(); err != nil {
		return 0, err
	}
	if input.TicketID <= 0 || !freePoolIn(input.Stage, "qualification", "followup") || !freePoolIn(input.Status, "pass", "degraded", "failed", "unknown") || !freePoolIn(input.ErrorCode, "", "timeout", "disconnected", "incomplete", "upstream_error", "invalid_response") {
		return 0, ErrFreePoolInvalid
	}
	if input.Status == "pass" && (input.NewState == nil || *input.NewState || input.ErrorCode != "") {
		return 0, ErrFreePoolInvalid
	}
	if input.Status == "degraded" && (input.NewState == nil || !*input.NewState) {
		return 0, ErrFreePoolInvalid
	}
	var newState any
	if input.NewState != nil {
		if *input.NewState {
			newState = 1
		} else {
			newState = 0
		}
	}
	var id int64
	err := db.withFreePoolWriteTx(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			INSERT INTO probe_observations (ticket_id, stage, status, new_state, error_code, created_at)
			VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
			input.TicketID, input.Stage, input.Status, newState, input.ErrorCode, time.Now().UTC().UnixMilli()).Scan(&id)
	})
	return id, err
}

func (db *DB) GetFreePoolProbeSummary(ctx context.Context, ticketID int64) (FreePoolProbeSummary, error) {
	result := FreePoolProbeSummary{TicketID: ticketID, Items: []FreePoolProbeAggregate{}}
	if err := db.requireFreePool(); err != nil {
		return result, err
	}
	if ticketID <= 0 {
		return result, ErrFreePoolInvalid
	}
	var exists bool
	if err := db.conn.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tickets WHERE id = $1)`, ticketID).Scan(&exists); err != nil {
		return result, err
	}
	if !exists {
		return result, sql.ErrNoRows
	}
	rows, err := db.conn.QueryContext(ctx, `
		SELECT stage, status, error_code, COUNT(*), SUM(CASE WHEN new_state = 1 THEN 1 ELSE 0 END), MAX(created_at)
		FROM probe_observations WHERE ticket_id = $1
		GROUP BY stage, status, error_code ORDER BY stage, status, error_code`, ticketID)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		var item FreePoolProbeAggregate
		if err := rows.Scan(&item.Stage, &item.Status, &item.ErrorCode, &item.Count, &item.NewStateCount, &item.LastCreatedAt); err != nil {
			return result, err
		}
		result.Total += item.Count
		result.Items = append(result.Items, item)
	}
	return result, rows.Err()
}
