// Package restore rewrites a session's transcript files byte-for-byte from the
// archive. Every stored line is zstd-compressed without its trailing newline,
// so output is each live record in line_no order followed by "\n".
package restore

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wamra/session-index/internal/paths"
	"github.com/wamra/session-index/internal/store"
	"github.com/wamra/session-index/internal/zst"
)

// prefixBytes matches ingest's prefix hash length (files.sha256_prefix).
const prefixBytes = 4096

type fileRow struct {
	id     int64
	path   string
	kind   string
	prefix string // archived files.sha256_prefix; empty when unset
	offset int64  // archived files."offset": total ingested bytes
	mode   int64  // original permission bits; 0 = unknown (restored as 0600)
	mtime  int64  // original mtime in unix nanoseconds; 0 = unknown
}

// assetRow is one archived non-transcript file of the session.
type assetRow struct {
	id          int64
	path, kind  string
	mode, mtime int64
	size        int64
	sha         string
	raw         []byte
}

const defaultMode = 0o600

// querier is satisfied by *sql.DB and *sql.Tx.
type querier interface {
	QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error)
}

// Session restores every archived file of sessionID (main, subagent and
// workflow files). With inPlace false the files are written under toDir at
// their path relative to the projects root. With inPlace true they are written
// to their original paths. In both modes, if any target already exists the
// call fails before anything is written, and placement never overwrites a
// file (os.Link fails with EEXIST). All records are read in one read
// transaction and validated before any file is written. It returns the paths
// placed by this call.
func Session(ctx context.Context, st *store.Store, sessionID, toDir string, inPlace bool) ([]string, error) {
	return SessionOptions(ctx, st, sessionID, toDir, Options{InPlace: inPlace})
}

// Options selects what SessionOptions restores and where.
type Options struct {
	// InPlace writes the files to their original paths instead of under toDir.
	InPlace bool
	// FileHistory also writes the session's file-history snapshots.
	FileHistory bool
	// ProfileDirs also writes the session's uploads/ and tasks/ files, and
	// implies FileHistory.
	ProfileDirs bool
}

// SessionOpts is Session with the file-history switch: snapshots under the
// profile's file-history directory (not under projects/) are written only
// when withFileHistory is true. Besides transcripts, the session's archived
// assets are written in the same tree, and every file gets its original mode
// (0600 when unknown) and mtime.
func SessionOpts(ctx context.Context, st *store.Store, sessionID, toDir string, inPlace, withFileHistory bool) ([]string, error) {
	return SessionOptions(ctx, st, sessionID, toDir, Options{InPlace: inPlace, FileHistory: withFileHistory})
}

