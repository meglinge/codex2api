package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

const FreePoolMintLease = 90 * time.Second

var ErrFreePoolMintLeaseLost = errors.New("free pool mint lease lost")

// FreePoolMintClaim 只给铸票进程。管理接口不得返回这个类型。
type FreePoolMintClaim struct {
	ID          int64           `json:"-"`
	ProxyURL    string          `json:"-"`
	Credentials json.RawMessage `json:"-"`
	ClaimedAt   int64           `json:"-"`
	LeaseUntil  int64           `json:"-"`
}

func (FreePoolMintClaim) Format(s fmt.State, _ rune) {
	_, _ = io.WriteString(s, "FreePoolMintClaim{redacted}")
}

type FreePoolMintOutcome string

const (
	FreePoolMintPass              FreePoolMintOutcome = "pass"
	FreePoolMintDegraded          FreePoolMintOutcome = "degraded"
	FreePoolMintTimeout           FreePoolMintOutcome = "timeout"
	FreePoolMintDisconnected      FreePoolMintOutcome = "disconnected"
	FreePoolMintIncomplete        FreePoolMintOutcome = "incomplete"
	FreePoolMintInvalidResponse   FreePoolMintOutcome = "invalid_response"
	FreePoolMintUpstreamError     FreePoolMintOutcome = "upstream_error"
	FreePoolMintRateLimited       FreePoolMintOutcome = "rate_limited"
	FreePoolMintBadCredentials    FreePoolMintOutcome = "bad_credentials"
	FreePoolMintUnsupportedEgress FreePoolMintOutcome = "unsupported_egress"
	FreePoolMintInterrupted       FreePoolMintOutcome = "interrupted"
	FreePoolMintSourceUnavailable FreePoolMintOutcome = "source_unavailable"
	FreePoolMintHardExpired       FreePoolMintOutcome = "hard_expired"
)

// FreePoolRejectionReason 是这个结果要不要把票从该消费号的选择库拿掉。
// 限流和凭据失败跟降智一样：这个号不能再领。独占打开时整张票退出选择库。
func FreePoolRejectionReason(outcome FreePoolMintOutcome) (string, bool) {
	switch outcome {
	case FreePoolMintDegraded:
		return "state_changed", true
	case FreePoolMintInvalidResponse:
		return "invalid_response", true
	case FreePoolMintRateLimited:
		return "rate_limited", true
	case FreePoolMintBadCredentials:
		return "bad_credentials", true
	default:
		return "", false
	}
}

func freePoolMintObservation(outcome FreePoolMintOutcome) (status, code string, newState any, valid bool) {
	switch outcome {
	case FreePoolMintPass:
		return "pass", "", 0, true
	case FreePoolMintDegraded:
		return "degraded", "", 1, true
	case FreePoolMintTimeout:
		return "unknown", "timeout", nil, true
	case FreePoolMintDisconnected:
		return "unknown", "disconnected", nil, true
	case FreePoolMintIncomplete, FreePoolMintInterrupted, FreePoolMintHardExpired:
		return "unknown", "incomplete", nil, true
	case FreePoolMintInvalidResponse, FreePoolMintUnsupportedEgress, FreePoolMintSourceUnavailable:
		return "unknown", "invalid_response", nil, true
	case FreePoolMintUpstreamError, FreePoolMintBadCredentials, FreePoolMintRateLimited:
		return "failed", "upstream_error", nil, true
	default:
		return "", "", nil, false
	}
}

