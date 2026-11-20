package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// InsertUsageHistory logs a single request's token usage to the usageHistory table.
func (r *Repo) InsertUsageHistory(provider, model, connectionID, apiKey, endpoint string, promptTokens, completionTokens int, cost float64, status string, totalTokens int, meta string, tokensJSON string) error {
	timestamp := time.Now().UTC().Format(time.RFC3339)
	_, err := r.db.Exec(
		`INSERT INTO usageHistory (timestamp, provider, model, connectionId, apiKey, endpoint, promptTokens, completionTokens, cost, status, tokens, meta)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		timestamp, provider, model, connectionID, apiKey, endpoint, promptTokens, completionTokens, cost, status, tokensJSON, meta,
	)
	if err != nil {
		return fmt.Errorf("insert usage history: %w", err)
	}
	return nil
}

// MergeUsageDaily atomically reads, transforms, and writes a daily usage record inside
// a SQLite BEGIN IMMEDIATE transaction so concurrent processes sharing the same SQLite
// WAL file serialize safely without lost updates.
func (r *Repo) MergeUsageDaily(dateKey string, merge func(existing string) (string, error)) error {
	ctx := context.Background()
	conn, err := r.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire usage conn: %w", err)
	}
	defer conn.Close()

	_, _ = conn.ExecContext(ctx, "PRAGMA busy_timeout = 5000")
	for attempt := 0; attempt < 5; attempt++ {
		if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err == nil {
			break
		}
		if !strings.Contains(err.Error(), "SQLITE_BUSY") && !strings.Contains(err.Error(), "database is locked") {
			return fmt.Errorf("begin immediate daily usage %s: %w", dateKey, err)
		}
		time.Sleep(time.Duration(15*(attempt+1)) * time.Millisecond)
	}
	if err != nil {
		return fmt.Errorf("begin immediate daily usage %s: %w", dateKey, err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(ctx, "ROLLBACK")
		}
	}()

	var existing string
	err = conn.QueryRowContext(ctx, `SELECT data FROM usageDaily WHERE dateKey = ?`, dateKey).Scan(&existing)
	if err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("read daily usage %s: %w", dateKey, err)
	}

	nextData, err := merge(existing)
	if err != nil {
		return err
	}

	if _, err := conn.ExecContext(ctx, `INSERT OR REPLACE INTO usageDaily (dateKey, data) VALUES (?, ?)`, dateKey, nextData); err != nil {
		return fmt.Errorf("upsert daily usage %s: %w", dateKey, err)
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("commit daily usage %s: %w", dateKey, err)
	}
	committed = true
	return nil
}

// InsertRequestDetail logs a request detail record for the Recent Requests dashboard tab.
func (r *Repo) InsertRequestDetail(id, provider, model, connectionID, status string, data string) error {
	timestamp := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	_, err := r.db.Exec(
		`INSERT OR IGNORE INTO requestDetails (id, timestamp, provider, model, connectionId, status, data) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, timestamp, provider, model, connectionID, status, data,
	)
	if err != nil {
		return fmt.Errorf("insert request detail %s: %w", id, err)
	}
	return nil
}

// UpdateConnectionLastUsed updates the lastUsedAt timestamp and increments
// consecutiveUseCount for the given provider connection.
func (r *Repo) UpdateConnectionLastUsed(connectionID string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := r.db.Exec(
		`UPDATE providerConnections SET lastUsedAt = ?, consecutiveUseCount = COALESCE(consecutiveUseCount, 0) + 1 WHERE id = ?`,
		now, connectionID,
	)
	if err != nil {
		return fmt.Errorf("update connection last used %s: %w", connectionID, err)
	}
	return nil
}
