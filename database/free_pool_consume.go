package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
)

const FreePoolUseValidationLease = 90 * time.Second

type FreePoolProbeLoad struct {
	AccountID int64
	At        time.Time
	Count     int
}

// FreePoolConsumerProbeLoads 返回最近 since 以来每个消费号的测票次数，按 bucket 聚合。
func (db *DB) FreePoolConsumerProbeLoads(ctx context.Context, since time.Time, bucket time.Duration) ([]FreePoolProbeLoad, error) {
	if err := db.requireFreePool(); err != nil {
		return nil, err
	}
	if bucket < time.Minute {
		bucket = time.Minute
	}
	sinceMS := since.UTC().UnixMilli()
	width := bucket.Milliseconds()
	rows, err := db.conn.QueryContext(ctx, `
		SELECT consumer_account_id, MAX(created_at), COUNT(*)
		FROM free_pool_first_uses
		WHERE created_at >= $1
		GROUP BY consumer_account_id, created_at / $2`, sinceMS, width)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var loads []FreePoolProbeLoad
	for rows.Next() {
		var item FreePoolProbeLoad
		var at int64
		if err := rows.Scan(&item.AccountID, &at, &item.Count); err != nil {
			return nil, err
		}
		item.At = time.UnixMilli(at)
		loads = append(loads, item)
	}
	return loads, rows.Err()
}

// FreePoolTicketLifeMedian 统计票从首次验证通过到降智的秒数。
// 样本不足 minSamples 时 samples 仍返回实际条数，median 为 0，调用方继续用默认延迟。
func (db *DB) FreePoolTicketLifeMedian(ctx context.Context, minSamples int) (samples int, median time.Duration, err error) {
	if err := db.requireFreePool(); err != nil {
		return 0, 0, err
	}
	if minSamples < 1 {
		minSamples = FreePoolSpareMedianSamples
	}
	const lifeSQL = `
		SELECT (r.created_at - MIN(v.verified_at)) / 1000 AS life
		FROM free_pool_account_rejections r
		JOIN free_pool_account_verifications v
		  ON v.ticket_id = r.ticket_id AND v.consumer_account_id = r.consumer_account_id
		WHERE r.reason = 'state_changed' AND r.created_at > v.verified_at
		GROUP BY r.ticket_id, r.consumer_account_id, r.created_at
		HAVING (r.created_at - MIN(v.verified_at)) / 1000 BETWEEN 5 AND 3600`
	if err := db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM (`+lifeSQL+`) life_rows`).Scan(&samples); err != nil {
		return 0, 0, err
	}
	if samples < minSamples {
		return samples, 0, nil
	}
	var seconds int64
	err = db.conn.QueryRowContext(ctx, `SELECT life FROM (`+lifeSQL+`) life_rows ORDER BY life LIMIT 1 OFFSET $1`, samples/2).Scan(&seconds)
	if err != nil {
		return samples, 0, err
	}
	return samples, time.Duration(seconds) * time.Second, nil
}

var (
	ErrFreePoolUseLeaseLost   = errors.New("free pool use lease lost")
	ErrFreePoolTicketRejected = errors.New("free pool ticket rejected for this account")
	ErrFreePoolConsumer       = errors.New("unsupported free pool consumer")
	ErrFreePoolNeedsProbe     = errors.New("free pool account needs a probed ticket")
	ErrFreePoolBindingReady   = errors.New("free pool binding is already usable")
	ErrFreePoolProbeLost      = errors.New("free pool probe lost")
)

type FreePoolMintCommit string

const (
	FreePoolMintStored   FreePoolMintCommit = "stored"
	FreePoolMintLost     FreePoolMintCommit = "lost"
	FreePoolMintRejected FreePoolMintCommit = "rejected"
	FreePoolMintCopied   FreePoolMintCommit = "copied"
)

const freePoolConsumerSQL = `
	COALESCE(platform, 'openai') = 'openai'
	AND COALESCE(type, 'oauth') <> 'responses_api'
	AND LOWER(TRIM(COALESCE(credentials ->> 'upstream_type', ''))) = ''
	AND LOWER(TRIM(COALESCE(credentials ->> 'auth_mode', ''))) <> 'agentidentity'
	AND status <> 'deleted'
	AND COALESCE(error_message, '') <> 'deleted'
`

// FreePoolUseLease 只给代理内部。管理接口不得返回。
type FreePoolUseLease struct {
	TicketID          int64              `json:"-"`
	ConsumerAccountID int64              `json:"-"`
	Token             string             `json:"-"`
	Verified          bool               `json:"-"`
	Sibling           bool               `json:"-"`
	Promoted          bool               `json:"-"`
	ConsumerState     string             `json:"-"`
	Model             string             `json:"-"`
	Pair              FreePoolCookiePair `json:"-"`
	HardExpiresAt     int64              `json:"-"`
	LeaseUntil        int64              `json:"-"`
}

func (FreePoolUseLease) Format(s fmt.State, _ rune) {
	_, _ = io.WriteString(s, "FreePoolUseLease{redacted}")
}

type FreePoolSpareClaim struct {
	TicketID          int64
	ConsumerAccountID int64
	Token             string
	Model             string
	Pair              FreePoolCookiePair
	HardExpiresAt     int64
}

func (FreePoolSpareClaim) Format(s fmt.State, _ rune) {
	_, _ = io.WriteString(s, "FreePoolSpareClaim{redacted}")
}

type FreePoolProbeClaim struct {
	TicketID          int64
	ConsumerAccountID int64
	Token             string
	Model             string
	Pair              FreePoolCookiePair
	HardExpiresAt     int64
}

func (FreePoolProbeClaim) Format(s fmt.State, _ rune) {
	_, _ = io.WriteString(s, "FreePoolProbeClaim{redacted}")
}

func sweepFreePoolHolds(ctx context.Context, tx *sql.Tx, nowMS int64) error {
	if err := sweepFreePoolSpares(ctx, tx, nowMS); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `
		DELETE FROM free_pool_probe_tickets
		WHERE probe_until <= $1
		   OR NOT EXISTS (
		   	SELECT 1 FROM tickets t
		   	JOIN free_pool_accounts source ON source.id = t.source_account_id
		   	WHERE t.id = free_pool_probe_tickets.ticket_id AND t.status = 'ready' AND t.hard_expires_at > $2
		   	  AND source.status = 'active' AND source.deleted_at IS NULL
		   )`, nowMS, nowMS)
	return err
}

// promoteFreePoolSpareTx 在同一事务里把这个号测好的预备票升成当前绑定。
// 当前绑定还可用时不动。返回升上去的票号；没有可升的预备票时返回 0。
func promoteFreePoolSpareTx(ctx context.Context, tx *sql.Tx, consumerID, nowMS int64) (int64, error) {
	if err := sweepFreePoolSpares(ctx, tx, nowMS); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE ticket_bindings SET status = 'pending'
		WHERE consumer_account_id = $1 AND status = 'bound'
		  AND NOT EXISTS (
		  	SELECT 1 FROM tickets t
		  	JOIN free_pool_accounts source ON source.id = t.source_account_id
		  	WHERE t.id = ticket_bindings.ticket_id
		  	  AND t.status IN ('ready', 'leased') AND t.hard_expires_at > $2
		  	  AND source.status = 'active' AND source.deleted_at IS NULL
		  	  AND NOT EXISTS (
		  	  	SELECT 1 FROM free_pool_account_rejections r
		  	  	WHERE r.ticket_id = t.id AND r.consumer_account_id = ticket_bindings.consumer_account_id
		  	  )
		  	  AND EXISTS (
		  	  	SELECT 1 FROM free_pool_account_verifications v
		  	  	WHERE v.ticket_id = t.id AND v.consumer_account_id = ticket_bindings.consumer_account_id AND v.consumer_state <> ''
		  	  )
		  )`, consumerID, nowMS); err != nil {
		return 0, err
	}
	result, err := tx.ExecContext(ctx, `
		INSERT INTO ticket_bindings (ticket_id, consumer_account_id, status, bound_at)
		SELECT s.ticket_id, s.consumer_account_id, 'bound', $1
		FROM free_pool_spare_tickets s
		WHERE s.consumer_account_id = $2 AND s.phase = 'ready'
		  AND EXISTS (
		  	SELECT 1 FROM free_pool_account_verifications v
		  	WHERE v.ticket_id = s.ticket_id AND v.consumer_account_id = s.consumer_account_id AND v.consumer_state <> ''
		  )
		ORDER BY s.created_at, s.ticket_id
		LIMIT 1
		ON CONFLICT(consumer_account_id) DO UPDATE SET
			ticket_id = excluded.ticket_id, status = 'bound', bound_at = excluded.bound_at
		WHERE ticket_bindings.status <> 'bound'`, nowMS, consumerID)
	if err != nil {
		return 0, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if rows != 1 {
		return 0, nil
	}
	var ticketID int64
	if err := tx.QueryRowContext(ctx, `SELECT ticket_id FROM ticket_bindings WHERE consumer_account_id = $1 AND status = 'bound'`, consumerID).Scan(&ticketID); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM free_pool_spare_tickets WHERE consumer_account_id = $1 AND phase = 'ready'`, consumerID); err != nil {
		return 0, err
	}
	return ticketID, nil
}

func sweepFreePoolSpares(ctx context.Context, tx *sql.Tx, nowMS int64) error {
	_, err := tx.ExecContext(ctx, `
		DELETE FROM free_pool_spare_tickets
		WHERE (phase = 'probing' AND probe_until <= $1)
		   OR NOT EXISTS (
		   	SELECT 1 FROM tickets t
		   	JOIN free_pool_accounts source ON source.id = t.source_account_id
		   	WHERE t.id = free_pool_spare_tickets.ticket_id AND t.status = 'ready' AND t.hard_expires_at > $2
		   	  AND source.status = 'active' AND source.deleted_at IS NULL
		   )
		   OR EXISTS (
		   	SELECT 1 FROM free_pool_account_rejections r
		   	WHERE r.ticket_id = free_pool_spare_tickets.ticket_id AND r.consumer_account_id = free_pool_spare_tickets.consumer_account_id
		   )
		   OR NOT EXISTS (
		   	SELECT 1 FROM accounts a
		   	WHERE a.id = free_pool_spare_tickets.consumer_account_id
		   	  AND COALESCE(a.use_tickets, FALSE) AND COALESCE(a.enabled, TRUE)
		   )`, nowMS, nowMS)
	return err
}

// BindFreePoolProbedTicket 给测试和迁移用：把一张已经双 400 通过的票绑到消费号上。
func (db *DB) BindFreePoolProbedTicket(ctx context.Context, consumerID, ticketID int64, model, state string, now time.Time) error {
	probe := FreePoolProbeClaim{TicketID: ticketID, ConsumerAccountID: consumerID, Model: strings.TrimSpace(model), Token: uuid.NewString()}
	nowMS := now.UTC().UnixMilli()
	if err := db.withFreePoolWriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO free_pool_probe_tickets (ticket_id, consumer_account_id, model, token, probe_until, created_at) VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT DO NOTHING`, probe.TicketID, consumerID, probe.Model, probe.Token, nowMS+int64(FreePoolUseValidationLease/time.Millisecond), nowMS)
		return err
	}); err != nil {
		return err
	}
	return db.WinFreePoolProbe(ctx, probe, state, now)
}

