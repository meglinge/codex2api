package database

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"runtime"
	"time"
)

// freePoolAdvisoryLockKey is the transaction-scoped advisory lock that
// serializes free-pool writes across PostgreSQL sessions. The constant is
// the ASCII of "fp_write".
const freePoolAdvisoryLockKey int64 = 0x66705f7772697465 // "fp_write"

// FreePoolSupported reports whether this database can host the free pool.
// A nil receiver is unsupported.
func (db *DB) FreePoolSupported() bool {
	return db != nil && (db.driver == "sqlite" || db.driver == "postgres")
}

func (db *DB) requireFreePool() error {
	if db == nil || !db.FreePoolSupported() {
		return ErrFreePoolUnsupported
	}
	if db.conn == nil {
		return errors.New("database unavailable")
	}
	return nil
}

// withFreePoolWriteTx serializes free-pool mutations. SQLite reuses the
// process-wide writer gate. PostgreSQL waits on a process-local semaphore
// and then takes pg_advisory_xact_lock so other processes are excluded too.
//
// Not re-entrant: fn must never call another free-pool write method.
func (db *DB) withFreePoolWriteTx(ctx context.Context, fn func(*sql.Tx) error) error {
	if err := db.requireFreePool(); err != nil {
		return err
	}
	if db.isSQLite() {
		return db.withWriteTx(ctx, fn)
	}
	if db.freePoolWriteSem == nil {
		return errors.New("free pool write lock unavailable")
	}

	// PC is captured before the wait so the slow-path log names the caller,
	// matching sqliteWriteLockCaller.
	callerPC, _, _, callerOK := runtime.Caller(1)
	waitStart := time.Now()
	select {
	case db.freePoolWriteSem <- struct{}{}:
	case <-ctx.Done():
		waited := time.Since(waitStart)
		if waited > time.Second {
			log.Printf("[free-pool] write lock slow caller=%s wait=%s hold=%s", sqliteWriteLockCaller(callerPC, callerOK), waited, time.Duration(0))
		}
		return ctx.Err()
	}
	waited := time.Since(waitStart)
	heldStart := time.Now()
	defer func() {
		held := time.Since(heldStart)
		<-db.freePoolWriteSem
		if waited > time.Second || held > 200*time.Millisecond {
			log.Printf("[free-pool] write lock slow caller=%s wait=%s hold=%s", sqliteWriteLockCaller(callerPC, callerOK), waited, held)
		}
	}()
	return db.runFreePoolPostgresWriteTx(ctx, fn)
}

func (db *DB) runFreePoolPostgresWriteTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, freePoolAdvisoryLockKey); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}
