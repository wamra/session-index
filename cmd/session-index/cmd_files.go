package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

type fileChangeRow struct {
	ID        int64  `json:"id"`
	TS        string `json:"ts"`
	SessionID string `json:"session_id"`
	Profile   string `json:"profile"`
	Tool      string `json:"tool"`
	FilePath  string `json:"file_path"`
	Op        string `json:"op"`
	Added     int    `json:"added_chars"`
	Removed   int    `json:"removed_chars"`
	ResultOK  *int   `json:"result_ok"`
	RecordID  int64  `json:"record_id"`
	ResultRec *int64 `json:"result_record_id"`
	Summary   string `json:"summary"`
}

func (r fileChangeRow) status() string {
	switch {
	case r.ResultOK == nil:
		return "?"
	case *r.ResultOK == 0:
		return "err"
	}
	return "ok"
}

const fileChangeCols = `fc.id, COALESCE(fc.ts,''), COALESCE(fc.session_id,''), COALESCE(fc.profile,''), fc.tool, fc.file_path, COALESCE(fc.op,''),
	COALESCE(fc.added_chars,0), COALESCE(fc.removed_chars,0), fc.result_ok, fc.record_id, fc.result_record_id, COALESCE(s.text,'')`

func scanChanges(rows *sql.Rows) ([]fileChangeRow, error) {
	defer rows.Close()
	var out []fileChangeRow
	for rows.Next() {
		var r fileChangeRow
		var ok sql.NullInt64
		var res sql.NullInt64
		if err := rows.Scan(&r.ID, &r.TS, &r.SessionID, &r.Profile, &r.Tool, &r.FilePath, &r.Op, &r.Added, &r.Removed, &ok, &r.RecordID, &res, &r.Summary); err != nil {
			return nil, err
		}
		if ok.Valid {
			v := int(ok.Int64)
			r.ResultOK = &v
		}
		if res.Valid {
			v := res.Int64
			r.ResultRec = &v
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// cmdFiles lists the file changes whose path contains SUBSTRING, newest first.
func cmdFiles(args []string, out, errw io.Writer) int {
	fs := newFlags("files", errw)
	profile := fs.String("profile", "", "only changes from this profile")
	asJSON := fs.Bool("json", false, "print JSON")
	limit := fs.Int("limit", 200, "max changes (0 = all)")
	pos, err := parseInterspersed(fs, args)
	if err != nil || len(pos) != 1 || pos[0] == "" {
		fmt.Fprintln(errw, "files: exactly one SUBSTRING is required")
		return 2
	}
	st, err := openStore()
	if err != nil {
		fmt.Fprintf(errw, "open archive: %v\n", err)
		return 1
	}
	defer st.Close()
	sub := pos[0]
	var where []string
	var qargs []any
	if len([]rune(sub)) >= 3 { // trigram index
		where = append(where, `fc.id IN (SELECT change_id FROM file_fts WHERE file_fts MATCH ?)`)
		qargs = append(qargs, `"`+strings.ReplaceAll(sub, `"`, `""`)+`"`)
	}
	// exact, case-sensitive substring check on top of the (case-insensitive) index
	where = append(where, `instr(fc.file_path, ?) > 0`)
	qargs = append(qargs, sub)
	if *profile != "" {
		where = append(where, `fc.profile = ?`)
		qargs = append(qargs, *profile)
	}
	q := `SELECT ` + fileChangeCols + ` FROM file_changes fc LEFT JOIN summaries s ON s.change_id=fc.id WHERE ` +
		strings.Join(where, " AND ") + ` ORDER BY fc.ts DESC, fc.id DESC`
	if *limit > 0 {
		q += fmt.Sprintf(` LIMIT %d`, *limit)
	}
	rows, err := st.DB().QueryContext(context.Background(), q, qargs...)
	if err != nil {
		fmt.Fprintf(errw, "files: %v\n", err)
		return 1
	}
	changes, err := scanChanges(rows)
	if err != nil {
		fmt.Fprintf(errw, "files: %v\n", err)
		return 1
	}
	if *asJSON {
		if changes == nil {
			changes = []fileChangeRow{}
		}
		b, err := json.MarshalIndent(map[string]any{"changes": changes}, "", "  ")
		if err != nil {
			fmt.Fprintf(errw, "files: %v\n", err)
			return 1
		}
		fmt.Fprintln(out, string(b))
		return 0
	}
	for _, c := range changes {
		sid := c.SessionID
		if len(sid) > 8 {
			sid = sid[:8]
		}
		line := fmt.Sprintf("%s %s %s %-9s %-3s %s", c.TS, c.Profile, sid, c.Tool, c.status(), c.FilePath)
		if c.Summary != "" {
			line += " — " + snippet(c.Summary, 200)
		}
		fmt.Fprintln(out, line)
	}
	return 0
}

// showChanges prints a session's file changes grouped by file.
func showChanges(ctx context.Context, db *sql.DB, sid string, out, errw io.Writer) int {
	rows, err := db.QueryContext(ctx, `SELECT `+fileChangeCols+` FROM file_changes fc LEFT JOIN summaries s ON s.change_id=fc.id
		WHERE fc.session_id=? ORDER BY fc.file_path, fc.ts, fc.id`, sid)
	if err != nil {
		fmt.Fprintf(errw, "show: %v\n", err)
		return 1
	}
	changes, err := scanChanges(rows)
	if err != nil {
		fmt.Fprintf(errw, "show: %v\n", err)
		return 1
	}
	if len(changes) == 0 {
		fmt.Fprintf(errw, "show: no file changes for session %s\n", sid)
		return 1
	}
	last := ""
	for _, c := range changes {
		if c.FilePath != last {
			fmt.Fprintf(out, "%s\n", c.FilePath)
			last = c.FilePath
		}
		line := fmt.Sprintf("  %s %-9s %-3s +%d/-%d", c.TS, c.Tool, c.status(), c.Added, c.Removed)
		if c.Summary != "" {
			line += " — " + snippet(c.Summary, 200)
		}
		fmt.Fprintln(out, line)
	}
	return 0
}