// SessionOptions is Session with the optional asset groups of Options.
// Pseudo-sessions such as "plans:<profile>" restore their files without any
// option.
func SessionOptions(ctx context.Context, st *store.Store, sessionID, toDir string, opts Options) ([]string, error) {
	inPlace := opts.InPlace
	withFileHistory := opts.FileHistory || opts.ProfileDirs
	tx, err := st.DB().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	files, err := sessionFiles(ctx, tx, sessionID)
	if err != nil {
		return nil, err
	}
	// A per-file id ("plans:<profile>/<name>") names one file of the
	// pseudo-session its asset is stored under.
	assetSID, docRel := paths.SplitDocSession(sessionID)
	assets, err := sessionAssets(ctx, tx, assetSID, withFileHistory, opts.ProfileDirs)
	if err != nil {
		return nil, err
	}
	if docRel != "" {
		assets = docAssets(assets, docRel)
	}
	if len(files) == 0 && len(assets) == 0 {
		return nil, fmt.Errorf("restore: no files for session %s", sessionID)
	}

	targets := make([]string, len(files)+len(assets))
	for i, f := range files {
		if inPlace {
			targets[i] = f.path
			continue
		}
		rel, err := relPath(f.path, sessionID, f.kind)
		if err != nil {
			return nil, err
		}
		targets[i] = filepath.Join(toDir, rel)
	}
	for j, a := range assets {
		i := len(files) + j
		if inPlace {
			targets[i] = a.path
			continue
		}
		rel, err := assetRel(a.path, assetSID, a.kind)
		if err != nil {
			return nil, err
		}
		targets[i] = filepath.Join(toDir, rel)
	}

	// Fast fail: refuse before any work if a target already exists.
	for _, t := range targets {
		if _, err := os.Lstat(t); err == nil {
			return nil, fmt.Errorf("restore: target %s already exists", t)
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("restore: stat %s: %w", t, err)
		}
	}

	// Validate every file before writing anything.
	for _, f := range files {
		if err := validate(ctx, tx, f); err != nil {
			return nil, err
		}
	}
	for _, a := range assets {
		if _, err := assetBytes(a); err != nil {
			return nil, err
		}
	}

	// Phase 1: write every file to a temp path in its target directory.
	temps := make([]string, 0, len(files)+len(assets))
	removeTemps := func() {
		for _, t := range temps {
			os.Remove(t)
		}
	}
	for i, f := range files {
		tmp, err := writeTemp(ctx, tx, f, targets[i])
		if err != nil {
			removeTemps()
			return nil, err
		}
		temps = append(temps, tmp)
	}
	for j, a := range assets {
		tmp, err := writeAssetTemp(a, targets[len(files)+j])
		if err != nil {
			removeTemps()
			return nil, err
		}
		temps = append(temps, tmp)
	}

	// Phase 2: place every file with a no-clobber link. On failure, undo the
	// placements made by this call and remove the remaining temps.
	var placed []string
	for i := range temps {
		if err := os.Link(temps[i], targets[i]); err != nil {
			remaining := rollback(placed)
			removeTemps()
			return remaining, fmt.Errorf("restore: place %s: %w", targets[i], err)
		}
		placed = append(placed, targets[i])
	}
	removeTemps()
	return placed, nil
}

// rollback removes the given placed files and returns any that could not be
// removed (those remain on disk).
func rollback(placed []string) []string {
	var remaining []string
	for _, p := range placed {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			remaining = append(remaining, p)
		}
	}
	return remaining
}