func freePoolExcludeTicketSQL(exclude []int64, start int) string {
	if len(exclude) == 0 {
		return ""
	}
	placeholders := make([]string, len(exclude))
	for i := range exclude {
		placeholders[i] = fmt.Sprintf("$%d", start+i)
	}
	return "AND t.id NOT IN (" + strings.Join(placeholders, ",") + ")"
}

func freePoolExcludeArgs(exclude []int64, limit int) []any {
	args := make([]any, 0, len(exclude)+1)
	for _, id := range exclude {
		args = append(args, id)
	}
	args = append(args, limit)
	return args
}

func (db *DB) ReserveFreePoolProbeTickets(ctx context.Context, consumerID int64, model string, n int, now time.Time, exclude ...int64) ([]FreePoolProbeClaim, error) {
	if err := db.requireFreePool(); err != nil {
		return nil, err
	}
	nowMS := now.UTC().UnixMilli()
	model = strings.TrimSpace(model)
	if consumerID <= 0 || nowMS <= 0 || n < 1 || model == "" || len(model) > 128 {
		return nil, ErrFreePoolInvalid
	}
	var claims []FreePoolProbeClaim
	err := db.withFreePoolWriteTx(ctx, func(tx *sql.Tx) error {
		if err := sweepFreePoolHolds(ctx, tx, nowMS); err != nil {
			return err
		}
		var allowed bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM accounts WHERE id = $1 AND COALESCE(use_tickets, FALSE) AND COALESCE(enabled, TRUE) AND `+freePoolConsumerSQL+`)`, consumerID).Scan(&allowed); err != nil {
			return err
		}
		if !allowed {
			return ErrFreePoolConsumer
		}
		// 主票已经不可用、但预备票测好了时，先升预备票。升成功就不用再打一轮双 400。
		if promoted, err := promoteFreePoolSpareTx(ctx, tx, consumerID, nowMS); err != nil {
			return err
		} else if promoted > 0 {
			return ErrFreePoolBindingReady
		}
		var ready bool
		if err := tx.QueryRowContext(ctx, `
			SELECT EXISTS(
				SELECT 1 FROM ticket_bindings b
				JOIN tickets t ON t.id = b.ticket_id
				WHERE b.consumer_account_id = $1 AND b.status = 'bound'
				  AND t.status IN ('ready', 'leased') AND t.hard_expires_at > $2
				  AND EXISTS (
				  	SELECT 1 FROM free_pool_account_verifications v
				  	WHERE v.ticket_id = b.ticket_id AND v.consumer_account_id = b.consumer_account_id AND v.consumer_state <> ''
				  )
				  AND NOT EXISTS (
				  	SELECT 1 FROM free_pool_account_rejections r
				  	WHERE r.ticket_id = b.ticket_id AND r.consumer_account_id = b.consumer_account_id
				  )
			)`, consumerID, nowMS).Scan(&ready); err != nil {
			return err
		}
		if ready {
			return ErrFreePoolBindingReady
		}
		var exclusive bool
		if err := tx.QueryRowContext(ctx, `SELECT exclusive_tickets = 1 FROM free_pool_settings WHERE id = 1`).Scan(&exclusive); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		boundFilter := ""
		if exclusive {
			boundFilter = `AND NOT EXISTS (SELECT 1 FROM ticket_bindings b WHERE b.ticket_id = t.id AND b.status = 'bound')`
		}
		rows, err := tx.QueryContext(ctx, `
			SELECT ranked.id FROM (
				SELECT t.id, t.source_gateway, ROW_NUMBER() OVER (
					PARTITION BY t.source_gateway
					ORDER BY t.issued_at DESC, t.id DESC
				) AS gateway_rank
				FROM tickets t
				JOIN free_pool_accounts source ON source.id = t.source_account_id
				WHERE t.status = 'ready' AND t.hard_expires_at > $1
				  AND source.status = 'active' AND source.deleted_at IS NULL
				  `+boundFilter+`
				  AND NOT EXISTS (SELECT 1 FROM free_pool_spare_tickets s WHERE s.ticket_id = t.id)
				  AND NOT EXISTS (SELECT 1 FROM free_pool_probe_tickets p WHERE p.ticket_id = t.id)
				  AND NOT EXISTS (SELECT 1 FROM free_pool_account_rejections r WHERE r.ticket_id = t.id AND r.consumer_account_id = $2)
				  AND NOT EXISTS (SELECT 1 FROM free_pool_account_verifications v WHERE v.ticket_id = t.id AND v.consumer_account_id = $2)
				  `+freePoolExcludeTicketSQL(exclude, 4)+`
			) ranked
			ORDER BY ranked.gateway_rank, CASE WHEN EXISTS (
				SELECT 1 FROM free_pool_gateway_uses g WHERE g.consumer_account_id = $3 AND g.gateway = ranked.source_gateway
			) THEN 1 ELSE 0 END, ranked.id DESC
			LIMIT `+fmt.Sprintf("$%d", 4+len(exclude)), append([]any{nowMS, consumerID, consumerID}, freePoolExcludeArgs(exclude, n)...)...)
		if err != nil {
			return err
		}
		defer rows.Close()
		var ids []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		probeUntil := now.Add(FreePoolUseValidationLease).UTC().UnixMilli()
		for _, id := range ids {
			token := uuid.NewString()
			result, err := tx.ExecContext(ctx, `INSERT INTO free_pool_probe_tickets (ticket_id, consumer_account_id, model, token, probe_until, created_at) VALUES ($1, $2, $3, $4, $5, $6)`, id, consumerID, model, token, probeUntil, nowMS)
			if err != nil {
				return err
			}
			if n, err := result.RowsAffected(); err != nil || n != 1 {
				return err
			}
			var pairJSON string
			claim := FreePoolProbeClaim{TicketID: id, ConsumerAccountID: consumerID, Token: token, Model: model}
			if err := tx.QueryRowContext(ctx, `SELECT pair, hard_expires_at FROM tickets WHERE id = $1`, id).Scan(&pairJSON, &claim.HardExpiresAt); err != nil {
				return err
			}
			if err := json.Unmarshal([]byte(pairJSON), &claim.Pair); err != nil {
				return ErrFreePoolInvalid
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO free_pool_gateway_uses (consumer_account_id, gateway, used_at)
				SELECT $1, source_gateway, $2 FROM tickets WHERE id = $3 AND source_gateway <> ''
				ON CONFLICT(consumer_account_id, gateway) DO UPDATE SET used_at = excluded.used_at`, consumerID, nowMS, id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO free_pool_first_uses (ticket_id, consumer_account_id, model, gateway, edge_colo, passed, reason, created_at)
				SELECT $1, $2, $3, COALESCE(source_gateway, ''),
				       CASE WHEN length(COALESCE(source_colo, '')) = 3 THEN source_colo ELSE '' END,
				       0, '', $4
				FROM tickets WHERE id = $5
				  AND NOT EXISTS (SELECT 1 FROM free_pool_first_uses WHERE ticket_id = $6 AND consumer_account_id = $7)`,
				id, consumerID, model, nowMS, id, id, consumerID); err != nil {
				return err
			}
			claims = append(claims, claim)
		}
		return nil
	})
	return claims, err
}

func (db *DB) WinFreePoolProbe(ctx context.Context, probe FreePoolProbeClaim, state string, now time.Time) error {
	if err := db.requireFreePool(); err != nil {
		return err
	}
	state = strings.TrimSpace(state)
	nowMS := now.UTC().UnixMilli()
	if probe.TicketID <= 0 || probe.ConsumerAccountID <= 0 || probe.Token == "" || probe.Model == "" || state == "" || nowMS <= 0 {
		return ErrFreePoolInvalid
	}
	return db.withFreePoolWriteTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `
			DELETE FROM free_pool_probe_tickets
			WHERE ticket_id = $1 AND consumer_account_id = $2 AND token = $3 AND probe_until > $4
			  AND EXISTS (
			  	SELECT 1 FROM tickets t
			  	JOIN free_pool_accounts source ON source.id = t.source_account_id
			  	WHERE t.id = $5 AND t.status = 'ready' AND t.hard_expires_at > $6
			  	  AND source.status = 'active' AND source.deleted_at IS NULL
			  )
			  AND NOT EXISTS (
			  	SELECT 1 FROM free_pool_account_rejections r
			  	WHERE r.ticket_id = $7 AND r.consumer_account_id = $8
			  )
			  AND EXISTS (
			  	SELECT 1 FROM accounts WHERE id = $9 AND COALESCE(use_tickets, FALSE) AND COALESCE(enabled, TRUE) AND `+freePoolConsumerSQL+`)`,
			probe.TicketID, probe.ConsumerAccountID, probe.Token, nowMS, probe.TicketID, nowMS, probe.TicketID, probe.ConsumerAccountID, probe.ConsumerAccountID)
		if err := freePoolUseRequireOne(result, err); err != nil {
			return ErrFreePoolProbeLost
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO free_pool_account_verifications (ticket_id, consumer_account_id, model, verified_at, consumer_state)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT(ticket_id, consumer_account_id, model) DO UPDATE SET
				consumer_state = excluded.consumer_state, verified_at = excluded.verified_at`,
			probe.TicketID, probe.ConsumerAccountID, probe.Model, nowMS, state); err != nil {
			return err
		}
		result, err = tx.ExecContext(ctx, `
			INSERT INTO ticket_bindings (ticket_id, consumer_account_id, status, bound_at)
			VALUES ($1, $2, 'bound', $3)
			ON CONFLICT(consumer_account_id) DO UPDATE SET
				ticket_id = excluded.ticket_id, status = 'bound', bound_at = excluded.bound_at
			WHERE ticket_bindings.status <> 'bound'`, probe.TicketID, probe.ConsumerAccountID, nowMS)
		if err := freePoolUseRequireOne(result, err); err != nil {
			return ErrFreePoolProbeLost
		}
		if _, err := tx.ExecContext(ctx, `UPDATE tickets SET status = 'leased' WHERE id = $1 AND status = 'ready'`, probe.TicketID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE free_pool_first_uses SET passed = 1, reason = '' WHERE ticket_id = $1 AND consumer_account_id = $2 AND passed = 0`, probe.TicketID, probe.ConsumerAccountID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO free_pool_first_uses (ticket_id, consumer_account_id, model, gateway, edge_colo, passed, reason, created_at)
			SELECT $1, $2, $3, COALESCE(source_gateway, ''),
			       CASE WHEN length(COALESCE(source_colo, '')) = 3 THEN source_colo ELSE '' END,
			       1, '', $4
			FROM tickets WHERE id = $5
			  AND NOT EXISTS (SELECT 1 FROM free_pool_first_uses WHERE ticket_id = $6 AND consumer_account_id = $7)`,
			probe.TicketID, probe.ConsumerAccountID, probe.Model, nowMS, probe.TicketID, probe.TicketID, probe.ConsumerAccountID); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO probe_observations (ticket_id, stage, status, new_state, error_code, created_at) VALUES ($1, 'followup', 'pass', 0, '', $2)`, probe.TicketID, nowMS)
		return err
	})
}

func (db *DB) SettleFreePoolProbe(ctx context.Context, probe FreePoolProbeClaim, outcome FreePoolMintOutcome, abandoned bool, now time.Time) error {
	if err := db.requireFreePool(); err != nil {
		return err
	}
	nowMS := now.UTC().UnixMilli()
	return db.withFreePoolWriteTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `DELETE FROM free_pool_probe_tickets WHERE ticket_id = $1 AND token = $2`, probe.TicketID, probe.Token)
		if err != nil {
			return err
		}
		rows, err := result.RowsAffected()
		if err != nil || rows == 0 || abandoned {
			if abandoned && rows == 1 {
				_, err = tx.ExecContext(ctx, `DELETE FROM free_pool_first_uses WHERE ticket_id = $1 AND consumer_account_id = $2 AND passed = 0 AND reason = ''`, probe.TicketID, probe.ConsumerAccountID)
			}
			return err
		}
		reason, reject := FreePoolRejectionReason(outcome)
		if !reject {
			probeStatus, code, changed, valid := freePoolMintObservation(outcome)
			if !valid {
				return nil
			}
			if _, err := tx.ExecContext(ctx, `UPDATE free_pool_first_uses SET reason = $1 WHERE ticket_id = $2 AND consumer_account_id = $3 AND passed = 0 AND reason = ''`, code, probe.TicketID, probe.ConsumerAccountID); err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO probe_observations (ticket_id, stage, status, new_state, error_code, created_at) VALUES ($1, 'followup', $2, $3, $4, $5)`, probe.TicketID, probeStatus, changed, code, nowMS)
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO free_pool_account_rejections (ticket_id, consumer_account_id, reason, created_at)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT(ticket_id, consumer_account_id) DO UPDATE SET reason = excluded.reason, created_at = excluded.created_at`,
			probe.TicketID, probe.ConsumerAccountID, reason, nowMS); err != nil {
			return err
		}
		var exclusive bool
		if err := tx.QueryRowContext(ctx, `SELECT exclusive_tickets = 1 FROM free_pool_settings WHERE id = 1`).Scan(&exclusive); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if exclusive {
			if _, err := tx.ExecContext(ctx, `UPDATE tickets SET status = 'retired' WHERE id = $1 AND status = 'ready'`, probe.TicketID); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE free_pool_first_uses SET reason = $1 WHERE ticket_id = $2 AND consumer_account_id = $3 AND passed = 0 AND reason = ''`, reason, probe.TicketID, probe.ConsumerAccountID); err != nil {
			return err
		}
		probeStatus, code, changed, _ := freePoolMintObservation(outcome)
		_, err = tx.ExecContext(ctx, `INSERT INTO probe_observations (ticket_id, stage, status, new_state, error_code, created_at) VALUES ($1, 'followup', $2, $3, $4, $5)`, probe.TicketID, probeStatus, changed, code, nowMS)
		return err
	})
}

