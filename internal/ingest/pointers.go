package ingest

import (
	"context"
	"database/sql"
	"path/filepath"
	"regexp"
	"strings"
)

// savedRE matches Claude Code's message form
// `Output too large (...). Full output saved to: <path>` at the start of the
// tool_result text or of a line. Bare tool-results paths quoted elsewhere
// (Read or grep output, prose) are not pointers.
var savedRE = regexp.MustCompile(`(?m)^(?:[A-Za-z]+: )?Output too large[^\n]*?Full output saved to:[ \t]*(\S[^\n]*?/tool-results/[^\s"'<>)\]]+)`)

// SavedPath returns the tool-results file a tool_result text points to (its
// first pointer), or "".
func SavedPath(text string) string {
	if !strings.Contains(text, "/tool-results/") {
		return ""
	}
	if m := savedRE.FindStringSubmatch(text); m != nil {
		return strings.TrimRight(m[1], ".,;:")
	}
	return ""
}

type queryExec interface {
	ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
}

var likeEsc = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// ResolvePointers sets tool_outputs.asset_id for pointers whose file is
// archived: first by exact path, then by the trailing
// <slug>/<sid>/tool-results/<file>, then <sid>/tool-results/<file> (the same
// file seen under another home directory spelling or a renamed project). It returns how many were set.
func ResolvePointers(ctx context.Context, q queryExec) (int, error) {
	if _, err := q.ExecContext(ctx, `UPDATE tool_outputs SET asset_id=(SELECT id FROM assets WHERE path=tool_outputs.saved_path)
		WHERE saved_path IS NOT NULL AND asset_id IS NULL AND EXISTS (SELECT 1 FROM assets WHERE path=tool_outputs.saved_path)`); err != nil {
		return 0, err
	}
	rows, err := q.QueryContext(ctx, `SELECT DISTINCT saved_path FROM tool_outputs WHERE saved_path IS NOT NULL AND asset_id IS NULL`)
	if err != nil {
		return 0, err
	}
	var open []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			rows.Close()
			return 0, err
		}
		open = append(open, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	n := 0
	for _, p := range open {
		parts := strings.Split(filepath.Clean(p), string(filepath.Separator))
		if len(parts) < 3 {
			continue
		}
		// <slug>/<sid>/tool-results/<file> first; the project slug changes when a
		// project directory is moved, so then <sid>/tool-results/<file>
		// (session ids are unique).
		var id int64
		found := false
		for _, n := range []int{4, 3} {
			if len(parts) < n {
				continue
			}
			suffix := "/" + strings.Join(parts[len(parts)-n:], "/")
			if q.QueryRowContext(ctx, `SELECT id FROM assets WHERE path LIKE ? ESCAPE '\' ORDER BY id LIMIT 1`, "%"+likeEsc.Replace(suffix)).Scan(&id) == nil {
				found = true
				break
			}
		}
		if !found {
			continue
		}
		r, err := q.ExecContext(ctx, `UPDATE tool_outputs SET asset_id=? WHERE saved_path=? AND asset_id IS NULL`, id, p)
		if err != nil {
			return n, err
		}
		c, _ := r.RowsAffected()
		n += int(c)
	}
	return n, nil
}
