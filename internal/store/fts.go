package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// Querier is the part of *sql.DB, *sql.Conn and *sql.Tx used by the FTS helpers.
type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// FTSTables lists every FTS5 virtual table in the archive (chunk_fts,
// tool_fts, asset_fts, file_fts and any added later), derived from
// sqlite_master, ordered by name.
func FTSTables(ctx context.Context, q Querier) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND sql LIKE 'CREATE VIRTUAL TABLE % USING fts5%' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// Execer is the part of *sql.DB, *sql.Conn and *sql.Tx that executes statements.
type Execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// FTSCommand runs an FTS5 special command ('rebuild', 'integrity-check', ...)
// on table.
func FTSCommand(ctx context.Context, e Execer, table, cmd string) error {
	if strings.ContainsAny(table, "\"` ") {
		return fmt.Errorf("fts table name %q", table)
	}
	if _, err := e.ExecContext(ctx, `INSERT INTO "`+table+`"("`+table+`") VALUES('`+cmd+`')`); err != nil {
		return fmt.Errorf("%s %s: %w", table, cmd, err)
	}
	return nil
}