func (db *DB) ReserveFreePoolSpareTickets(ctx context.Context, consumerID int64, model string, n int, now time.Time) ([]FreePoolSpareClaim, error) {
	if err := db.requireFreePool(); err != nil {
		return nil, err
	}
	nowMS := now.UTC().UnixMilli()
	model = strings.TrimSpace(model)
	if consumerID <= 0 || nowMS <= 0 || n < 1 || model == "" || len(model) > 128 {
		return nil, ErrFreePoolInvalid
	}
	var spares []FreePoolSpareClaim
	err := db.withFreePoolWriteTx(ctx, func(tx *sql.Tx) error {
		if err := sweepFreePoolHolds(ctx, tx, nowMS); err != nil {
			return err
		}
		// 号还开着就能备票。主票已经死了也要能备，测好后同一事务里接上。
		var allowed bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM accounts WHERE id = $1 AND COALESCE(use_tickets, FALSE) AND COALESCE(enabled, TRUE) AND `+freePoolConsumerSQL+`)`, consumerID).Scan(&allowed); err != nil {
			return err
		}
		if !allowed {
			return sql.ErrNoRows
		}
		var held int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM free_pool_spare_tickets WHERE consumer_account_id = $1 AND phase = 'ready'`, consumerID).Scan(&held); err != nil {
			return err
		}
		if held > 0 {
			return sql.ErrNoRows
		}
		rows, err := tx.QueryContext(ctx, `
			SELECT ranked.id FROM (
				SELECT t.id, t.source_gateway, ROW_NUMBER() OVER (
					PARTITION BY t.source_gateway
					ORDER BY t.issued_at DESC, t.id DESC
				) AS gateway_rank
				FROM tickets t
				JOIN free_pool_accounts source ON source.id = t.source_account_id
				WHERE t.status = 'ready' AND t.hard_expires_at > $1
				  AND source.status = 'active' AND source.deleted_at IS NULL
				  AND NOT EXISTS (SELECT 1 FROM ticket_bindings b WHERE b.ticket_id = t.id AND b.status <> 'released')
				  AND NOT EXISTS (SELECT 1 FROM free_pool_spare_tickets s WHERE s.ticket_id = t.id)
				  AND NOT EXISTS (SELECT 1 FROM free_pool_probe_tickets p WHERE p.ticket_id = t.id)
				  AND NOT EXISTS (SELECT 1 FROM free_pool_account_rejections r WHERE r.ticket_id = t.id AND r.consumer_account_id = $2)
				  AND NOT EXISTS (SELECT 1 FROM free_pool_account_verifications v WHERE v.ticket_id = t.id AND v.consumer_account_id = $2)
			) ranked
			ORDER BY ranked.gateway_rank, CASE WHEN EXISTS (
				SELECT 1 FROM free_pool_gateway_uses g WHERE g.consumer_account_id = $3 AND g.gateway = ranked.source_gateway
			) THEN 1 ELSE 0 END, ranked.id DESC
			LIMIT $4`, nowMS, consumerID, consumerID, n)
		if err != nil {
			return err
		}
		defer rows.Close()
		var ids []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(ids) == 0 {
			return sql.ErrNoRows
		}
		probeUntil := now.Add(FreePoolUseValidationLease).UTC().UnixMilli()
		for _, id := range ids {
			token := uuid.NewString()
			spare := FreePoolSpareClaim{TicketID: id, ConsumerAccountID: consumerID, Token: token, Model: model}
			var pairJSON string
			if err := tx.QueryRowContext(ctx, `SELECT pair, hard_expires_at FROM tickets WHERE id = $1`, id).Scan(&pairJSON, &spare.HardExpiresAt); err != nil {
				return err
			}
			if err := json.Unmarshal([]byte(pairJSON), &spare.Pair); err != nil {
				return ErrFreePoolInvalid
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO free_pool_spare_tickets (consumer_account_id, ticket_id, model, token, phase, probe_until, created_at) VALUES ($1, $2, $3, $4, 'probing', $5, $6)`, consumerID, id, model, token, probeUntil, nowMS); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO free_pool_gateway_uses (consumer_account_id, gateway, used_at)
				SELECT $1, source_gateway, $2
				FROM tickets WHERE id = $3 AND source_gateway <> ''
				ON CONFLICT(consumer_account_id, gateway) DO UPDATE SET used_at = excluded.used_at`,
				consumerID, nowMS, id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO free_pool_first_uses (ticket_id, consumer_account_id, model, gateway, edge_colo, passed, reason, created_at)
				SELECT $1, $2, $3, COALESCE(source_gateway, ''),
				       CASE WHEN length(COALESCE(source_colo, '')) = 3 THEN source_colo ELSE '' END,
				       0, '', $4
				FROM tickets WHERE id = $5
				  AND NOT EXISTS (SELECT 1 FROM free_pool_first_uses WHERE ticket_id = $6 AND consumer_account_id = $7)`,
				id, consumerID, model, nowMS, id, id, consumerID); err != nil {
				return err
			}
			spares = append(spares, spare)
		}
		return nil
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, sql.ErrNoRows
	}
	return spares, err
}

func (db *DB) CommitFreePoolSpareTicket(ctx context.Context, spare FreePoolSpareClaim, state string, now time.Time) error {
	if err := db.requireFreePool(); err != nil {
		return err
	}
	state = strings.TrimSpace(state)
	nowMS := now.UTC().UnixMilli()
	if spare.TicketID <= 0 || spare.ConsumerAccountID <= 0 || spare.Token == "" || spare.Model == "" || state == "" || nowMS <= 0 {
		return ErrFreePoolInvalid
	}
	return db.withFreePoolWriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM free_pool_spare_tickets WHERE consumer_account_id = $1 AND phase = 'ready' AND ticket_id <> $2`, spare.ConsumerAccountID, spare.TicketID); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `
			UPDATE free_pool_spare_tickets SET phase = 'ready'
			WHERE consumer_account_id = $1 AND ticket_id = $2 AND token = $3 AND phase = 'probing' AND probe_until > $4
			  AND EXISTS (
			  	SELECT 1 FROM tickets t
			  	JOIN free_pool_accounts source ON source.id = t.source_account_id
			  	WHERE t.id = $5 AND t.status = 'ready' AND t.hard_expires_at > $6
			  	  AND source.status = 'active' AND source.deleted_at IS NULL
			  )`, spare.ConsumerAccountID, spare.TicketID, spare.Token, nowMS, spare.TicketID, nowMS)
		if err := freePoolUseRequireOne(result, err); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO free_pool_account_verifications (ticket_id, consumer_account_id, model, verified_at, consumer_state)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT(ticket_id, consumer_account_id, model) DO UPDATE SET
				consumer_state = excluded.consumer_state, verified_at = excluded.verified_at`,
			spare.TicketID, spare.ConsumerAccountID, spare.Model, nowMS, state); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE free_pool_first_uses SET passed = 1, reason = '' WHERE ticket_id = $1 AND consumer_account_id = $2 AND passed = 0`, spare.TicketID, spare.ConsumerAccountID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO probe_observations (ticket_id, stage, status, new_state, error_code, created_at) VALUES ($1, 'followup', 'pass', 0, '', $2)`, spare.TicketID, nowMS); err != nil {
			return err
		}
		// 主票如果在预备票测好之前已经死了，这里立刻接上，不等下一次用户领票。
		_, err = promoteFreePoolSpareTx(ctx, tx, spare.ConsumerAccountID, nowMS)
		return err
	})
}

func (db *DB) DropFreePoolSpareTicket(ctx context.Context, spare FreePoolSpareClaim, outcome FreePoolMintOutcome, now time.Time) error {
	if err := db.requireFreePool(); err != nil {
		return err
	}
	nowMS := now.UTC().UnixMilli()
	return db.withFreePoolWriteTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `DELETE FROM free_pool_spare_tickets WHERE consumer_account_id = $1 AND token = $2 AND phase = 'probing'`, spare.ConsumerAccountID, spare.Token)
		if err != nil {
			return err
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return err
		}
		reason, reject := FreePoolRejectionReason(outcome)
		if rows == 0 || !reject {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO free_pool_account_rejections (ticket_id, consumer_account_id, reason, created_at)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT(ticket_id, consumer_account_id) DO UPDATE SET reason = excluded.reason, created_at = excluded.created_at`,
			spare.TicketID, spare.ConsumerAccountID, reason, nowMS); err != nil {
			return err
		}
		var exclusive bool
		if err := tx.QueryRowContext(ctx, `SELECT exclusive_tickets = 1 FROM free_pool_settings WHERE id = 1`).Scan(&exclusive); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if exclusive {
			if _, err := tx.ExecContext(ctx, `UPDATE tickets SET status = 'retired' WHERE id = $1 AND status = 'ready'`, spare.TicketID); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE free_pool_first_uses SET reason = $1 WHERE ticket_id = $2 AND consumer_account_id = $3 AND passed = 0 AND reason = ''`, reason, spare.TicketID, spare.ConsumerAccountID); err != nil {
			return err
		}
		probeStatus, code, changed, _ := freePoolMintObservation(outcome)
		_, err = tx.ExecContext(ctx, `INSERT INTO probe_observations (ticket_id, stage, status, new_state, error_code, created_at) VALUES ($1, 'followup', $2, $3, $4, $5)`, spare.TicketID, probeStatus, changed, code, nowMS)
		return err
	})
}

// FreePoolConnectedConsumerIDs 返回这个模型的票已经验过、并且绑定票还可用的消费号。
// 换号时优先用这些号，避免再打一轮双 400。
type FreePoolConsumerTicketState struct {
	AccountID  int64
	Connected  bool
	ProbeModel string
}

func (db *DB) FreePoolConsumerTicketStates(ctx context.Context, now time.Time) ([]FreePoolConsumerTicketState, error) {
	if err := db.requireFreePool(); err != nil {
		return nil, err
	}
	nowMS := now.UTC().UnixMilli()
	rows, err := db.conn.QueryContext(ctx, `
		SELECT a.id,
		       EXISTS(
		       	SELECT 1 FROM ticket_bindings b
		       	JOIN tickets t ON t.id = b.ticket_id
		       	JOIN free_pool_accounts source ON source.id = t.source_account_id
		       	WHERE b.consumer_account_id = a.id AND b.status = 'bound'
		       	  AND t.status IN ('ready', 'leased') AND t.hard_expires_at > $1
		       	  AND source.status = 'active' AND source.deleted_at IS NULL
		       	  AND EXISTS (
		       	  	SELECT 1 FROM free_pool_account_verifications v
		       	  	WHERE v.ticket_id = t.id AND v.consumer_account_id = a.id AND v.consumer_state <> ''
		       	  )
		       	  AND NOT EXISTS (
		       	  	SELECT 1 FROM free_pool_account_rejections r
		       	  	WHERE r.ticket_id = t.id AND r.consumer_account_id = a.id
		       	  )
		       ),
		       COALESCE((
		       	SELECT v.model FROM free_pool_account_verifications v
		       	WHERE v.consumer_account_id = a.id AND v.consumer_state <> ''
		       	ORDER BY v.verified_at DESC LIMIT 1
		       ), '')
		FROM accounts a
		WHERE COALESCE(a.use_tickets, FALSE) AND COALESCE(a.enabled, TRUE) AND `+freePoolConsumerSQL, nowMS)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FreePoolConsumerTicketState
	for rows.Next() {
		var state FreePoolConsumerTicketState
		var connected bool
		if err := rows.Scan(&state.AccountID, &connected, &state.ProbeModel); err != nil {
			return nil, err
		}
		state.Connected = connected
		out = append(out, state)
	}
	return out, rows.Err()
}

func (db *DB) FreePoolConnectedConsumerIDs(ctx context.Context, model string, now time.Time) (map[int64]struct{}, error) {
	out := map[int64]struct{}{}
	if err := db.requireFreePool(); err != nil {
		return out, err
	}
	nowMS := now.UTC().UnixMilli()
	model = strings.TrimSpace(model)
	if nowMS <= 0 || model == "" {
		return out, nil
	}
	rows, err := db.conn.QueryContext(ctx, `
		SELECT b.consumer_account_id
		FROM ticket_bindings b
		JOIN tickets t ON t.id = b.ticket_id
		JOIN free_pool_accounts source ON source.id = t.source_account_id
		JOIN free_pool_account_verifications v
		  ON v.ticket_id = t.id AND v.consumer_account_id = b.consumer_account_id
		 AND v.model = $1 AND v.consumer_state <> ''
		WHERE b.status = 'bound'
		  AND t.status IN ('ready', 'leased') AND t.hard_expires_at > $2
		  AND source.status = 'active' AND source.deleted_at IS NULL
		  AND NOT EXISTS (
		  	SELECT 1 FROM free_pool_account_rejections r
		  	WHERE r.ticket_id = t.id AND r.consumer_account_id = b.consumer_account_id
		  )`, model, nowMS)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return out, err
		}
		out[id] = struct{}{}
	}
	return out, rows.Err()
}

func (db *DB) GetAccountUseTickets(ctx context.Context, id int64) (bool, error) {
	if err := db.requireFreePool(); err != nil {
		return false, err
	}
	var enabled bool
	err := db.conn.QueryRowContext(ctx, `
		SELECT COALESCE(use_tickets, FALSE) FROM accounts
		WHERE id = $1 AND status <> 'deleted' AND COALESCE(error_message, '') <> 'deleted'`, id).Scan(&enabled)
	return enabled, err
}

func (db *DB) SetAccountUseTickets(ctx context.Context, id int64, enabled bool) error {
	if err := db.requireFreePool(); err != nil {
		return err
	}
	if id <= 0 {
		return ErrFreePoolInvalid
	}
	return db.withFreePoolWriteTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `
			UPDATE accounts SET use_tickets = $1, updated_at = CURRENT_TIMESTAMP
			WHERE id = $2 AND `+freePoolConsumerSQL, enabled, id)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n == 1 {
			return nil
		}
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM accounts WHERE id = $1)`, id).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return sql.ErrNoRows
		}
		return ErrFreePoolConsumer
	})
}

