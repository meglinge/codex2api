package database

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// QualityTestPrompt is a reusable prompt preset for the quality test studio.
// The built-in pelican prompt is never stored; the studio falls back to it when
// no preset is selected.
type QualityTestPrompt struct {
	ID         int64      `json:"id"`
	Name       string     `json:"name"`
	Prompt     string     `json:"prompt"`
	UsageCount int        `json:"usage_count"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
}

func (db *DB) ensureQualityTestPromptSchema(ctx context.Context) error {
	idType, timeType := "BIGSERIAL PRIMARY KEY", "TIMESTAMPTZ"
	if db.isSQLite() {
		idType, timeType = "INTEGER PRIMARY KEY AUTOINCREMENT", "TIMESTAMP"
	}
	statements := []string{
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS quality_test_prompts (
		 id %s, name TEXT NOT NULL, prompt TEXT NOT NULL,
		 usage_count INTEGER NOT NULL DEFAULT 0, last_used_at %s,
		 created_at %s NOT NULL DEFAULT CURRENT_TIMESTAMP, updated_at %s NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`, idType, timeType, timeType, timeType),
		`CREATE INDEX IF NOT EXISTS idx_quality_test_prompts_updated ON quality_test_prompts(updated_at DESC)`,
	}
	for _, statement := range statements {
		if _, err := db.conn.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

const qualityTestPromptColumns = `id, name, prompt, usage_count, last_used_at, created_at, updated_at`

func (db *DB) ListQualityTestPrompts(ctx context.Context) ([]QualityTestPrompt, error) {
	rows, err := db.conn.QueryContext(ctx, `SELECT `+qualityTestPromptColumns+` FROM quality_test_prompts ORDER BY updated_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]QualityTestPrompt, 0)
	for rows.Next() {
		item, err := scanQualityTestPrompt(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, *item)
	}
	return result, rows.Err()
}

func (db *DB) GetQualityTestPrompt(ctx context.Context, id int64) (*QualityTestPrompt, error) {
	rows, err := db.conn.QueryContext(ctx, `SELECT `+qualityTestPromptColumns+` FROM quality_test_prompts WHERE id=$1`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, sql.ErrNoRows
	}
	return scanQualityTestPrompt(rows)
}

func (db *DB) InsertQualityTestPrompt(ctx context.Context, name, prompt string) (int64, error) {
	return db.insertRowID(ctx,
		`INSERT INTO quality_test_prompts (name, prompt) VALUES ($1,$2) RETURNING id`,
		`INSERT INTO quality_test_prompts (name, prompt) VALUES ($1,$2)`,
		strings.TrimSpace(name), prompt)
}

func (db *DB) UpdateQualityTestPrompt(ctx context.Context, id int64, name, prompt string) error {
	result, err := db.conn.ExecContext(ctx, `UPDATE quality_test_prompts SET name=$1, prompt=$2, updated_at=CURRENT_TIMESTAMP WHERE id=$3`, strings.TrimSpace(name), prompt, id)
	if err != nil {
		return err
	}
	if affected, err := result.RowsAffected(); err == nil && affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (db *DB) DeleteQualityTestPrompt(ctx context.Context, id int64) error {
	_, err := db.conn.ExecContext(ctx, `DELETE FROM quality_test_prompts WHERE id=$1`, id)
	return err
}

// Usage bookkeeping is best effort; a missing preset is not an error for the job.
func (db *DB) IncrementQualityTestPromptUsage(ctx context.Context, id int64) error {
	if id <= 0 {
		return nil
	}
	_, err := db.conn.ExecContext(ctx, `UPDATE quality_test_prompts SET usage_count = usage_count + 1, last_used_at = CURRENT_TIMESTAMP WHERE id=$1`, id)
	return err
}

func scanQualityTestPrompt(scanner interface{ Scan(...any) error }) (*QualityTestPrompt, error) {
	var item QualityTestPrompt
	var lastUsed, created, updated any
	if err := scanner.Scan(&item.ID, &item.Name, &item.Prompt, &item.UsageCount, &lastUsed, &created, &updated); err != nil {
		return nil, err
	}
	var err error
	if item.CreatedAt, err = parseDBTimeValue(created); err != nil {
		return nil, err
	}
	if item.UpdatedAt, err = parseDBTimeValue(updated); err != nil {
		return nil, err
	}
	value, err := parseDBNullTimeValue(lastUsed)
	if err != nil {
		return nil, err
	}
	if value.Valid {
		item.LastUsedAt = &value.Time
	}
	return &item, nil
}
