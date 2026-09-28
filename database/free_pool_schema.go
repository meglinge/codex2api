package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

var freePoolSQLiteDDL = []string{
	`CREATE TABLE IF NOT EXISTS free_pool_accounts (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL CHECK(length(trim(name)) BETWEEN 1 AND 128),
		status TEXT NOT NULL DEFAULT 'disabled' CHECK(status IN ('active', 'disabled')),
		proxy_url TEXT NOT NULL DEFAULT '',
		credentials TEXT NOT NULL DEFAULT '{}' CHECK(json_valid(credentials) AND json_type(credentials) = 'object'),
		cooldown_until INTEGER NULL CHECK(cooldown_until IS NULL OR cooldown_until >= 0),
		last_error TEXT NOT NULL DEFAULT '',
		consecutive_failures INTEGER NOT NULL DEFAULT 0 CHECK(consecutive_failures >= 0),
		created_at INTEGER NOT NULL CHECK(created_at > 0),
		updated_at INTEGER NOT NULL CHECK(updated_at > 0),
		deleted_at INTEGER NULL CHECK(deleted_at IS NULL OR deleted_at > 0),
		CHECK(deleted_at IS NULL OR status = 'disabled')
	);`,
	`CREATE TABLE IF NOT EXISTS tickets (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		source_account_id INTEGER NOT NULL,
		mint_model TEXT NOT NULL DEFAULT 'gpt-5.6-terra' CHECK(mint_model = 'gpt-5.6-terra'),
		state TEXT NOT NULL CHECK(length(trim(state)) > 0),
		pair TEXT NOT NULL CHECK(
			json_valid(pair)
			AND json_type(pair) = 'object'
			AND json_type(pair, '$.__cflb') = 'text'
			AND json_type(pair, '$.__oailb') = 'text'
			AND length(trim(json_extract(pair, '$.__cflb'))) > 0
			AND length(trim(json_extract(pair, '$.__oailb'))) > 0
		),
		source_gateway TEXT NOT NULL,
		source_colo TEXT NOT NULL,
		issued_at INTEGER NOT NULL CHECK(issued_at > 0),
		hard_expires_at INTEGER NOT NULL CHECK(hard_expires_at > issued_at),
		status TEXT NOT NULL DEFAULT 'candidate' CHECK(status IN (
			'candidate', 'ready', 'leased', 'quarantined', 'expired', 'retired'
		)),
		quarantine_reason TEXT NOT NULL DEFAULT ''
	);`,
	`CREATE TABLE IF NOT EXISTS ticket_bindings (
		ticket_id INTEGER NOT NULL CHECK(ticket_id > 0),
		consumer_account_id INTEGER NOT NULL CHECK(consumer_account_id > 0),
		status TEXT NOT NULL DEFAULT 'bound' CHECK(status IN ('bound', 'released', 'pending')),
		bound_at INTEGER NOT NULL CHECK(bound_at > 0),
		PRIMARY KEY(consumer_account_id)
	);`,
	`CREATE TABLE IF NOT EXISTS free_pool_account_rejections (
		ticket_id INTEGER NOT NULL,
		consumer_account_id INTEGER NOT NULL,
		reason TEXT NOT NULL CHECK(reason IN ('state_changed', 'invalid_response', 'rate_limited', 'bad_credentials')),
		created_at INTEGER NOT NULL CHECK(created_at > 0),
		PRIMARY KEY(ticket_id, consumer_account_id)
	);`,
	`CREATE TABLE IF NOT EXISTS free_pool_account_verifications (
		ticket_id INTEGER NOT NULL,
		consumer_account_id INTEGER NOT NULL,
		model TEXT NOT NULL DEFAULT '',
		verified_at INTEGER NOT NULL CHECK(verified_at > 0),
		consumer_state TEXT NOT NULL DEFAULT '',
		PRIMARY KEY(ticket_id, consumer_account_id, model)
	);`,
	`CREATE TABLE IF NOT EXISTS probe_observations (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		ticket_id INTEGER NOT NULL,
		stage TEXT NOT NULL CHECK(stage IN ('qualification', 'followup')),
		status TEXT NOT NULL CHECK(status IN ('pass', 'degraded', 'failed', 'unknown')),
		new_state INTEGER NULL CHECK(new_state IS NULL OR new_state IN (0, 1)),
		error_code TEXT NOT NULL DEFAULT '' CHECK(error_code IN (
			'', 'timeout', 'disconnected', 'incomplete', 'upstream_error', 'invalid_response'
		)),
		created_at INTEGER NOT NULL CHECK(created_at > 0),
		CHECK(status <> 'pass' OR new_state = 0),
		CHECK(status <> 'pass' OR error_code = ''),
		CHECK(status <> 'degraded' OR new_state = 1)
	);`,
	`CREATE TABLE IF NOT EXISTS free_pool_gateway_uses (
		consumer_account_id INTEGER NOT NULL,
		gateway TEXT NOT NULL,
		used_at INTEGER NOT NULL CHECK(used_at > 0),
		PRIMARY KEY(consumer_account_id, gateway)
	);`,
	`CREATE TABLE IF NOT EXISTS free_pool_first_uses (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		ticket_id INTEGER NOT NULL,
		consumer_account_id INTEGER NOT NULL,
		model TEXT NOT NULL DEFAULT '',
		gateway TEXT NOT NULL DEFAULT '',
		passed INTEGER NOT NULL CHECK(passed IN (0, 1)),
		reason TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL CHECK(created_at > 0),
		edge_colo TEXT NOT NULL DEFAULT ''
	);`,
	`CREATE INDEX IF NOT EXISTS idx_free_pool_first_uses_created ON free_pool_first_uses(created_at DESC);`,
	`CREATE TABLE IF NOT EXISTS free_pool_ticket_stalls (
		ticket_id INTEGER NOT NULL,
		consumer_account_id INTEGER NOT NULL,
		stalls INTEGER NOT NULL DEFAULT 0 CHECK(stalls >= 0),
		PRIMARY KEY(ticket_id, consumer_account_id)
	);`,
	`CREATE TABLE IF NOT EXISTS free_pool_use_leases (
		ticket_id INTEGER NOT NULL CHECK(ticket_id > 0),
		lease_token TEXT PRIMARY KEY CHECK(length(lease_token) > 0),
		lease_until INTEGER NOT NULL CHECK(lease_until > 0),
		phase TEXT NOT NULL CHECK(phase IN ('verifying', 'active'))
	);`,
	`CREATE INDEX IF NOT EXISTS idx_free_pool_use_leases_expiry ON free_pool_use_leases(lease_until);`,
	`CREATE INDEX IF NOT EXISTS idx_free_pool_use_leases_ticket ON free_pool_use_leases(ticket_id);`,
	`CREATE TABLE IF NOT EXISTS free_pool_probe_tickets (
		ticket_id INTEGER PRIMARY KEY CHECK(ticket_id > 0),
		consumer_account_id INTEGER NOT NULL CHECK(consumer_account_id > 0),
		model TEXT NOT NULL CHECK(length(trim(model)) BETWEEN 1 AND 128),
		token TEXT NOT NULL UNIQUE CHECK(length(token) > 0),
		probe_until INTEGER NOT NULL CHECK(probe_until > 0),
		created_at INTEGER NOT NULL CHECK(created_at > 0)
	);`,
	`CREATE INDEX IF NOT EXISTS idx_free_pool_probe_tickets_consumer ON free_pool_probe_tickets(consumer_account_id);`,
	`CREATE TABLE IF NOT EXISTS free_pool_spare_tickets (
		consumer_account_id INTEGER NOT NULL CHECK(consumer_account_id > 0),
		ticket_id INTEGER PRIMARY KEY CHECK(ticket_id > 0),
		model TEXT NOT NULL CHECK(length(trim(model)) BETWEEN 1 AND 128),
		token TEXT NOT NULL CHECK(length(token) > 0),
		phase TEXT NOT NULL CHECK(phase IN ('probing', 'ready')),
		probe_until INTEGER NOT NULL CHECK(probe_until > 0),
		created_at INTEGER NOT NULL CHECK(created_at > 0)
	);`,
	`CREATE INDEX IF NOT EXISTS idx_free_pool_spare_tickets_consumer ON free_pool_spare_tickets(consumer_account_id, phase);`,
	`CREATE TRIGGER IF NOT EXISTS fp_probe_spare_exclusive
	BEFORE INSERT ON free_pool_probe_tickets
	WHEN EXISTS (SELECT 1 FROM free_pool_spare_tickets s WHERE s.ticket_id = NEW.ticket_id)
	BEGIN
		SELECT RAISE(ABORT, 'ticket_already_held');
	END;`,
	`CREATE TRIGGER IF NOT EXISTS fp_spare_probe_exclusive
	BEFORE INSERT ON free_pool_spare_tickets
	WHEN EXISTS (SELECT 1 FROM free_pool_probe_tickets p WHERE p.ticket_id = NEW.ticket_id)
	BEGIN
		SELECT RAISE(ABORT, 'ticket_already_held');
	END;`,
	`CREATE TABLE IF NOT EXISTS free_pool_settings (
		id INTEGER PRIMARY KEY CHECK(id = 1),
		mint_workers INTEGER NOT NULL DEFAULT 2 CHECK(mint_workers BETWEEN 1 AND 1024),
		proxy_template TEXT NOT NULL DEFAULT '',
		countries TEXT NOT NULL DEFAULT '',
		validation_attempts INTEGER NOT NULL DEFAULT 15 CHECK(validation_attempts BETWEEN 1 AND 64),
		mint_failure_strikes INTEGER NOT NULL DEFAULT 3 CHECK(mint_failure_strikes BETWEEN 1 AND 64),
		exclusive_tickets INTEGER NOT NULL DEFAULT 0 CHECK(exclusive_tickets IN (0, 1)),
		first_token_timeout_seconds INTEGER NOT NULL DEFAULT 45 CHECK(first_token_timeout_seconds BETWEEN 5 AND 600),
		first_token_strikes INTEGER NOT NULL DEFAULT 1 CHECK(first_token_strikes BETWEEN 1 AND 16)
	);`,
	`INSERT OR IGNORE INTO free_pool_settings (id, mint_workers) VALUES (1, 2);`,
	`CREATE INDEX IF NOT EXISTS idx_free_pool_accounts_live ON free_pool_accounts(status, id DESC) WHERE deleted_at IS NULL;`,
	`CREATE INDEX IF NOT EXISTS idx_tickets_source_id ON tickets(source_account_id, id DESC);`,
	`CREATE INDEX IF NOT EXISTS idx_tickets_status_id ON tickets(status, id DESC);`,
	`CREATE INDEX IF NOT EXISTS idx_probe_observations_ticket ON probe_observations(ticket_id, created_at DESC, id DESC);`,
	`CREATE TRIGGER IF NOT EXISTS fp_ticket_source_insert
	BEFORE INSERT ON tickets
	WHEN NOT EXISTS (
		SELECT 1 FROM free_pool_accounts
		WHERE id = NEW.source_account_id AND deleted_at IS NULL
	)
	BEGIN
		SELECT RAISE(ABORT, 'free_pool_source_unavailable');
	END;`,
	`CREATE TRIGGER IF NOT EXISTS fp_binding_consumer_immutable
	BEFORE UPDATE OF consumer_account_id ON ticket_bindings
	WHEN NEW.consumer_account_id <> OLD.consumer_account_id
	BEGIN
		SELECT RAISE(ABORT, 'ticket_rebinding_not_supported');
	END;`,
	`CREATE TRIGGER IF NOT EXISTS fp_probe_immutable
	BEFORE UPDATE ON probe_observations
	BEGIN
		SELECT RAISE(ABORT, 'probe_observation_immutable');
	END;`,
}

