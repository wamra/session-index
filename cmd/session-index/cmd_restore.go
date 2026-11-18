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
	pd := fs.Bool("with-profile-dirs", false, "also write the session's uploads/ and tasks/ files (<profile>/uploads/<sid>/, <profile>/tasks/<sid>/); implies --with-file-history")
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
	placed, err := restore.SessionOptions(ctx, st, sid, *to, restore.Options{InPlace: *inPlace, FileHistory: *fh, ProfileDirs: *pd})
	if err != nil {
		fmt.Fprintf(errw, "restore: %v\n", err)
		return 1
	}
	for _, p := range placed {
		fmt.Fprintln(out, p)
	}
	return 0
}
