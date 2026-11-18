package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/wamra/session-index/internal/keys"
	"github.com/wamra/session-index/internal/paths"
	"github.com/wamra/session-index/internal/store"
)

// kdfParams is the passphrase stretching of a new key file (a var so tests
// can use cheap settings).
var kdfParams = keys.DefaultParams

// passReader reads passphrases: without echo from a terminal, otherwise one
// line each from standard input.
type passReader struct {
	errw io.Writer
	tty  *os.File
	br   *bufio.Reader
}

func newPassReader(errw io.Writer) *passReader {
	if f, ok := stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		return &passReader{errw: errw, tty: f}
	}
	return &passReader{errw: errw, br: bufio.NewReader(stdin)}
}

func (p *passReader) read(prompt string) ([]byte, error) {
	var b []byte
	if p.tty != nil {
		fmt.Fprint(p.errw, prompt)
		var err error
		b, err = term.ReadPassword(int(p.tty.Fd()))
		fmt.Fprintln(p.errw)
		if err != nil {
			return nil, err
		}
	} else {
		line, err := p.br.ReadString('\n')
		if err != nil && err != io.EOF {
			return nil, err
		}
		b = []byte(strings.TrimRight(line, "\r\n"))
	}
	if len(b) == 0 {
		return nil, errors.New("no passphrase given")
	}
	return b, nil
}