// ClaimFreePoolMintAccount 原子领取一个已启用、未删除、冷却已结束的 free 账号。
func (db *DB) ClaimFreePoolMintAccount(ctx context.Context, now time.Time) (FreePoolMintClaim, error) {
	var claim FreePoolMintClaim
	if err := db.requireFreePool(); err != nil {
		return claim, err
	}
	nowMS := now.UTC().UnixMilli()
	if nowMS <= 0 {
		return claim, ErrFreePoolInvalid
	}
	claim.ClaimedAt = nowMS
	claim.LeaseUntil = now.Add(FreePoolMintLease).UTC().UnixMilli()
	var credentials string
	err := db.withFreePoolWriteTx(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			UPDATE free_pool_accounts
			SET cooldown_until = $1, updated_at = $2
			WHERE id = (
				SELECT id FROM free_pool_accounts
				WHERE status = 'active' AND deleted_at IS NULL
				  AND (cooldown_until IS NULL OR cooldown_until <= $3)
				ORDER BY COALESCE(cooldown_until, 0), id
				LIMIT 1
			)
			  AND status = 'active' AND deleted_at IS NULL
			  AND (cooldown_until IS NULL OR cooldown_until <= $4)
			RETURNING id, proxy_url, credentials`,
			claim.LeaseUntil, nowMS, nowMS, nowMS).Scan(&claim.ID, &claim.ProxyURL, &credentials)
	})
	if err != nil {
		return FreePoolMintClaim{}, err
	}
	claim.Credentials = json.RawMessage(credentials)
	return claim, nil
}

// FinishFreePoolMint 在同一事务里结束租约。ticketID 为 0 表示还没形成候选票。
func (db *DB) FinishFreePoolMint(ctx context.Context, claim FreePoolMintClaim, ticketID int64, outcome FreePoolMintOutcome, now time.Time, cooldown time.Duration) error {
	if err := db.requireFreePool(); err != nil {
		return err
	}
	if _, _, _, valid := freePoolMintObservation(outcome); !valid || claim.ID <= 0 || ticketID < 0 || now.UTC().UnixMilli() < claim.ClaimedAt || cooldown < time.Second || cooldown > 24*time.Hour || (outcome == FreePoolMintPass && ticketID == 0) {
		return ErrFreePoolInvalid
	}
	nowMS := now.UTC().UnixMilli()
	return db.withFreePoolWriteTx(ctx, func(tx *sql.Tx) error {
		var status, credentials string
		var deletedAt, lease sql.NullInt64
		err := tx.QueryRowContext(ctx, `SELECT status, deleted_at, cooldown_until, credentials FROM free_pool_accounts WHERE id = $1`, claim.ID).Scan(&status, &deletedAt, &lease, &credentials)
		if errors.Is(err, sql.ErrNoRows) || !lease.Valid || lease.Int64 != claim.LeaseUntil || lease.Int64 <= nowMS {
			if err == nil || errors.Is(err, sql.ErrNoRows) {
				return ErrFreePoolMintLeaseLost
			}
			return err
		}
		if err != nil {
			return err
		}
		// 铸票出口是全池轮转代理，领取后会改 claim.ProxyURL。不能拿它和账号自己保存的代理比较。
		if status != "active" || deletedAt.Valid || credentials != string(claim.Credentials) {
			outcome = FreePoolMintSourceUnavailable
		}
		if ticketID > 0 {
			var hardExpiresAt int64
			err = tx.QueryRowContext(ctx, `
				SELECT hard_expires_at FROM tickets
				WHERE id = $1 AND source_account_id = $2 AND status = 'candidate'
				  AND issued_at >= $3 AND issued_at < $4`, ticketID, claim.ID, claim.ClaimedAt, claim.LeaseUntil).Scan(&hardExpiresAt)
			if err != nil {
				return err
			}
			ticketStatus, reason := "quarantined", string(outcome)
			if hardExpiresAt <= nowMS {
				outcome = FreePoolMintHardExpired
				ticketStatus, reason = "expired", string(outcome)
			} else if outcome == FreePoolMintPass {
				ticketStatus, reason = "ready", ""
			}
			result, err := tx.ExecContext(ctx, `UPDATE tickets SET status = $1, quarantine_reason = $2 WHERE id = $3 AND source_account_id = $4 AND status = 'candidate'`, ticketStatus, reason, ticketID, claim.ID)
			if err != nil {
				return err
			}
			if n, err := result.RowsAffected(); err != nil || n != 1 {
				if err != nil {
					return err
				}
				return sql.ErrNoRows
			}
			probeStatus, code, newState, _ := freePoolMintObservation(outcome)
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO probe_observations (ticket_id, stage, status, new_state, error_code, created_at)
				VALUES ($1, 'qualification', $2, $3, $4, $5)`, ticketID, probeStatus, newState, code, nowMS); err != nil {
				return err
			}
		}
		lastError := string(outcome)
		nextCooldown := now.Add(cooldown).UTC().UnixMilli()
		if outcome == FreePoolMintPass {
			lastError = ""
		} else if outcome != FreePoolMintBadCredentials && outcome != FreePoolMintSourceUnavailable {
			// 普通失败先记次数。连续达到上限才进入冷却，避免一次抖动把号轮空。
			var strikes int
			if err := tx.QueryRowContext(ctx, `SELECT consecutive_failures FROM free_pool_accounts WHERE id = $1`, claim.ID).Scan(&strikes); err != nil {
				return err
			}
			strikesLimit := FreePoolMintStrikesDefault
			var limit sql.NullInt64
			if err := tx.QueryRowContext(ctx, `SELECT mint_failure_strikes FROM free_pool_settings WHERE id = 1`).Scan(&limit); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if limit.Int64 > 0 {
				strikesLimit = int(limit.Int64)
			}
			if strikes+1 < strikesLimit {
				nextCooldown = nowMS
			}
		}
		result, err := tx.ExecContext(ctx, `
			UPDATE free_pool_accounts
			SET cooldown_until = $1, last_error = $2, updated_at = $3,
			    consecutive_failures = CASE WHEN $2 = '' THEN 0 ELSE consecutive_failures + 1 END
			WHERE id = $4 AND cooldown_until = $5`, nextCooldown, lastError, nowMS, claim.ID, claim.LeaseUntil)
		if err != nil {
			return err
		}
		if n, err := result.RowsAffected(); err != nil || n != 1 {
			if err != nil {
				return err
			}
			return ErrFreePoolMintLeaseLost
		}
		return nil
	})
}

