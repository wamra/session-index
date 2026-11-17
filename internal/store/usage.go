package store

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"time"
)

// Usage kinds (the per-day counters of the cost guards).
const (
	UsageSummarize = "summarize"
	UsageEmbed     = "embed"
)

// UsageKey is the meta key of the counter of kind for the UTC date of now.
func UsageKey(kind string, now time.Time) string {
	return "usage:" + kind + ":" + now.UTC().Format("2006-01-02")
}

// UsageToday returns how many items of kind were sent to the provider on the
// UTC date of now.
func UsageToday(ctx context.Context, db *sql.DB, kind string, now time.Time) (int, error) {
	var v string
	err := db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=?`, UsageKey(kind, now)).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(v)
}

// AddUsage adds n to today's counter of kind.
func AddUsage(ctx context.Context, db *sql.DB, kind string, n int, now time.Time) error {
	if n <= 0 {
		return nil
	}
	_, err := db.ExecContext(ctx, `INSERT INTO meta(key, value) VALUES(?, ?)
		ON CONFLICT(key) DO UPDATE SET value = CAST(CAST(value AS INTEGER) + ? AS TEXT)`, UsageKey(kind, now), strconv.Itoa(n), n)
	return err
}