func freePoolUseRequireOne(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrFreePoolUseLeaseLost
	}
	return nil
}

// ClaimFreePoolReadyTicket 领取一张仍在硬截止时间内、还没被任何消费账号占用的票。
// 一张票同时只属于一个消费账号；这个账号的后续请求继续用同一张。
// freePoolClaimExpirySQL 必须和测试共用。表别名是 t，全表扫描会显示成 SCAN t，不能靠 SCAN tickets 判断。
const freePoolClaimExpirySQL = `
		UPDATE ticket_bindings SET status = 'released'
		WHERE status = 'bound'
		  AND EXISTS (SELECT 1 FROM tickets t WHERE t.id = ticket_bindings.ticket_id AND t.hard_expires_at <= $1)
		  AND NOT EXISTS (
		  	SELECT 1 FROM free_pool_use_leases l
		  	WHERE l.ticket_id = ticket_bindings.ticket_id AND l.lease_until > $2
		  )`

func (db *DB) sweepFreePoolClaimExpiry(ctx context.Context, tx *sql.Tx, nowMS int64) error {
	last := db.freePoolClaimSweep.Load()
	if last > 0 && nowMS-last < int64(2*time.Second/time.Millisecond) {
		return nil
	}
	if _, err := tx.ExecContext(ctx, freePoolClaimExpirySQL, nowMS, nowMS); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tickets SET status = CASE WHEN hard_expires_at <= $1 THEN 'expired' ELSE 'ready' END WHERE status = 'leased' AND id IN (SELECT ticket_id FROM free_pool_use_leases WHERE lease_until <= $2)`, nowMS, nowMS); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM free_pool_use_leases WHERE lease_until <= $1`, nowMS); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tickets SET status = 'expired' WHERE status = 'ready' AND hard_expires_at <= $1`, nowMS); err != nil {
		return err
	}
	db.freePoolClaimSweep.Store(nowMS)
	return nil
}

func (db *DB) ClaimFreePoolReadyTicket(ctx context.Context, consumerID int64, model string, now time.Time) (FreePoolUseLease, error) {
	var claim FreePoolUseLease
	if err := db.requireFreePool(); err != nil {
		return claim, err
	}
	nowMS := now.UTC().UnixMilli()
	model = strings.TrimSpace(model)
	if consumerID <= 0 || nowMS <= 0 || model == "" || len(model) > 128 {
		return claim, ErrFreePoolInvalid
	}
	err := db.withFreePoolWriteTx(ctx, func(tx *sql.Tx) error {
		if err := db.sweepFreePoolClaimExpiry(ctx, tx, nowMS); err != nil {
			return err
		}
		var allowed bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM accounts WHERE id = $1 AND COALESCE(use_tickets, FALSE) AND COALESCE(enabled, TRUE) AND `+freePoolConsumerSQL+`)`, consumerID).Scan(&allowed); err != nil {
			return err
		}
		if !allowed {
			return ErrFreePoolConsumer
		}
		promotedID, err := promoteFreePoolSpareTx(ctx, tx, consumerID, nowMS)
		if err != nil {
			return err
		}
		promoted := promotedID > 0
		var pairJSON string
		// 这里只续用已经测过的绑定，或升级测好的备用票。没测过的新票由并发探测占用，不能提前绑上。
		err = tx.QueryRowContext(ctx, `
			SELECT t.id, t.pair, t.hard_expires_at FROM tickets t
			JOIN ticket_bindings b ON b.ticket_id = t.id AND b.consumer_account_id = $1 AND b.status = 'bound'
			JOIN free_pool_accounts source ON source.id = t.source_account_id
			WHERE t.status IN ('ready', 'leased') AND t.hard_expires_at > $2
			  AND source.status = 'active' AND source.deleted_at IS NULL
			  AND EXISTS (
			  	SELECT 1 FROM free_pool_account_verifications v
			  	WHERE v.ticket_id = t.id AND v.consumer_account_id = b.consumer_account_id AND v.consumer_state <> ''
			  )
			  AND NOT EXISTS (SELECT 1 FROM free_pool_account_rejections r WHERE r.ticket_id = t.id AND r.consumer_account_id = $3)`,
			consumerID, nowMS, consumerID).Scan(&claim.TicketID, &pairJSON, &claim.HardExpiresAt)
		if errors.Is(err, sql.ErrNoRows) {
			claim.TicketID = 0
			return nil
		}
		if err != nil {
			return err
		}
		if err := json.Unmarshal([]byte(pairJSON), &claim.Pair); err != nil {
			return ErrFreePoolInvalid
		}
		claim.ConsumerAccountID = consumerID
		claim.Token = uuid.NewString()
		claim.Promoted = promoted
		// 只恢复这个消费账号自己验证得到的 state。没有记录，或旧记录是空串，都必须重新验证。
		claim.Model = model
		err = tx.QueryRowContext(ctx, `
			SELECT v.consumer_state FROM free_pool_account_verifications v
			WHERE v.ticket_id = $1 AND v.consumer_account_id = $2 AND v.model = $3 AND v.consumer_state <> ''
			  AND NOT EXISTS (
				SELECT 1 FROM free_pool_account_rejections r
				WHERE r.ticket_id = v.ticket_id AND r.consumer_account_id = v.consumer_account_id
			  )`, claim.TicketID, consumerID, model).Scan(&claim.ConsumerState)
		if errors.Is(err, sql.ErrNoRows) {
			claim.Verified = false
			claim.ConsumerState = ""
		} else if err != nil {
			return err
		} else if claim.ConsumerState != "" {
			claim.Verified = true
		} else {
			claim.Verified = false
		}
		if !claim.Verified {
			err = tx.QueryRowContext(ctx, `
				SELECT EXISTS(
					SELECT 1 FROM free_pool_account_verifications v
					WHERE v.ticket_id = $1 AND v.consumer_account_id = $2 AND v.model <> $3 AND v.consumer_state <> ''
				) AND NOT EXISTS (
					SELECT 1 FROM free_pool_account_rejections r
					WHERE r.ticket_id = $4 AND r.consumer_account_id = $5
				)`, claim.TicketID, consumerID, model, claim.TicketID, consumerID).Scan(&claim.Sibling)
			if err != nil {
				return err
			}
		}
		leaseUntil := now.Add(FreePoolUseValidationLease).UTC().UnixMilli()
		if claim.HardExpiresAt < leaseUntil {
			leaseUntil = claim.HardExpiresAt
		}
		claim.LeaseUntil = leaseUntil
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO ticket_bindings (ticket_id, consumer_account_id, status, bound_at)
			VALUES ($1, $2, 'bound', $3)
			ON CONFLICT(consumer_account_id) DO UPDATE SET
				ticket_id = excluded.ticket_id, status = 'bound', bound_at = excluded.bound_at
			WHERE ticket_bindings.status <> 'bound' OR ticket_bindings.ticket_id = excluded.ticket_id`,
			claim.TicketID, consumerID, nowMS); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE tickets SET status = 'leased' WHERE id = $1 AND status = 'ready' AND hard_expires_at > $2`, claim.TicketID, nowMS); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO free_pool_gateway_uses (consumer_account_id, gateway, used_at)
			SELECT $1, source_gateway, $2
			FROM tickets WHERE id = $3 AND source_gateway <> ''
			ON CONFLICT(consumer_account_id, gateway) DO UPDATE SET used_at = excluded.used_at`,
			consumerID, nowMS, claim.TicketID); err != nil {
			return err
		}
		if !claim.Verified {
			if _, err := tx.ExecContext(ctx, `INSERT INTO free_pool_first_uses (ticket_id, consumer_account_id, model, gateway, edge_colo, passed, reason, created_at)
				SELECT $1, $2, $3, COALESCE((SELECT source_gateway FROM tickets WHERE id = $4), ''),
				       CASE WHEN length(COALESCE((SELECT source_colo FROM tickets WHERE id = $5), '')) = 3 THEN (SELECT source_colo FROM tickets WHERE id = $6) ELSE '' END,
				       0, '', $7
				WHERE NOT EXISTS (
					SELECT 1 FROM free_pool_first_uses WHERE ticket_id = $8 AND consumer_account_id = $9
				)`, claim.TicketID, consumerID, model, claim.TicketID, claim.TicketID, claim.TicketID, nowMS, claim.TicketID, consumerID); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO free_pool_use_leases (ticket_id, lease_token, lease_until, phase) VALUES ($1, $2, $3, 'verifying')`, claim.TicketID, claim.Token, claim.LeaseUntil)
		return err
	})
	if err != nil {
		return FreePoolUseLease{}, err
	}
	if claim.TicketID == 0 {
		return FreePoolUseLease{}, ErrFreePoolNeedsProbe
	}
	return claim, nil
}

