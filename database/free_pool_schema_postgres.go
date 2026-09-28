package database

import (
	"context"
	"fmt"
)

// ensureFreePoolPostgresSchema creates the final free-pool shape that a fresh
// SQLite database has after every ensure* step. It is idempotent and runs in
// one transaction whose first statement takes the free-pool advisory lock.
func (db *DB) ensureFreePoolPostgresSchema(ctx context.Context) error {
	if db == nil || db.conn == nil {
		return fmt.Errorf("database unavailable")
	}
	if db.isSQLite() {
		return nil
	}
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, freePoolAdvisoryLockKey); err != nil {
		return err
	}
	for _, statement := range freePoolPostgresDDL {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("free pool postgres schema: %w", err)
		}
	}
	return tx.Commit()
}

// freePoolPostgresDDL is the translated final SQLite shape. Ids are
// BIGSERIAL/BIGINT, millisecond timestamps stay BIGINT, and 0/1 flags stay
// INTEGER. There are no BOOLEAN, JSONB, or TIMESTAMPTZ columns here.
var freePoolPostgresDDL = []string{
	`CREATE TABLE IF NOT EXISTS free_pool_accounts (
		id BIGSERIAL PRIMARY KEY,
		name TEXT NOT NULL CHECK(length(btrim(name)) BETWEEN 1 AND 128),
		status TEXT NOT NULL DEFAULT 'disabled' CHECK(status IN ('active', 'disabled')),
		proxy_url TEXT NOT NULL DEFAULT '',
		credentials TEXT NOT NULL DEFAULT '{}' CHECK(jsonb_typeof(credentials::jsonb) = 'object'),
		cooldown_until BIGINT NULL CHECK(cooldown_until IS NULL OR cooldown_until >= 0),
		last_error TEXT NOT NULL DEFAULT '',
		consecutive_failures INTEGER NOT NULL DEFAULT 0 CHECK(consecutive_failures >= 0),
		created_at BIGINT NOT NULL CHECK(created_at > 0),
		updated_at BIGINT NOT NULL CHECK(updated_at > 0),
		deleted_at BIGINT NULL CHECK(deleted_at IS NULL OR deleted_at > 0),
		CHECK(deleted_at IS NULL OR status = 'disabled')
	)`,
	`CREATE TABLE IF NOT EXISTS tickets (
		id BIGSERIAL PRIMARY KEY,
		source_account_id BIGINT NOT NULL REFERENCES free_pool_accounts(id),
		mint_model TEXT NOT NULL DEFAULT 'gpt-5.6-terra' CHECK(mint_model = 'gpt-5.6-terra'),
		state TEXT NOT NULL CHECK(length(btrim(state)) > 0),
		pair TEXT NOT NULL CHECK(
			jsonb_typeof(pair::jsonb) = 'object'
			AND jsonb_typeof(pair::jsonb->'__cflb') = 'string'
			AND jsonb_typeof(pair::jsonb->'__oailb') = 'string'
			AND length(btrim(pair::jsonb->>'__cflb')) > 0
			AND length(btrim(pair::jsonb->>'__oailb')) > 0
		),
		source_gateway TEXT NOT NULL,
		source_colo TEXT NOT NULL,
		issued_at BIGINT NOT NULL CHECK(issued_at > 0),
		hard_expires_at BIGINT NOT NULL CHECK(hard_expires_at > issued_at),
		status TEXT NOT NULL DEFAULT 'candidate' CHECK(status IN (
			'candidate', 'ready', 'leased', 'quarantined', 'expired', 'retired'
		)),
		quarantine_reason TEXT NOT NULL DEFAULT ''
	)`,
	`CREATE TABLE IF NOT EXISTS ticket_bindings (
		ticket_id BIGINT NOT NULL CHECK(ticket_id > 0),
		consumer_account_id BIGINT NOT NULL CHECK(consumer_account_id > 0),
		status TEXT NOT NULL DEFAULT 'bound' CHECK(status IN ('bound', 'released', 'pending')),
		bound_at BIGINT NOT NULL CHECK(bound_at > 0),
		PRIMARY KEY(consumer_account_id)
	)`,
	`CREATE TABLE IF NOT EXISTS free_pool_account_rejections (
		ticket_id BIGINT NOT NULL,
		consumer_account_id BIGINT NOT NULL,
		reason TEXT NOT NULL CHECK(reason IN ('state_changed', 'invalid_response', 'rate_limited', 'bad_credentials')),
		created_at BIGINT NOT NULL CHECK(created_at > 0),
		PRIMARY KEY(ticket_id, consumer_account_id)
	)`,
	`CREATE TABLE IF NOT EXISTS free_pool_account_verifications (
		ticket_id BIGINT NOT NULL,
		consumer_account_id BIGINT NOT NULL,
		model TEXT NOT NULL DEFAULT '',
		verified_at BIGINT NOT NULL CHECK(verified_at > 0),
		consumer_state TEXT NOT NULL DEFAULT '',
		PRIMARY KEY(ticket_id, consumer_account_id, model)
	)`,
	`CREATE TABLE IF NOT EXISTS probe_observations (
		id BIGSERIAL PRIMARY KEY,
		ticket_id BIGINT NOT NULL,
		stage TEXT NOT NULL CHECK(stage IN ('qualification', 'followup')),
		status TEXT NOT NULL CHECK(status IN ('pass', 'degraded', 'failed', 'unknown')),
		new_state INTEGER NULL CHECK(new_state IS NULL OR new_state IN (0, 1)),
		error_code TEXT NOT NULL DEFAULT '' CHECK(error_code IN (
			'', 'timeout', 'disconnected', 'incomplete', 'upstream_error', 'invalid_response'
		)),
		created_at BIGINT NOT NULL CHECK(created_at > 0),
		CHECK(status <> 'pass' OR new_state = 0),
		CHECK(status <> 'pass' OR error_code = ''),
		CHECK(status <> 'degraded' OR new_state = 1)
	)`,
	`CREATE TABLE IF NOT EXISTS free_pool_gateway_uses (
		consumer_account_id BIGINT NOT NULL,
		gateway TEXT NOT NULL,
		used_at BIGINT NOT NULL CHECK(used_at > 0),
		PRIMARY KEY(consumer_account_id, gateway)
	)`,
	`CREATE TABLE IF NOT EXISTS free_pool_first_uses (
		id BIGSERIAL PRIMARY KEY,
		ticket_id BIGINT NOT NULL,
		consumer_account_id BIGINT NOT NULL,
		model TEXT NOT NULL DEFAULT '',
		gateway TEXT NOT NULL DEFAULT '',
		passed INTEGER NOT NULL CHECK(passed IN (0, 1)),
		reason TEXT NOT NULL DEFAULT '',
		created_at BIGINT NOT NULL CHECK(created_at > 0),
		edge_colo TEXT NOT NULL DEFAULT ''
	)`,
	`CREATE INDEX IF NOT EXISTS idx_free_pool_first_uses_created ON free_pool_first_uses(created_at DESC)`,
	`CREATE INDEX IF NOT EXISTS idx_free_pool_first_uses_ticket_consumer ON free_pool_first_uses(ticket_id, consumer_account_id)`,
	`CREATE TABLE IF NOT EXISTS free_pool_ticket_stalls (
		ticket_id BIGINT NOT NULL,
		consumer_account_id BIGINT NOT NULL,
		stalls INTEGER NOT NULL DEFAULT 0 CHECK(stalls >= 0),
		PRIMARY KEY(ticket_id, consumer_account_id)
	)`,
	`CREATE TABLE IF NOT EXISTS free_pool_use_leases (
		ticket_id BIGINT NOT NULL CHECK(ticket_id > 0),
		lease_token TEXT PRIMARY KEY CHECK(length(lease_token) > 0),
		lease_until BIGINT NOT NULL CHECK(lease_until > 0),
		phase TEXT NOT NULL CHECK(phase IN ('verifying', 'active'))
	)`,
	`CREATE INDEX IF NOT EXISTS idx_free_pool_use_leases_expiry ON free_pool_use_leases(lease_until)`,
	`CREATE INDEX IF NOT EXISTS idx_free_pool_use_leases_ticket ON free_pool_use_leases(ticket_id)`,
	`CREATE TABLE IF NOT EXISTS free_pool_probe_tickets (
		ticket_id BIGINT PRIMARY KEY CHECK(ticket_id > 0),
		consumer_account_id BIGINT NOT NULL CHECK(consumer_account_id > 0),
		model TEXT NOT NULL CHECK(length(btrim(model)) BETWEEN 1 AND 128),
		token TEXT NOT NULL UNIQUE CHECK(length(token) > 0),
		probe_until BIGINT NOT NULL CHECK(probe_until > 0),
		created_at BIGINT NOT NULL CHECK(created_at > 0)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_free_pool_probe_tickets_consumer ON free_pool_probe_tickets(consumer_account_id)`,
	`CREATE TABLE IF NOT EXISTS free_pool_spare_tickets (
		consumer_account_id BIGINT NOT NULL CHECK(consumer_account_id > 0),
		ticket_id BIGINT PRIMARY KEY CHECK(ticket_id > 0),
		model TEXT NOT NULL CHECK(length(btrim(model)) BETWEEN 1 AND 128),
		token TEXT NOT NULL CHECK(length(token) > 0),
		phase TEXT NOT NULL CHECK(phase IN ('probing', 'ready')),
		probe_until BIGINT NOT NULL CHECK(probe_until > 0),
		created_at BIGINT NOT NULL CHECK(created_at > 0)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_free_pool_spare_tickets_consumer ON free_pool_spare_tickets(consumer_account_id, phase)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_free_pool_spare_tickets_ready_consumer ON free_pool_spare_tickets(consumer_account_id) WHERE phase = 'ready'`,
	`CREATE OR REPLACE FUNCTION free_pool_ticket_hold_exclusive() RETURNS trigger AS $$
	BEGIN
		PERFORM 1 FROM tickets WHERE id = NEW.ticket_id FOR NO KEY UPDATE;
		IF TG_TABLE_NAME = 'free_pool_probe_tickets' THEN
			IF EXISTS (SELECT 1 FROM free_pool_spare_tickets s WHERE s.ticket_id = NEW.ticket_id) THEN
				RAISE EXCEPTION 'ticket_already_held';
			END IF;
		ELSIF EXISTS (SELECT 1 FROM free_pool_probe_tickets p WHERE p.ticket_id = NEW.ticket_id) THEN
			RAISE EXCEPTION 'ticket_already_held';
		END IF;
		RETURN NEW;
	END;
	$$ LANGUAGE plpgsql`,
	`DROP TRIGGER IF EXISTS fp_probe_spare_exclusive ON free_pool_probe_tickets`,
	`CREATE TRIGGER fp_probe_spare_exclusive
		BEFORE INSERT ON free_pool_probe_tickets
		FOR EACH ROW EXECUTE FUNCTION free_pool_ticket_hold_exclusive()`,
	`DROP TRIGGER IF EXISTS fp_spare_probe_exclusive ON free_pool_spare_tickets`,
	`CREATE TRIGGER fp_spare_probe_exclusive
		BEFORE INSERT ON free_pool_spare_tickets
		FOR EACH ROW EXECUTE FUNCTION free_pool_ticket_hold_exclusive()`,
	`CREATE TABLE IF NOT EXISTS free_pool_settings (
		id INTEGER PRIMARY KEY CHECK(id = 1),
		mint_workers INTEGER NOT NULL DEFAULT 2 CHECK(mint_workers BETWEEN 1 AND 1024),
		proxy_template TEXT NOT NULL DEFAULT '',
		countries TEXT NOT NULL DEFAULT '',
		validation_attempts INTEGER NOT NULL DEFAULT 15 CHECK(validation_attempts BETWEEN 1 AND 64),
		mint_failure_strikes INTEGER NOT NULL DEFAULT 3 CHECK(mint_failure_strikes BETWEEN 1 AND 64),
		exclusive_tickets INTEGER NOT NULL DEFAULT 0 CHECK(exclusive_tickets IN (0, 1)),
		first_token_timeout_seconds INTEGER NOT NULL DEFAULT 45 CHECK(first_token_timeout_seconds BETWEEN 5 AND 600),
		first_token_strikes INTEGER NOT NULL DEFAULT 1 CHECK(first_token_strikes BETWEEN 1 AND 16),
		switch_rest_strikes INTEGER NOT NULL DEFAULT 8 CHECK(switch_rest_strikes BETWEEN 1 AND 64),
		spare_delay_seconds INTEGER NOT NULL DEFAULT 30 CHECK(spare_delay_seconds BETWEEN 0 AND 600),
		probe_concurrency INTEGER NOT NULL DEFAULT 1 CHECK(probe_concurrency BETWEEN 1 AND 16)
	)`,
	`INSERT INTO free_pool_settings (id, mint_workers) VALUES (1, 2) ON CONFLICT (id) DO NOTHING`,
	`CREATE INDEX IF NOT EXISTS idx_free_pool_accounts_live ON free_pool_accounts(status, id DESC) WHERE deleted_at IS NULL`,
	`CREATE INDEX IF NOT EXISTS idx_tickets_source_id ON tickets(source_account_id, id DESC)`,
	`CREATE INDEX IF NOT EXISTS idx_tickets_status_id ON tickets(status, id DESC)`,
	`CREATE INDEX IF NOT EXISTS idx_probe_observations_ticket ON probe_observations(ticket_id, created_at DESC, id DESC)`,
	`CREATE INDEX IF NOT EXISTS idx_free_pool_verifications_consumer ON free_pool_account_verifications(consumer_account_id, verified_at)`,
}