// readNew asks for a new passphrase, twice on a terminal.
func (p *passReader) readNew() ([]byte, error) {
	b, err := p.read(fmt.Sprintf("New passphrase (at least %d characters): ", keys.MinPassphrase))
	if err != nil {
		return nil, err
	}
	if len(b) < keys.MinPassphrase {
		return nil, fmt.Errorf("the passphrase must have at least %d characters", keys.MinPassphrase)
	}
	if p.tty != nil {
		again, err := p.read("Repeat the passphrase: ")
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(b, again) {
			return nil, errors.New("the two passphrases differ")
		}
	}
	return b, nil
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// convertFlags are the flags encrypt and decrypt share.
func convertFlags(fs *flag.FlagSet) (rec *bool, wait *time.Duration) {
	rec = fs.Bool("recover", false, "only repair an interrupted encrypt or decrypt run")
	wait = fs.Duration("wait", 30*time.Second, "how long to wait for other session-index processes to leave the archive")
	return
}

// recoverRun repairs an interrupted run, if there is one.
func recoverRun(p string, out io.Writer) error {
	done, err := store.Recover(p)
	if err != nil {
		return err
	}
	if done {
		fmt.Fprintf(out, "repaired an interrupted run: %s is as it was before it\n", p)
	}
	return nil
}

func progressTo(out io.Writer) func(string) {
	return func(s string) { fmt.Fprintln(out, s+" ...") }
}

func cmdEncrypt(args []string, out, errw io.Writer) int {
	fs := newFlags("encrypt", errw)
	rec, wait := convertFlags(fs)
	if _, err := parseInterspersed(fs, args); err != nil {
		return 2
	}
	p := dbPath()
	fail := func(err error) int {
		fmt.Fprintf(errw, "encrypt: %v\n", err)
		return 1
	}
	if err := recoverRun(p, out); err != nil {
		return fail(err)
	}
	if *rec {
		return 0
	}
	state, err := store.Sniff(p)
	if err != nil {
		return fail(err)
	}
	kp := keys.Path(p)
	id, kerr := keys.ID(kp)
	hasKey := kerr == nil
	if kerr != nil && !errors.Is(kerr, keys.ErrNoKeyFile) {
		return fail(kerr)
	}
	if state == store.FileOpaque {
		if !hasKey {
			return fail(fmt.Errorf("%s is not a plaintext SQLite database and has no key file (%s)", p, kp))
		}
		fmt.Fprintf(out, "already encrypted: %s (key %s)\n", p, shortID(id))
		return 0
	}
	if state == store.FileAbsent && hasKey {
		fmt.Fprintf(out, "no archive at %s yet; its key file exists (key %s), so it will be created encrypted\n", p, shortID(id))
		return 0
	}

	// The key: the one of an existing key file, else a new one.
	pr := newPassReader(errw)
	var key []byte
	if hasKey {
		pass, err := pr.read(fmt.Sprintf("Passphrase of the existing key file (key %s): ", shortID(id)))
		if err != nil {
			return fail(err)
		}
		if _, key, err = keys.Unwrap(kp, pass); err != nil {
			return fail(err)
		}
	} else {
		pass, err := pr.readNew()
		if err != nil {
			return fail(err)
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return fail(err)
		}
		if id, key, err = keys.Create(kp, pass, kdfParams); err != nil {
			return fail(err)
		}
		fmt.Fprintf(out, "key file written: %s (key %s, locked with the passphrase)\n", kp, shortID(id))
	}
	if err := keys.Store(id, key); err != nil {
		return fail(err)
	}
	if state == store.FileAbsent {
		fmt.Fprintf(out, "no archive at %s yet: it will be created encrypted\n", p)
		return 0
	}

	backup, err := store.Encrypt(context.Background(), p, key, store.ConvertOpts{Wait: *wait, Progress: progressTo(out)})
	if err != nil {
		fmt.Fprintf(errw, "encrypt: %v\nThe archive is unchanged (plaintext). The key file stays; run encrypt again with the same passphrase.\n", err)
		return 1
	}
	fmt.Fprintf(out, "encrypted: %s (key %s, unlocked on this machine)\n", p, shortID(id))
	fmt.Fprintf(out, "plaintext copy kept: %s (%s)\n", backup, humanBytes(dbSize(backup)))
	fmt.Fprintln(out, "Check the archive (session-index doctor, a search), then delete that copy yourself; nothing here deletes it.")
	fmt.Fprintln(out, "Other machines need `session-index unlock` once. Without the passphrase the archive cannot be read: there is no recovery.")
	return 0
}

func cmdDecrypt(args []string, out, errw io.Writer) int {
	fs := newFlags("decrypt", errw)
	rec, wait := convertFlags(fs)
	if _, err := parseInterspersed(fs, args); err != nil {
		return 2
	}
	p := dbPath()
	fail := func(err error) int {
		fmt.Fprintf(errw, "decrypt: %v\n", err)
		return 1
	}
	if err := recoverRun(p, out); err != nil {
		return fail(err)
	}
	if *rec {
		return 0
	}
	state, err := store.Sniff(p)
	if err != nil {
		return fail(err)
	}
	switch state {
	case store.FileAbsent:
		return fail(fmt.Errorf("no archive at %s", p))
	case store.FilePlain:
		fmt.Fprintf(out, "not encrypted: %s\n", p)
		return 0
	}
	key, _, err := keys.ForArchive(p)
	if errors.Is(err, keys.ErrLocked) {
		var pass []byte
		if pass, err = newPassReader(errw).read("Passphrase: "); err == nil {
			_, key, err = keys.Unwrap(keys.Path(p), pass)
		}
	}
	if err != nil {
		return fail(err)
	}
	backup, err := store.Decrypt(context.Background(), p, key, store.ConvertOpts{Wait: *wait, Progress: progressTo(out)})
	if err != nil {
		fmt.Fprintf(errw, "decrypt: %v\nThe archive is unchanged (encrypted).\n", err)
		return 1
	}
	fmt.Fprintf(out, "decrypted: %s is a plaintext archive again\n", p)
	fmt.Fprintf(out, "encrypted copy kept: %s (%s)\n", backup, humanBytes(dbSize(backup)))
	fmt.Fprintf(out, "The key file %s stays, so `session-index encrypt` can reuse it.\n", keys.Path(p))
	return 0
}

// keyTarget is one archive whose key file unlock and lock handle.
type keyTarget struct {
	name, keyFile, id string
}

// keyTargets lists the own archive and every peer archive that has a key
// file next to it.
func keyTargets() ([]keyTarget, error) {
	own, _ := ownProfile()
	all := append([]paths.Peer{{Name: own, Path: dbPath()}}, searchPeers(nil, io.Discard)...)
	seen := map[string]bool{}
	var out []keyTarget
	for _, a := range all {
		kp := keys.Path(a.Path)
		if seen[kp] {
			continue
		}
		seen[kp] = true
		id, err := keys.ID(kp)
		if errors.Is(err, keys.ErrNoKeyFile) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", kp, err)
		}
		out = append(out, keyTarget{a.Name, kp, id})
	}
	return out, nil
}