func (db *DB) ActivateFreePoolUse(ctx context.Context, claim FreePoolUseLease, now time.Time) error {
	if err := db.requireFreePool(); err != nil {
		return err
	}
	if claim.ConsumerState == "" {
		return ErrFreePoolInvalid
	}
	nowMS := now.UTC().UnixMilli()
	return db.withFreePoolWriteTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `
			UPDATE free_pool_use_leases
			SET phase = 'active', lease_until = (SELECT hard_expires_at FROM tickets WHERE id = $1)
			WHERE ticket_id = $2 AND lease_token = $3 AND phase = 'verifying' AND lease_until > $4
			  AND EXISTS (
				SELECT 1 FROM tickets t
				JOIN ticket_bindings b ON b.ticket_id = t.id
				JOIN free_pool_accounts source ON source.id = t.source_account_id
				WHERE t.id = free_pool_use_leases.ticket_id AND t.status = 'leased' AND t.hard_expires_at > $5
				  AND b.consumer_account_id = $6 AND b.status = 'bound'
				  AND source.status = 'active' AND source.deleted_at IS NULL
			  )
			  AND EXISTS (
				SELECT 1 FROM accounts WHERE id = $7 AND COALESCE(use_tickets, FALSE)
				  AND COALESCE(enabled, TRUE) AND `+freePoolConsumerSQL+`)`,
			claim.TicketID, claim.TicketID, claim.Token, nowMS, nowMS, claim.ConsumerAccountID, claim.ConsumerAccountID)
		if err := freePoolUseRequireOne(result, err); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO probe_observations (ticket_id, stage, status, new_state, error_code, created_at) VALUES ($1, 'followup', 'pass', 0, '', $2)`, claim.TicketID, nowMS); err != nil {
			return err
		}
		var existing string
		if _, err := tx.ExecContext(ctx, `UPDATE free_pool_first_uses SET passed = 1, reason = '' WHERE ticket_id = $1 AND consumer_account_id = $2 AND passed = 0`, claim.TicketID, claim.ConsumerAccountID); err != nil {
			return err
		}
		err = tx.QueryRowContext(ctx, `SELECT consumer_state FROM free_pool_account_verifications WHERE ticket_id = $1 AND consumer_account_id = $2 AND model = $3`, claim.TicketID, claim.ConsumerAccountID, claim.Model).Scan(&existing)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			_, err = tx.ExecContext(ctx, `INSERT INTO free_pool_account_verifications (ticket_id, consumer_account_id, model, verified_at, consumer_state) VALUES ($1, $2, $3, $4, $5)`, claim.TicketID, claim.ConsumerAccountID, claim.Model, nowMS, claim.ConsumerState)
		case err != nil:
			return err
		case existing == "":
			result, err = tx.ExecContext(ctx, `UPDATE free_pool_account_verifications SET consumer_state = $1, verified_at = $2 WHERE ticket_id = $3 AND consumer_account_id = $4 AND model = $5 AND consumer_state = ''`, claim.ConsumerState, nowMS, claim.TicketID, claim.ConsumerAccountID, claim.Model)
			err = freePoolUseRequireOne(result, err)
		case existing == claim.ConsumerState:
			err = nil
		default:
			return ErrFreePoolInvalid
		}
		return err
	})
}

func (db *DB) ActivateFreePoolSiblingUse(ctx context.Context, claim FreePoolUseLease, now time.Time) error {
	if err := db.requireFreePool(); err != nil {
		return err
	}
	if !claim.Sibling || claim.Verified || claim.ConsumerState != "" {
		return ErrFreePoolInvalid
	}
	nowMS := now.UTC().UnixMilli()
	return db.withFreePoolWriteTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `
			UPDATE free_pool_use_leases
			SET phase = 'active', lease_until = (SELECT hard_expires_at FROM tickets WHERE id = $1)
			WHERE ticket_id = $2 AND lease_token = $3 AND phase = 'verifying' AND lease_until > $4
			  AND EXISTS (
				SELECT 1 FROM free_pool_account_verifications v
				WHERE v.ticket_id = $5 AND v.consumer_account_id = $6 AND v.model <> $7 AND v.consumer_state <> ''
			  )
			  AND NOT EXISTS (
				SELECT 1 FROM free_pool_account_rejections r
				WHERE r.ticket_id = $8 AND r.consumer_account_id = $9
			  )`,
			claim.TicketID, claim.TicketID, claim.Token, nowMS,
			claim.TicketID, claim.ConsumerAccountID, claim.Model,
			claim.TicketID, claim.ConsumerAccountID)
		return freePoolUseRequireOne(result, err)
	})
}

func (db *DB) CommitFreePoolMintedState(ctx context.Context, claim FreePoolUseLease, state string, now time.Time) (FreePoolMintCommit, error) {
	if err := db.requireFreePool(); err != nil {
		return "", err
	}
	state = strings.TrimSpace(state)
	if claim.TicketID <= 0 || claim.ConsumerAccountID <= 0 || claim.Token == "" || claim.Model == "" || state == "" {
		return "", ErrFreePoolInvalid
	}
	nowMS := now.UTC().UnixMilli()
	var commit FreePoolMintCommit
	err := db.withFreePoolWriteTx(ctx, func(tx *sql.Tx) error {
		var leased bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM free_pool_use_leases WHERE ticket_id = $1 AND lease_token = $2 AND phase = 'active' AND lease_until > $3)`, claim.TicketID, claim.Token, nowMS).Scan(&leased); err != nil {
			return err
		}
		if !leased {
			return ErrFreePoolUseLeaseLost
		}
		var rejected bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM free_pool_account_rejections WHERE ticket_id = $1 AND consumer_account_id = $2)`, claim.TicketID, claim.ConsumerAccountID).Scan(&rejected); err != nil {
			return err
		}
		if rejected {
			commit = FreePoolMintRejected
			return nil
		}
		var copied bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM free_pool_account_verifications WHERE ticket_id = $1 AND consumer_account_id = $2 AND consumer_state = $3)`, claim.TicketID, claim.ConsumerAccountID, state).Scan(&copied); err != nil {
			return err
		}
		if copied {
			commit = FreePoolMintCopied
			return nil
		}
		result, err := tx.ExecContext(ctx, `
			INSERT INTO free_pool_account_verifications (ticket_id, consumer_account_id, model, verified_at, consumer_state)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT(ticket_id, consumer_account_id, model) DO UPDATE
			  SET consumer_state = excluded.consumer_state, verified_at = excluded.verified_at
			WHERE free_pool_account_verifications.consumer_state = ''`,
			claim.TicketID, claim.ConsumerAccountID, claim.Model, nowMS, state)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n == 1 {
			commit = FreePoolMintStored
			return nil
		}
		var existing string
		err = tx.QueryRowContext(ctx, `SELECT consumer_state FROM free_pool_account_verifications WHERE ticket_id = $1 AND consumer_account_id = $2 AND model = $3`, claim.TicketID, claim.ConsumerAccountID, claim.Model).Scan(&existing)
		if err != nil {
			return err
		}
		if existing == state {
			commit = FreePoolMintStored
			return nil
		}
		commit = FreePoolMintLost
		return nil
	})
	return commit, err
}