func (db *DB) ensureFreePoolSQLiteSchema(ctx context.Context) error {
	if db == nil || !db.isSQLite() {
		return fmt.Errorf("free pool sqlite schema requires sqlite")
	}
	if db.conn == nil {
		return errors.New("database unavailable")
	}
	if err := db.withWriteTx(ctx, func(tx *sql.Tx) error {
		for _, statement := range freePoolSQLiteDDL {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	// 旧库的验证记录没有消费账号自己的 state。空串表示尚未按当前协议验证，不能回填铸票 state。
	if err := db.ensureSQLiteColumn(ctx, "free_pool_accounts", "consecutive_failures", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := db.ensureSQLiteColumn(ctx, "free_pool_account_verifications", "consumer_state", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := db.ensureSQLiteColumn(ctx, "free_pool_first_uses", "edge_colo", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := db.ensureFreePoolVerificationModel(ctx); err != nil {
		return err
	}
	if err := db.ensureFreePoolAccountBinding(ctx); err != nil {
		return err
	}
	if err := db.ensureFreePoolRejectionReasons(ctx); err != nil {
		return err
	}
	if err := db.ensureFreePoolSpareConcurrency(ctx); err != nil {
		return err
	}
	if err := db.ensureFreePoolMintWorkersLimit(ctx); err != nil {
		return err
	}
	if err := db.ensureFreePoolSettingsColumns(ctx); err != nil {
		return err
	}
	for _, statement := range []string{
		`CREATE INDEX IF NOT EXISTS idx_free_pool_first_uses_ticket_consumer ON free_pool_first_uses(ticket_id, consumer_account_id)`,
		`CREATE INDEX IF NOT EXISTS idx_free_pool_verifications_consumer ON free_pool_account_verifications(consumer_account_id, verified_at)`,
	} {
		if _, err := db.conn.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

// ensureFreePoolSpareConcurrency 把旧库「一个号一行备用票」改成「一张票一行」。
// 同一个号才能同时测多张，测过的仍只留一张。
func (db *DB) ensureFreePoolSpareConcurrency(ctx context.Context) error {
	var sqlText string
	if err := db.conn.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'free_pool_spare_tickets'`).Scan(&sqlText); err != nil {
		return err
	}
	if strings.Contains(sqlText, "ticket_id INTEGER PRIMARY KEY") {
		return nil
	}
	return db.withWriteTx(ctx, func(tx *sql.Tx) error {
		statements := []string{
			`DROP TRIGGER IF EXISTS fp_probe_spare_exclusive`,
			`DROP TRIGGER IF EXISTS fp_spare_probe_exclusive`,
			`CREATE TABLE free_pool_spare_tickets_next (
				consumer_account_id INTEGER NOT NULL CHECK(consumer_account_id > 0),
				ticket_id INTEGER PRIMARY KEY CHECK(ticket_id > 0),
				model TEXT NOT NULL CHECK(length(trim(model)) BETWEEN 1 AND 128),
				token TEXT NOT NULL CHECK(length(token) > 0),
				phase TEXT NOT NULL CHECK(phase IN ('probing', 'ready')),
				probe_until INTEGER NOT NULL CHECK(probe_until > 0),
				created_at INTEGER NOT NULL CHECK(created_at > 0)
			)`,
			`INSERT INTO free_pool_spare_tickets_next (consumer_account_id, ticket_id, model, token, phase, probe_until, created_at)
			 SELECT consumer_account_id, ticket_id, model, token, phase, probe_until, created_at FROM free_pool_spare_tickets`,
			`DROP TABLE free_pool_spare_tickets`,
			`ALTER TABLE free_pool_spare_tickets_next RENAME TO free_pool_spare_tickets`,
			`CREATE INDEX IF NOT EXISTS idx_free_pool_spare_tickets_consumer ON free_pool_spare_tickets(consumer_account_id, phase)`,
			`CREATE TRIGGER IF NOT EXISTS fp_probe_spare_exclusive
			BEFORE INSERT ON free_pool_probe_tickets
			WHEN EXISTS (SELECT 1 FROM free_pool_spare_tickets s WHERE s.ticket_id = NEW.ticket_id)
			BEGIN
				SELECT RAISE(ABORT, 'ticket_already_held');
			END`,
			`CREATE TRIGGER IF NOT EXISTS fp_spare_probe_exclusive
			BEFORE INSERT ON free_pool_spare_tickets
			WHEN EXISTS (SELECT 1 FROM free_pool_probe_tickets p WHERE p.ticket_id = NEW.ticket_id)
			BEGIN
				SELECT RAISE(ABORT, 'ticket_already_held');
			END`,
		}
		for _, statement := range statements {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return err
			}
		}
		return nil
	})
}

// ensureFreePoolMintWorkersLimit 放宽旧库里 mint_workers 的 1–64 约束。SQLite 不能改 CHECK，只能重建这张单行表。
func (db *DB) ensureFreePoolMintWorkersLimit(ctx context.Context) error {
	var sqlText string
	if err := db.conn.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'free_pool_settings'`).Scan(&sqlText); err != nil {
		return err
	}
	if strings.Contains(sqlText, "BETWEEN 1 AND 1024") {
		return nil
	}
	return db.withWriteTx(ctx, func(tx *sql.Tx) error {
		statements := []string{
			`CREATE TABLE free_pool_settings_workers (
				id INTEGER PRIMARY KEY CHECK(id = 1),
				mint_workers INTEGER NOT NULL DEFAULT 2 CHECK(mint_workers BETWEEN 1 AND 1024),
				proxy_template TEXT NOT NULL DEFAULT '',
				countries TEXT NOT NULL DEFAULT '',
				validation_attempts INTEGER NOT NULL DEFAULT 15 CHECK(validation_attempts BETWEEN 1 AND 64),
				mint_failure_strikes INTEGER NOT NULL DEFAULT 3 CHECK(mint_failure_strikes BETWEEN 1 AND 64),
				exclusive_tickets INTEGER NOT NULL DEFAULT 0 CHECK(exclusive_tickets IN (0, 1)),
				first_token_timeout_seconds INTEGER NOT NULL DEFAULT 45 CHECK(first_token_timeout_seconds BETWEEN 5 AND 600),
				first_token_strikes INTEGER NOT NULL DEFAULT 1 CHECK(first_token_strikes BETWEEN 1 AND 16)
			)`,
			`INSERT INTO free_pool_settings_workers (id, mint_workers, proxy_template, countries, validation_attempts, mint_failure_strikes, exclusive_tickets, first_token_timeout_seconds, first_token_strikes)
			 SELECT id, mint_workers, proxy_template, countries, validation_attempts, mint_failure_strikes, exclusive_tickets, first_token_timeout_seconds, first_token_strikes FROM free_pool_settings`,
			`DROP TABLE free_pool_settings`,
			`ALTER TABLE free_pool_settings_workers RENAME TO free_pool_settings`,
		}
		for _, statement := range statements {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return err
			}
		}
		return nil
	})
}

// ensureFreePoolRejectionReasons 给旧拒绝表加上限流和凭据失败。SQLite 不能改 CHECK，只能重建这张表。
func (db *DB) ensureFreePoolRejectionReasons(ctx context.Context) error {
	var sqlText string
	if err := db.conn.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'free_pool_account_rejections'`).Scan(&sqlText); err != nil {
		return err
	}
	if strings.Contains(sqlText, "'rate_limited'") {
		return nil
	}
	return db.withWriteTx(ctx, func(tx *sql.Tx) error {
		statements := []string{
			`CREATE TABLE free_pool_account_rejections_next (
				ticket_id INTEGER NOT NULL,
				consumer_account_id INTEGER NOT NULL,
				reason TEXT NOT NULL CHECK(reason IN ('state_changed', 'invalid_response', 'rate_limited', 'bad_credentials')),
				created_at INTEGER NOT NULL CHECK(created_at > 0),
				PRIMARY KEY(ticket_id, consumer_account_id)
			)`,
			`INSERT INTO free_pool_account_rejections_next (ticket_id, consumer_account_id, reason, created_at)
			 SELECT ticket_id, consumer_account_id, reason, created_at FROM free_pool_account_rejections`,
			`DROP TABLE free_pool_account_rejections`,
			`ALTER TABLE free_pool_account_rejections_next RENAME TO free_pool_account_rejections`,
		}
		for _, statement := range statements {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return err
			}
		}
		return nil
	})
}

// ensureFreePoolBindingPending 给旧绑定表加上换票中的 pending。SQLite 不能改 CHECK，只能重建这张表。
func (db *DB) ensureFreePoolBindingPending(ctx context.Context) error {
	var sqlText string
	if err := db.conn.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'ticket_bindings'`).Scan(&sqlText); err != nil {
		return err
	}
	if strings.Contains(sqlText, "'pending'") {
		return nil
	}
	return db.withWriteTx(ctx, func(tx *sql.Tx) error {
		statements := []string{
			`DROP TRIGGER IF EXISTS fp_binding_consumer_immutable`,
			`CREATE TABLE ticket_bindings_pending (
				ticket_id INTEGER NOT NULL CHECK(ticket_id > 0),
				consumer_account_id INTEGER NOT NULL CHECK(consumer_account_id > 0),
				status TEXT NOT NULL DEFAULT 'bound' CHECK(status IN ('bound', 'released', 'pending')),
				bound_at INTEGER NOT NULL CHECK(bound_at > 0),
				PRIMARY KEY(consumer_account_id)
			)`,
			`INSERT INTO ticket_bindings_pending (ticket_id, consumer_account_id, status, bound_at)
			 SELECT ticket_id, consumer_account_id, status, bound_at FROM ticket_bindings`,
			`DROP TABLE ticket_bindings`,
			`ALTER TABLE ticket_bindings_pending RENAME TO ticket_bindings`,
			`CREATE TRIGGER IF NOT EXISTS fp_binding_consumer_immutable
			 BEFORE UPDATE OF consumer_account_id ON ticket_bindings
			 WHEN NEW.consumer_account_id <> OLD.consumer_account_id
			 BEGIN
			 	SELECT RAISE(ABORT, 'ticket_rebinding_not_supported');
			 END`,
		}
		for _, statement := range statements {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return err
			}
		}
		return nil
	})
}

// ensureFreePoolAccountBinding 把旧的「一票一个绑定」改成「一个消费账号同时只绑定一张票」。
func (db *DB) ensureFreePoolAccountBinding(ctx context.Context) error {
	var sqlText string
	if err := db.conn.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'ticket_bindings'`).Scan(&sqlText); err != nil {
		return err
	}
	if strings.Contains(sqlText, "PRIMARY KEY(consumer_account_id)") {
		if err := db.ensureFreePoolBindingPending(ctx); err != nil {
			return err
		}
		return db.ensureFreePoolUseLeaseSharesTicket(ctx)
	}
	if err := db.withWriteTx(ctx, func(tx *sql.Tx) error {
		statements := []string{
			`DROP TRIGGER IF EXISTS fp_binding_identity_immutable`,
			`CREATE TABLE ticket_bindings_account (
				ticket_id INTEGER NOT NULL CHECK(ticket_id > 0),
				consumer_account_id INTEGER NOT NULL CHECK(consumer_account_id > 0),
				status TEXT NOT NULL DEFAULT 'bound' CHECK(status IN ('bound', 'released', 'pending')),
				bound_at INTEGER NOT NULL CHECK(bound_at > 0),
				PRIMARY KEY(consumer_account_id)
			)`,
			`INSERT INTO ticket_bindings_account (ticket_id, consumer_account_id, status, bound_at)
			 SELECT ticket_id, consumer_account_id, status, bound_at FROM ticket_bindings
			 WHERE status = 'bound'
			 GROUP BY consumer_account_id
			 HAVING bound_at = MAX(bound_at)`,
			`DROP TABLE ticket_bindings`,
			`ALTER TABLE ticket_bindings_account RENAME TO ticket_bindings`,
			`CREATE TRIGGER IF NOT EXISTS fp_binding_consumer_immutable
			 BEFORE UPDATE OF consumer_account_id ON ticket_bindings
			 WHEN NEW.consumer_account_id <> OLD.consumer_account_id
			 BEGIN
			 	SELECT RAISE(ABORT, 'ticket_rebinding_not_supported');
			 END`,
		}
		for _, statement := range statements {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	return db.ensureFreePoolUseLeaseSharesTicket(ctx)
}

// ensureFreePoolUseLeaseSharesTicket 允许同一张票同时有多次使用。已建立的连接不因新模型领同一张票而被挤掉。
func (db *DB) ensureFreePoolUseLeaseSharesTicket(ctx context.Context) error {
	var sqlText string
	if err := db.conn.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'free_pool_use_leases'`).Scan(&sqlText); err != nil {
		return err
	}
	if strings.Contains(sqlText, "lease_token TEXT PRIMARY KEY") {
		return nil
	}
	return db.withWriteTx(ctx, func(tx *sql.Tx) error {
		statements := []string{
			`CREATE TABLE free_pool_use_leases_shared (
				ticket_id INTEGER NOT NULL CHECK(ticket_id > 0),
				lease_token TEXT PRIMARY KEY CHECK(length(lease_token) > 0),
				lease_until INTEGER NOT NULL CHECK(lease_until > 0),
				phase TEXT NOT NULL CHECK(phase IN ('verifying', 'active'))
			)`,
			`INSERT INTO free_pool_use_leases_shared (ticket_id, lease_token, lease_until, phase)
			 SELECT ticket_id, lease_token, lease_until, phase FROM free_pool_use_leases`,
			`DROP TABLE free_pool_use_leases`,
			`ALTER TABLE free_pool_use_leases_shared RENAME TO free_pool_use_leases`,
			`CREATE INDEX IF NOT EXISTS idx_free_pool_use_leases_expiry ON free_pool_use_leases(lease_until)`,
			`CREATE INDEX IF NOT EXISTS idx_free_pool_use_leases_ticket ON free_pool_use_leases(ticket_id)`,
		}
		for _, statement := range statements {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return err
			}
		}
		return nil
	})
}

// ensureFreePoolVerificationModel 把旧的账号级验证记录拆成账号+模型。旧记录没有模型，不能当成任何模型已验证。
func (db *DB) ensureFreePoolVerificationModel(ctx context.Context) error {
	var sqlText string
	if err := db.conn.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'free_pool_account_verifications'`).Scan(&sqlText); err != nil {
		return err
	}
	if strings.Contains(sqlText, "PRIMARY KEY(ticket_id, consumer_account_id, model)") {
		return nil
	}
	return db.withWriteTx(ctx, func(tx *sql.Tx) error {
		statements := []string{
			`CREATE TABLE free_pool_account_verifications_model (
				ticket_id INTEGER NOT NULL,
				consumer_account_id INTEGER NOT NULL,
				model TEXT NOT NULL DEFAULT '',
				verified_at INTEGER NOT NULL CHECK(verified_at > 0),
				consumer_state TEXT NOT NULL DEFAULT '',
				PRIMARY KEY(ticket_id, consumer_account_id, model)
			)`,
			`DROP TABLE free_pool_account_verifications`,
			`ALTER TABLE free_pool_account_verifications_model RENAME TO free_pool_account_verifications`,
		}
		for _, statement := range statements {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return err
			}
		}
		return nil
	})
}