// SweepFreePoolMintTickets 隔离超时未完成的候选票，并把已到硬截止时间的票标为过期。
func (db *DB) SweepFreePoolMintTickets(ctx context.Context, now time.Time) error {
	if err := db.requireFreePool(); err != nil {
		return err
	}
	nowMS := now.UTC().UnixMilli()
	if nowMS <= 0 {
		return ErrFreePoolInvalid
	}
	staleMS := now.Add(-FreePoolMintLease).UTC().UnixMilli()
	return db.withFreePoolWriteTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT id, hard_expires_at FROM tickets
			WHERE status = 'candidate' AND (hard_expires_at <= $1 OR issued_at <= $2)
			ORDER BY id LIMIT 100`, nowMS, staleMS)
		if err != nil {
			return err
		}
		type candidate struct{ id, expires int64 }
		var candidates []candidate
		for rows.Next() {
			var item candidate
			if err := rows.Scan(&item.id, &item.expires); err != nil {
				_ = rows.Close()
				return err
			}
			candidates = append(candidates, item)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, item := range candidates {
			status, reason := "quarantined", "qualification_abandoned"
			if item.expires <= nowMS {
				status, reason = "expired", "hard_expired"
			}
			if _, err := tx.ExecContext(ctx, `UPDATE tickets SET status = $1, quarantine_reason = $2 WHERE id = $3 AND status = 'candidate'`, status, reason, item.id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO probe_observations (ticket_id, stage, status, new_state, error_code, created_at)
				VALUES ($1, 'qualification', 'unknown', NULL, 'incomplete', $2)`, item.id, nowMS); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, `
			UPDATE tickets SET status = 'expired'
			WHERE id IN (
				SELECT id FROM tickets
				WHERE status IN ('ready', 'quarantined') AND hard_expires_at <= $1
				ORDER BY id LIMIT 200
			)`, nowMS)
		return err
	})
}