func (db *DB) CheckFreePoolUse(ctx context.Context, claim FreePoolUseLease, now time.Time) error {
	if err := db.requireFreePool(); err != nil {
		return err
	}
	nowMS := now.UTC().UnixMilli()
	var valid bool
	err := db.conn.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM free_pool_use_leases l
			JOIN tickets t ON t.id = l.ticket_id
			JOIN free_pool_accounts source ON source.id = t.source_account_id
			WHERE l.ticket_id = $1 AND l.lease_token = $2 AND l.phase = 'active' AND l.lease_until > $3
			  AND t.status IN ('leased', 'ready') AND t.hard_expires_at > $4
			  AND source.status = 'active' AND source.deleted_at IS NULL
			  AND EXISTS (
				SELECT 1 FROM accounts WHERE id = $5 AND COALESCE(use_tickets, FALSE)
				  AND COALESCE(enabled, TRUE) AND `+freePoolConsumerSQL+`)
			  AND (
				($6 = 0 AND EXISTS (
					SELECT 1 FROM free_pool_account_verifications v
					WHERE v.ticket_id = l.ticket_id AND v.consumer_account_id = $7 AND v.model = $8 AND v.consumer_state = $9 AND v.consumer_state <> ''))
				OR ($10 = 1 AND EXISTS (
					SELECT 1 FROM free_pool_account_verifications v
					WHERE v.ticket_id = l.ticket_id AND v.consumer_account_id = $11 AND v.model <> $12 AND v.consumer_state <> '')
				  AND NOT EXISTS (
					SELECT 1 FROM free_pool_account_rejections r
					WHERE r.ticket_id = l.ticket_id AND r.consumer_account_id = $13))
			  )
		)`, claim.TicketID, claim.Token, nowMS, nowMS, claim.ConsumerAccountID,
		boolToInt(claim.Sibling), claim.ConsumerAccountID, claim.Model, claim.ConsumerState,
		boolToInt(claim.Sibling), claim.ConsumerAccountID, claim.Model, claim.ConsumerAccountID).Scan(&valid)
	if err != nil {
		return err
	}
	if valid {
		return nil
	}
	var rejected bool
	if err := db.conn.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM free_pool_account_rejections WHERE ticket_id = $1 AND consumer_account_id = $2)`, claim.TicketID, claim.ConsumerAccountID).Scan(&rejected); err != nil {
		return err
	}
	if rejected {
		return ErrFreePoolTicketRejected
	}
	return ErrFreePoolUseLeaseLost
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