func sessionFiles(ctx context.Context, q querier, sessionID string) ([]fileRow, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, path, kind, sha256_prefix, "offset", mode, mtime FROM files WHERE session_id=? ORDER BY path`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []fileRow
	for rows.Next() {
		var f fileRow
		var kind, prefix sql.NullString
		var mode, mtime sql.NullInt64
		if err := rows.Scan(&f.id, &f.path, &kind, &prefix, &f.offset, &mode, &mtime); err != nil {
			return nil, err
		}
		f.mode, f.mtime = mode.Int64, mtime.Int64
		f.kind = kind.String
		f.prefix = prefix.String
		out = append(out, f)
	}
	return out, rows.Err()
}

// relPath returns the file's path relative to the projects root, i.e. starting
// at the project slug directory. For a main file that is the last two path
// components. For nested files it starts at the component before the session
// id directory.
func relPath(path, sessionID, kind string) (string, error) {
	parts := strings.Split(filepath.Clean(path), string(filepath.Separator))
	n := len(parts)
	if kind == "main" {
		if n < 2 {
			return "", fmt.Errorf("restore: main file %s has no project directory", path)
		}
		return filepath.Join(parts[n-2:]...), nil
	}
	for i := n - 2; i >= 1; i-- {
		if parts[i] == sessionID {
			return filepath.Join(parts[i-1:]...), nil
		}
	}
	return "", fmt.Errorf("restore: %s has no session directory %s", path, sessionID)
}

// records streams the live records of f in line_no order, calling fn with
// each decompressed line (without newline), its line_no and its stored byte_len.
func records(ctx context.Context, q querier, f fileRow, fn func(line []byte, lineNo, byteLen int64) error) error {
	rows, err := q.QueryContext(ctx, `SELECT line_no, raw, byte_len FROM records
		WHERE file_id=? AND superseded=0 ORDER BY line_no`, f.id)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var lineNo, byteLen int64
		var raw []byte
		if err := rows.Scan(&lineNo, &raw, &byteLen); err != nil {
			return err
		}
		line, err := zst.Decompress(raw)
		if err != nil {
			return fmt.Errorf("restore: %s line %d: decompress: %w", f.path, lineNo, err)
		}
		if err := fn(line, lineNo, byteLen); err != nil {
			return err
		}
	}
	return rows.Err()
}

// prefixWriter feeds at most prefixBytes of what is written into h.
type prefixWriter struct {
	h    hash.Hash
	left int
}

func (p *prefixWriter) write(b []byte) {
	n := min(len(b), p.left)
	p.h.Write(b[:n])
	p.left -= n
}

// validate checks that f has live records, that line_no runs contiguously
// from 1, that each record decompresses and its stored byte_len equals the
// line length plus the newline, that the byte_len total equals files."offset"
// (so a missing record is never restored as a silently shorter file), and that
// the reconstructed first min(4096, size) bytes match files.sha256_prefix.
func validate(ctx context.Context, q querier, f fileRow) error {
	pw := &prefixWriter{h: sha256.New(), left: prefixBytes}
	n := 0
	var sum int64
	err := records(ctx, q, f, func(line []byte, lineNo, byteLen int64) error {
		if lineNo != int64(n)+1 {
			return fmt.Errorf("restore: %s: line_no gap: expected %d, found %d (missing record)", f.path, n+1, lineNo)
		}
		sum += byteLen
		if int64(len(line))+1 != byteLen {
			return fmt.Errorf("restore: %s: stored byte_len %d does not match line length %d", f.path, byteLen, len(line)+1)
		}
		n++
		pw.write(line)
		pw.write([]byte{'\n'})
		return nil
	})
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("restore: file %s has no live records", f.path)
	}
	if sum != f.offset {
		return fmt.Errorf("restore: %s: records total %d bytes but archived offset is %d (missing or extra records)", f.path, sum, f.offset)
	}
	if f.prefix != "" {
		if got := hex.EncodeToString(pw.h.Sum(nil)); got != f.prefix {
			return fmt.Errorf("restore: %s: reconstructed prefix sha256 %s does not match archived %s", f.path, got, f.prefix)
		}
	}
	return nil
}

// writeTemp writes f's reconstructed content to a fresh temp file in the
// target's directory and returns its name. The temp file is removed on error.
func writeTemp(ctx context.Context, q querier, f fileRow, target string) (name string, err error) {
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, ".restore-*.tmp")
	if err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()

	w := bufio.NewWriterSize(tmp, 1<<20)
	err = records(ctx, q, f, func(line []byte, _, _ int64) error {
		if _, err := w.Write(line); err != nil {
			return err
		}
		return w.WriteByte('\n')
	})
	if err != nil {
		return "", err
	}
	if err = w.Flush(); err != nil {
		return "", err
	}
	if err = finishTemp(tmp, f.mode, f.mtime); err != nil {
		return "", err
	}
	return tmp.Name(), nil
}

// finishTemp sets mode (default 0600) and mtime on tmp, syncs and closes it.
func finishTemp(tmp *os.File, mode, mtime int64) error {
	m := os.FileMode(mode).Perm()
	if mode == 0 {
		m = defaultMode
	}
	if err := tmp.Chmod(m); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if mtime > 0 {
		t := time.Unix(0, mtime)
		return os.Chtimes(tmp.Name(), t, t)
	}
	return nil
}

func sessionAssets(ctx context.Context, q querier, sessionID string, withFileHistory, withProfileDirs bool) ([]assetRow, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, path, COALESCE(kind,''), mode, mtime, COALESCE(size,0), COALESCE(sha256,''), raw FROM assets
		WHERE session_id=? AND raw IS NOT NULL ORDER BY path`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []assetRow
	for rows.Next() {
		var a assetRow
		var mode, mtime sql.NullInt64
		if err := rows.Scan(&a.id, &a.path, &a.kind, &mode, &mtime, &a.size, &a.sha, &a.raw); err != nil {
			return nil, err
		}
		a.mode, a.mtime = mode.Int64, mtime.Int64
		if a.kind == "file-history" && !withFileHistory {
			continue
		}
		if (a.kind == paths.AssetUploads || a.kind == paths.AssetTasks) && !withProfileDirs && !strings.HasPrefix(sessionID, a.kind+":") {
			continue
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// docAssets keeps the assets whose path below their profile folder is rel.
func docAssets(assets []assetRow, rel string) []assetRow {
	var out []assetRow
	for _, a := range assets {
		if paths.ProfileDirRel(a.path, a.kind) == rel {
			out = append(out, a)
		}
	}
	return out
}

// Doc returns the archived bytes of the file a per-file id
// ("plans:<profile>/<name>") names; the newest one when the profile folder
// was archived from more than one location.
func Doc(ctx context.Context, st *store.Store, id string) ([]byte, error) {
	sid, rel := paths.SplitDocSession(id)
	if rel == "" {
		return nil, fmt.Errorf("restore: %s does not name a file", id)
	}
	assets, err := sessionAssets(ctx, st.DB(), sid, false, false)
	if err != nil {
		return nil, err
	}
	assets = docAssets(assets, rel)
	if len(assets) == 0 {
		return nil, fmt.Errorf("restore: no archived file for %s", id)
	}
	newest := assets[0]
	for _, a := range assets[1:] {
		if a.mtime > newest.mtime {
			newest = a
		}
	}
	return assetBytes(newest)
}

// assetBytes decompresses an asset and verifies its size and sha256.
func assetBytes(a assetRow) ([]byte, error) {
	data, err := zst.Decompress(a.raw)
	if err != nil {
		return nil, fmt.Errorf("restore: asset %s: decompress: %w", a.path, err)
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != a.sha || int64(len(data)) != a.size {
		return nil, fmt.Errorf("restore: asset %s: content does not match archived sha256/size", a.path)
	}
	return data, nil
}

func writeAssetTemp(a assetRow, target string) (string, error) {
	data, err := assetBytes(a)
	if err != nil {
		return "", err
	}
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, ".restore-*.tmp")
	if err != nil {
		return "", err
	}
	if _, err = tmp.Write(data); err == nil {
		err = finishTemp(tmp, a.mode, a.mtime)
	}
	if err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", err
	}
	return tmp.Name(), nil
}

func isProfileDirKind(kind string) bool {
	for _, k := range paths.SweptDirs {
		if k == kind {
			return true
		}
	}
	return false
}

// assetRel is an asset's path relative to the tree it came from: for
// file-history snapshots "file-history/<sid>/...", for plans, paste-cache,
// tasks and uploads files "<kind>/...", for memory files
// "<slug>/memory/<file>", otherwise it starts at the project slug.
func assetRel(path, sessionID, kind string) (string, error) {
	parts := strings.Split(filepath.Clean(path), string(filepath.Separator))
	n := len(parts)
	switch {
	case kind == "file-history":
		for i := n - 2; i >= 1; i-- {
			if parts[i] == sessionID && parts[i-1] == "file-history" {
				return filepath.Join(parts[i-1:]...), nil
			}
		}
	case isProfileDirKind(kind):
		// session-scoped: <kind>/<sid>/...; pseudo-session: <kind>/... (the
		// nearest enclosing directory named like the kind)
		for i := n - 2; i >= 1; i-- {
			if parts[i-1] == kind && parts[i] == sessionID {
				return filepath.Join(parts[i-1:]...), nil
			}
		}
		if strings.HasPrefix(sessionID, kind+":") {
			for i := n - 2; i >= 0; i-- {
				if parts[i] == kind {
					return filepath.Join(parts[i:]...), nil
				}
			}
		}
	case strings.HasPrefix(sessionID, "memory:"):
		slug := strings.TrimPrefix(sessionID, "memory:")
		for i := n - 2; i >= 1; i-- {
			if parts[i] == "memory" && parts[i-1] == slug {
				return filepath.Join(parts[i-1:]...), nil
			}
		}
	default:
		return relPath(path, sessionID, "")
	}
	return "", fmt.Errorf("restore: cannot place asset %s for session %s", path, sessionID)
}