func cmdUnlock(args []string, out, errw io.Writer) int {
	fs := newFlags("unlock", errw)
	if _, err := parseInterspersed(fs, args); err != nil {
		return 2
	}
	targets, err := keyTargets()
	if err != nil {
		fmt.Fprintf(errw, "unlock: %v\n", err)
		return 1
	}
	if len(targets) == 0 {
		fmt.Fprintf(out, "nothing to unlock: no %s next to %s or a peer archive\n", keys.FileName, dbPath())
		return 0
	}
	var locked []keyTarget
	for _, t := range targets {
		if _, err := keys.Load(t.id); err == nil {
			fmt.Fprintf(out, "%s: already unlocked (key %s)\n", t.name, shortID(t.id))
		} else if errors.Is(err, keys.ErrLocked) {
			locked = append(locked, t)
		} else {
			fmt.Fprintf(errw, "unlock: %s: %v\n", t.name, err)
			return 1
		}
	}
	if len(locked) == 0 {
		return 0
	}
	pass, err := newPassReader(errw).read("Passphrase: ")
	if err != nil {
		fmt.Fprintf(errw, "unlock: %v\n", err)
		return 1
	}
	code := 0
	for _, t := range locked {
		id, key, err := keys.Unwrap(t.keyFile, pass)
		if err == nil {
			err = keys.Store(id, key)
		}
		if err != nil {
			fmt.Fprintf(out, "%s: %v (key %s)\n", t.name, err, shortID(t.id))
			code = 1
			continue
		}
		fmt.Fprintf(out, "%s: unlocked (key %s)\n", t.name, shortID(t.id))
	}
	return code
}

func cmdLock(args []string, out, errw io.Writer) int {
	fs := newFlags("lock", errw)
	if _, err := parseInterspersed(fs, args); err != nil {
		return 2
	}
	targets, err := keyTargets()
	if err != nil {
		fmt.Fprintf(errw, "lock: %v\n", err)
		return 1
	}
	if len(targets) == 0 {
		fmt.Fprintln(out, "nothing to lock")
		return 0
	}
	code := 0
	for _, t := range targets {
		was, err := keys.Forget(t.id)
		switch {
		case err != nil:
			fmt.Fprintf(errw, "lock: %s: %v\n", t.name, err)
			code = 1
		case was:
			fmt.Fprintf(out, "%s: locked (key %s removed from this machine)\n", t.name, shortID(t.id))
		default:
			fmt.Fprintf(out, "%s: was already locked\n", t.name)
		}
	}
	return code
}

// hookLockedNotice tells the user, through the hook's JSON output, that the
// session is not being archived because the archive is locked. Claude Code
// shows systemMessage for Stop; SessionEnd discards it.
func hookLockedNotice(out io.Writer, event string, err error) {
	if event != "Stop" || !(errors.Is(err, keys.ErrLocked) || errors.Is(err, store.ErrWrongKey)) {
		return
	}
	json.NewEncoder(out).Encode(map[string]string{
		"systemMessage": "session-index: the archive is encrypted and locked on this machine, so this session is not being archived. Run `session-index unlock`, then `session-index sweep`.",
	})
}

// doctorEncryption prints how the archive at p is protected, and the
// plaintext copies an encrypt run left next to it.
func doctorEncryption(p string, out io.Writer) {
	e := store.Encryption(p)
	key := ""
	if e.KeyID != "" {
		key = "key " + shortID(e.KeyID)
	}
	switch {
	case e.Err != nil:
		fmt.Fprintf(out, "encryption: cannot tell: %v\n", e.Err)
	case e.State == store.FileOpaque && !e.KeyFile:
		fmt.Fprintf(out, "encryption: %s is not a plaintext database and %s is missing\n", filepath.Base(p), keys.Path(p))
	case e.State == store.FileOpaque && e.Unlocked:
		fmt.Fprintf(out, "encryption: on (%s, unlocked on this machine)\n", key)
	case e.State == store.FileOpaque:
		fmt.Fprintf(out, "encryption: on (%s), LOCKED on this machine: run `session-index unlock`\n", key)
	case e.KeyFile:
		fmt.Fprintf(out, "encryption: off (plaintext archive; a key file exists (%s): `session-index encrypt` uses it)\n", key)
	default:
		fmt.Fprintln(out, "encryption: off (plaintext archive)")
	}
	if _, err := os.Stat(store.MarkerPath(p)); err == nil {
		fmt.Fprintf(out, "WARNING: %s exists: an encrypt or decrypt run is active or was interrupted (`session-index encrypt --recover`)\n", store.MarkerPath(p))
	}
	if e.State == store.FileOpaque {
		left, _ := filepath.Glob(p + ".plaintext-*")
		for _, l := range left {
			fmt.Fprintf(out, "WARNING: plaintext copy of the archive still present: %s (%s)\n", l, humanBytes(dbSize(l)))
		}
	}
}