// MarkFreePoolTicketSwitching 在发现降智的那一刻把这个账号的绑定标成换票中，并记下这张票不能再用。
// 不等下一次领票。已经发出去的连接不在这里掐断。
// NoteFreePoolFirstTokenStall 记下这张票对这个账号又一次首字超时。达到上限就隔离，下次领票会换。
func (db *DB) NoteFreePoolFirstTokenStall(ctx context.Context, claim FreePoolUseLease, now time.Time) (bool, error) {
	if err := db.requireFreePool(); err != nil {
		return false, err
	}
	if claim.TicketID <= 0 || claim.ConsumerAccountID <= 0 {
		return false, ErrFreePoolInvalid
	}
	settings, err := db.GetFreePoolMintSettings(ctx)
	if err != nil {
		return false, err
	}
	limit := settings.FirstTokenStrikes
	if limit < 1 {
		limit = FreePoolFirstTokenStrikesDefault
	}
	nowMS := now.UTC().UnixMilli()
	var isolate bool
	err = db.withFreePoolWriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO free_pool_ticket_stalls (ticket_id, consumer_account_id, stalls)
			VALUES ($1, $2, 1)
			ON CONFLICT(ticket_id, consumer_account_id) DO UPDATE SET stalls = free_pool_ticket_stalls.stalls + 1`,
			claim.TicketID, claim.ConsumerAccountID); err != nil {
			return err
		}
		var stalls int
		if err := tx.QueryRowContext(ctx, `SELECT stalls FROM free_pool_ticket_stalls WHERE ticket_id = $1 AND consumer_account_id = $2`, claim.TicketID, claim.ConsumerAccountID).Scan(&stalls); err != nil {
			return err
		}
		if stalls < limit {
			return nil
		}
		isolate = true
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO free_pool_account_rejections (ticket_id, consumer_account_id, reason, created_at)
			VALUES ($1, $2, 'invalid_response', $3)
			ON CONFLICT(ticket_id, consumer_account_id) DO UPDATE SET reason = excluded.reason, created_at = excluded.created_at`,
			claim.TicketID, claim.ConsumerAccountID, nowMS); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE ticket_bindings SET status = 'pending' WHERE consumer_account_id = $1 AND ticket_id = $2 AND status = 'bound'`, claim.ConsumerAccountID, claim.TicketID); err != nil {
			return err
		}
		_, err = promoteFreePoolSpareTx(ctx, tx, claim.ConsumerAccountID, nowMS)
		return err
	})
	return isolate, err
}

func (db *DB) ClearFreePoolFirstTokenStall(ctx context.Context, claim FreePoolUseLease) error {
	if err := db.requireFreePool(); err != nil {
		return err
	}
	return db.withFreePoolWriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM free_pool_ticket_stalls WHERE ticket_id = $1 AND consumer_account_id = $2`, claim.TicketID, claim.ConsumerAccountID)
		return err
	})
}

// MarkFreePoolTicketSwitching 记下这张票不能再用。有测好的预备票时，同一事务里升成当前绑定，返回升上去的票号。
func (db *DB) MarkFreePoolTicketSwitching(ctx context.Context, claim FreePoolUseLease, now time.Time) (int64, error) {
	if err := db.requireFreePool(); err != nil {
		return 0, err
	}
	if claim.TicketID <= 0 || claim.ConsumerAccountID <= 0 {
		return 0, ErrFreePoolInvalid
	}
	nowMS := now.UTC().UnixMilli()
	var promoted int64
	err := db.withFreePoolWriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO free_pool_account_rejections (ticket_id, consumer_account_id, reason, created_at)
			VALUES ($1, $2, 'state_changed', $3)
			ON CONFLICT(ticket_id, consumer_account_id) DO UPDATE SET reason = excluded.reason, created_at = excluded.created_at`,
			claim.TicketID, claim.ConsumerAccountID, nowMS); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE ticket_bindings SET status = 'pending' WHERE consumer_account_id = $1 AND ticket_id = $2 AND status = 'bound'`, claim.ConsumerAccountID, claim.TicketID); err != nil {
			return err
		}
		id, err := promoteFreePoolSpareTx(ctx, tx, claim.ConsumerAccountID, nowMS)
		promoted = id
		return err
	})
	return promoted, err
}

func (db *DB) FinishFreePoolUse(ctx context.Context, claim FreePoolUseLease, outcome FreePoolMintOutcome, now time.Time) error {
	if err := db.requireFreePool(); err != nil {
		return err
	}
	if outcome != "" {
		if _, _, _, ok := freePoolMintObservation(outcome); !ok {
			return ErrFreePoolInvalid
		}
	}
	nowMS := now.UTC().UnixMilli()
	return db.withFreePoolWriteTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `DELETE FROM free_pool_use_leases WHERE lease_token = $1 AND lease_until > $2`, claim.Token, nowMS)
		if err := freePoolUseRequireOne(result, err); err != nil {
			return err
		}
		nextStatus := "ready"
		var exclusive bool
		if err := tx.QueryRowContext(ctx, `SELECT exclusive_tickets = 1 FROM free_pool_settings WHERE id = 1`).Scan(&exclusive); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		// 独占打开时，这个账号双 400 失败后票不再给别的账号。关闭时只隔离这个账号。
		if exclusive && (outcome == FreePoolMintDegraded || outcome == FreePoolMintInvalidResponse) {
			nextStatus = "retired"
		}
		if outcome == FreePoolMintDegraded || outcome == FreePoolMintInvalidResponse {
			reason := "state_changed"
			if outcome == FreePoolMintInvalidResponse {
				reason = "invalid_response"
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO free_pool_account_rejections (ticket_id, consumer_account_id, reason, created_at)
				VALUES ($1, $2, $3, $4)
				ON CONFLICT(ticket_id, consumer_account_id) DO UPDATE SET reason = excluded.reason, created_at = excluded.created_at`,
				claim.TicketID, claim.ConsumerAccountID, reason, nowMS); err != nil {
				return err
			}
		}
		var holders int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM free_pool_use_leases WHERE ticket_id = $1 AND lease_until > $2`, claim.TicketID, nowMS).Scan(&holders); err != nil {
			return err
		}
		if holders == 0 {
			result, err = tx.ExecContext(ctx, `
				UPDATE tickets SET status = CASE WHEN hard_expires_at <= $1 THEN 'expired' ELSE $2 END
				WHERE id = $3 AND status = 'leased'`, nowMS, nextStatus, claim.TicketID)
			if err := freePoolUseRequireOne(result, err); err != nil {
				return err
			}
		}
		// 双 400 失败才换账号绑定。正常结束或别的请求还在用时，绑定留着，已建立的连接也不动。
		// 换票期间先占成 pending，避免同一账号的其他请求把这张失效票当成已验证直接复用。
		if outcome == FreePoolMintDegraded || outcome == FreePoolMintInvalidResponse {
			if _, err := tx.ExecContext(ctx, `UPDATE ticket_bindings SET status = 'pending' WHERE ticket_id = $1 AND consumer_account_id = $2 AND status = 'bound'`, claim.TicketID, claim.ConsumerAccountID); err != nil {
				return err
			}
			if _, err := promoteFreePoolSpareTx(ctx, tx, claim.ConsumerAccountID, nowMS); err != nil {
				return err
			}
		}
		// 首次双 400 只要这次领票没有通过，就记失败原因。客户端中途取消不算失败，留空继续算进行中。
		if outcome != "" && outcome != FreePoolMintInterrupted {
			reason := string(outcome)
			if outcome == FreePoolMintDegraded {
				reason = "state_changed"
			}
			if _, err := tx.ExecContext(ctx, `UPDATE free_pool_first_uses SET reason = $1 WHERE ticket_id = $2 AND consumer_account_id = $3 AND passed = 0 AND reason = ''`, reason, claim.TicketID, claim.ConsumerAccountID); err != nil {
				return err
			}
		}
		if outcome == "" {
			return nil
		}
		probeStatus, code, changed, _ := freePoolMintObservation(outcome)
		_, err = tx.ExecContext(ctx, `INSERT INTO probe_observations (ticket_id, stage, status, new_state, error_code, created_at) VALUES ($1, 'followup', $2, $3, $4, $5)`, claim.TicketID, probeStatus, changed, code, nowMS)
		return err
	})
}

