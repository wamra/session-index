package main

import (
	"context"
	"fmt"
	"io"

	"github.com/wamra/session-index/internal/restore"
)

func cmdRestore(args []string, out, errw io.Writer) int {
	fs := newFlags("restore", errw)
	to := fs.String("to", "", "directory to restore into")
	inPlace := fs.Bool("in-place", false, "restore to original paths")
	fh := fs.Bool("with-file-history", false, "also write the session's file-history snapshots (<profile>/file-history/<sid>/)")
	pos, err := parseInterspersed(fs, args)
	if err != nil || len(pos) != 1 {
		fmt.Fprintln(errw, "restore: exactly one SESSION_ID is required")
		return 2
	}
	if (*to == "") == !*inPlace {
		fmt.Fprintln(errw, "restore: exactly one of --to DIR or --in-place is required")
		return 2
	}
	st, err := openStore()
	if err != nil {
		fmt.Fprintf(errw, "open archive: %v\n", err)
		return 1
	}
	defer st.Close()
	ctx := context.Background()
	sid, err := resolveSession(ctx, st.DB(), pos[0])
	if err != nil {
		fmt.Fprintln(errw, err)
		return 1
	}
	placed, err := restore.SessionOpts(ctx, st, sid, *to, *inPlace, *fh)
	if err != nil {
		fmt.Fprintf(errw, "restore: %v\n", err)
		return 1
	}
	for _, p := range placed {
		fmt.Fprintln(out, p)
	}
	return 0
}