// FreePoolClaimRecordCounts 是一次清除领票记录的计数。票和来源账号不在这里删。
type FreePoolClaimRecordCounts struct {
	Rejections    int64 `json:"rejections"`
	Verifications int64 `json:"verifications"`
	Bindings      int64 `json:"bindings"`
	Leases        int64 `json:"leases"`
	Spares        int64 `json:"spares"`
	Probes        int64 `json:"probes"`
	Released      int64 `json:"released"`
}

// ClearFreePoolClaimRecords 清掉消费侧领票痕迹：拒绝、验证、绑定和租约。
// 仍在租约里的票放回可领取；已经过硬截止的改成过期。不删除票，也不动来源账号。
func (db *DB) ClearFreePoolClaimRecords(ctx context.Context, now time.Time) (FreePoolClaimRecordCounts, error) {
	var counts FreePoolClaimRecordCounts
	if err := db.requireFreePool(); err != nil {
		return counts, err
	}
	nowMS := now.UTC().UnixMilli()
	if nowMS <= 0 {
		return counts, ErrFreePoolInvalid
	}
	err := db.withFreePoolWriteTx(ctx, func(tx *sql.Tx) error {
		var err error
		if counts.Rejections, err = freePoolDeleteCount(ctx, tx, `DELETE FROM free_pool_account_rejections`); err != nil {
			return err
		}
		if _, err = freePoolDeleteCount(ctx, tx, `DELETE FROM free_pool_gateway_uses`); err != nil {
			return err
		}
		if counts.Verifications, err = freePoolDeleteCount(ctx, tx, `DELETE FROM free_pool_account_verifications`); err != nil {
			return err
		}
		if counts.Bindings, err = freePoolDeleteCount(ctx, tx, `DELETE FROM ticket_bindings`); err != nil {
			return err
		}
		if counts.Leases, err = freePoolDeleteCount(ctx, tx, `DELETE FROM free_pool_use_leases`); err != nil {
			return err
		}
		if counts.Spares, err = freePoolDeleteCount(ctx, tx, `DELETE FROM free_pool_spare_tickets`); err != nil {
			return err
		}
		if counts.Probes, err = freePoolDeleteCount(ctx, tx, `DELETE FROM free_pool_probe_tickets`); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `
			UPDATE tickets
			SET status = CASE WHEN hard_expires_at <= $1 THEN 'expired' ELSE 'ready' END
			WHERE status = 'leased'`, nowMS)
		if err != nil {
			return err
		}
		counts.Released, err = result.RowsAffected()
		return err
	})
	return counts, err
}

func freePoolDeleteCount(ctx context.Context, tx *sql.Tx, query string) (int64, error) {
	result, err := tx.ExecContext(ctx, query)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

type FreePoolFirstUseGateway struct {
	Gateway     string   `json:"gateway"`
	EdgeColo    string   `json:"edge_colo"`
	Edges       []string `json:"edges"`
	Total       int64    `json:"total"`
	Passed      int64    `json:"passed"`
	Failed      int64    `json:"failed"`
	Pending     int64    `json:"pending"`
	UnusedReady int64    `json:"unused_ready"`
}

type FreePoolFirstUseStats struct {
	Total    int64                     `json:"total"`
	Passed   int64                     `json:"passed"`
	Failed   int64                     `json:"failed"`
	Pending  int64                     `json:"pending"`
	Nodes    int64                     `json:"nodes"`
	Gateways []FreePoolFirstUseGateway `json:"gateways"`
}

func (db *DB) FreePoolFirstUseStats(ctx context.Context, since time.Time) (FreePoolFirstUseStats, error) {
	var stats FreePoolFirstUseStats
	if err := db.requireFreePool(); err != nil {
		return stats, err
	}
	sinceMS := since.UTC().UnixMilli()
	err := db.conn.QueryRowContext(ctx, `
		SELECT COUNT(*),
		       COALESCE(SUM(CASE WHEN passed = 1 THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN passed = 0 AND reason <> '' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN passed = 0 AND reason = '' THEN 1 ELSE 0 END), 0)
		FROM free_pool_first_uses WHERE created_at >= $1`, sinceMS).Scan(&stats.Total, &stats.Passed, &stats.Failed, &stats.Pending)
	if err != nil {
		return stats, err
	}
	rows, err := db.conn.QueryContext(ctx, `
		SELECT gateway, edge_colo,
		       COUNT(*),
		       COALESCE(SUM(CASE WHEN passed = 1 THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN passed = 0 AND reason <> '' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN passed = 0 AND reason = '' THEN 1 ELSE 0 END), 0),
		       0
		FROM free_pool_first_uses
		WHERE created_at >= $1
		GROUP BY gateway, edge_colo
		ORDER BY gateway, edge_colo`, sinceMS)
	if err != nil {
		return stats, err
	}
	defer rows.Close()
	for rows.Next() {
		var item FreePoolFirstUseGateway
		if err := rows.Scan(&item.Gateway, &item.EdgeColo, &item.Total, &item.Passed, &item.Failed, &item.Pending, &item.UnusedReady); err != nil {
			return stats, err
		}
		if item.EdgeColo != "" {
			item.Edges = []string{item.EdgeColo}
		} else {
			item.Edges = []string{}
		}
		stats.Gateways = append(stats.Gateways, item)
	}
	if err := rows.Err(); err != nil {
		return stats, err
	}
	nowMS := time.Now().UTC().UnixMilli()
	if nowMS < sinceMS {
		nowMS = sinceMS
	}
	index := map[string]int{}
	seen := map[string]struct{}{}
	for i, item := range stats.Gateways {
		if item.Gateway != "" {
			seen[item.Gateway] = struct{}{}
		}
		if item.EdgeColo == "" {
			if _, exists := index[item.Gateway]; !exists {
				index[item.Gateway] = i
			}
		}
	}
	unused, err := db.conn.QueryContext(ctx, `
		SELECT source_gateway, COUNT(*)
		FROM tickets t
		WHERE t.status = 'ready' AND t.hard_expires_at > $1
		  AND NOT EXISTS (SELECT 1 FROM free_pool_first_uses f WHERE f.ticket_id = t.id)
		  AND NOT EXISTS (SELECT 1 FROM free_pool_account_verifications v WHERE v.ticket_id = t.id)
		  AND NOT EXISTS (SELECT 1 FROM ticket_bindings b WHERE b.ticket_id = t.id AND b.status = 'bound')
		GROUP BY source_gateway`, nowMS)
	if err != nil {
		return stats, err
	}
	defer unused.Close()
	for unused.Next() {
		var gateway string
		var count int64
		if err := unused.Scan(&gateway, &count); err != nil {
			return stats, err
		}
		if gateway != "" {
			seen[gateway] = struct{}{}
		}
		if at, ok := index[gateway]; ok {
			stats.Gateways[at].UnusedReady = count
			continue
		}
		stats.Gateways = append(stats.Gateways, FreePoolFirstUseGateway{Gateway: gateway, Edges: []string{}, UnusedReady: count})
	}
	if err := unused.Err(); err != nil {
		return stats, err
	}
	var discovered int64
	if err := db.conn.QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT source_gateway) FROM tickets
		WHERE status IN ('ready', 'leased') AND hard_expires_at > $1 AND source_gateway <> ''`, nowMS).Scan(&discovered); err != nil {
		return stats, err
	}
	stats.Nodes = discovered
	if int64(len(seen)) > stats.Nodes {
		stats.Nodes = int64(len(seen))
	}
	if stats.Gateways == nil {
		stats.Gateways = []FreePoolFirstUseGateway{}
	}
	return stats, nil
}

func (db *DB) FreePoolSparePhases(ctx context.Context, ids []int64) (map[int64]string, error) {
	out := map[int64]string{}
	if err := db.requireFreePool(); err != nil || len(ids) == 0 {
		return out, err
	}
	placeholders := make([]string, len(ids))
	for i := range ids {
		placeholders[i] = fmt.Sprintf("$%d", i+1)
	}
	query := `SELECT consumer_account_id, CASE WHEN SUM(CASE WHEN phase = 'ready' THEN 1 ELSE 0 END) > 0 THEN 'ready' WHEN SUM(CASE WHEN phase = 'probing' THEN 1 ELSE 0 END) > 0 THEN 'probing' ELSE '' END FROM free_pool_spare_tickets WHERE consumer_account_id IN (` + strings.Join(placeholders, ",") + `) GROUP BY consumer_account_id`
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := db.conn.QueryContext(ctx, query, args...)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var phase string
		if err := rows.Scan(&id, &phase); err != nil {
			return out, err
		}
		out[id] = phase
	}
	return out, rows.Err()
}

func (db *DB) FreePoolAccountRejectionReason(ctx context.Context, consumerID int64) (string, error) {
	if err := db.requireFreePool(); err != nil {
		return "", err
	}
	var reason string
	err := db.conn.QueryRowContext(ctx, `SELECT reason FROM free_pool_account_rejections WHERE consumer_account_id = $1 ORDER BY created_at DESC LIMIT 1`, consumerID).Scan(&reason)
	return reason, err
}
