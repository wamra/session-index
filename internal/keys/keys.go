// Package keys manages the archive's encryption key. The key itself is 32
// random bytes. Two copies exist: a passphrase-locked copy in a small JSON
// file next to the archive (it travels with the archive, for example through
// a synced folder), and an unlocked copy cached on this machine only, so
// hooks can open the archive unattended.
package keys

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/chacha20poly1305"
)

// FileName is the name of the passphrase-locked key file, next to the archive.
const FileName = "archive.key"

// Size is the length of the archive key in bytes.
const Size = 32

// MinPassphrase is the shortest passphrase Create accepts.
const MinPassphrase = 8

var (
	// ErrNoKeyFile is returned when the archive has no key file.
	ErrNoKeyFile = errors.New("no key file")
	// ErrLocked is returned when the key file exists but its key is not
	// cached on this machine.
	ErrLocked = errors.New("archive is encrypted and locked on this machine (run: session-index unlock)")
	// ErrWrongPassphrase is returned by Unwrap when the passphrase does not
	// open the key file (or the file was altered).
	ErrWrongPassphrase = errors.New("wrong passphrase")
)

// Params are the Argon2id cost settings stored in a key file.
type Params struct {
	Time      uint32 // passes
	MemoryKiB uint32
	Threads   uint8
}

// DefaultParams are the settings for new key files.
var DefaultParams = Params{Time: 3, MemoryKiB: 64 * 1024, Threads: 4}

// keyFile is the on-disk form of the passphrase-locked key.
type keyFile struct {
	Version   int    `json:"version"`
	ID        string `json:"id"`
	KDF       string `json:"kdf"`
	Time      uint32 `json:"time"`
	MemoryKiB uint32 `json:"memory_kib"`
	Threads   uint8  `json:"threads"`
	Salt      string `json:"salt"`
	Cipher    string `json:"cipher"`
	Nonce     string `json:"nonce"`
	Wrapped   string `json:"wrapped_key"`
}

const (
	version = 1
	kdfName = "argon2id"
	cipher  = "xchacha20poly1305"
	saltLen = 16
)

var idRe = regexp.MustCompile(`^[0-9a-f]{16}$`)

// Path returns the key file path for the archive at dbPath.
func Path(dbPath string) string { return filepath.Join(filepath.Dir(dbPath), FileName) }

// aad binds the id and the cost settings to the wrapped key, so editing them
// in the file makes Unwrap fail.
func (k keyFile) aad() []byte {
	return []byte(fmt.Sprintf("session-index key v%d %s %s %d %d %d %s", k.Version, k.ID, k.KDF, k.Time, k.MemoryKiB, k.Threads, k.Cipher))
}

func (k keyFile) validate() error {
	switch {
	case k.Version != version:
		return fmt.Errorf("unsupported key file version %d", k.Version)
	case !idRe.MatchString(k.ID):
		return errors.New("invalid key id")
	case k.KDF != kdfName || k.Cipher != cipher:
		return fmt.Errorf("unsupported key file algorithms %q/%q", k.KDF, k.Cipher)
	case k.Time < 1 || k.Time > 16 || k.MemoryKiB < 8 || k.MemoryKiB > 1<<20 || k.Threads < 1 || k.Threads > 16:
		return errors.New("key file cost settings out of range")
	}
	return nil
}

func derive(passphrase []byte, salt []byte, k keyFile) []byte {
	return argon2.IDKey(passphrase, salt, k.Time, k.MemoryKiB, k.Threads, chacha20poly1305.KeySize)
}

// Create makes a new random archive key and writes its passphrase-locked copy
// to path, which must not exist. It returns the key id and the key.
func Create(path string, passphrase []byte, p Params) (id string, key []byte, err error) {
	if len(passphrase) < MinPassphrase {
		return "", nil, fmt.Errorf("passphrase must be at least %d characters", MinPassphrase)
	}
	buf := make([]byte, 8+saltLen+chacha20poly1305.NonceSizeX+Size)
	if _, err := rand.Read(buf); err != nil {
		return "", nil, err
	}
	rawID, salt, nonce := buf[:8], buf[8:8+saltLen], buf[8+saltLen:8+saltLen+chacha20poly1305.NonceSizeX]
	key = buf[len(buf)-Size:]
	k := keyFile{
		Version: version, ID: hex.EncodeToString(rawID), KDF: kdfName,
		Time: p.Time, MemoryKiB: p.MemoryKiB, Threads: p.Threads,
		Salt: hex.EncodeToString(salt), Cipher: cipher, Nonce: hex.EncodeToString(nonce),
	}
	if err := k.validate(); err != nil {
		return "", nil, err
	}
	aead, err := chacha20poly1305.NewX(derive(passphrase, salt, k))
	if err != nil {
		return "", nil, err
	}
	k.Wrapped = hex.EncodeToString(aead.Seal(nil, nonce, key, k.aad()))
	b, err := json.MarshalIndent(k, "", "  ")
	if err != nil {
		return "", nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", nil, err
	}
	if _, err = f.Write(append(b, '\n')); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(path)
		return "", nil, err
	}
	return k.ID, key, nil
}

func read(path string) (keyFile, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return keyFile{}, fmt.Errorf("%w: %s", ErrNoKeyFile, path)
	}
	if err != nil {
		return keyFile{}, err
	}
	var k keyFile
	if err := json.Unmarshal(b, &k); err != nil {
		return keyFile{}, fmt.Errorf("key file %s: %w", path, err)
	}
	if err := k.validate(); err != nil {
		return keyFile{}, fmt.Errorf("key file %s: %w", path, err)
	}
	return k, nil
}

// ID returns the key id recorded in the key file at path.
func ID(path string) (string, error) {
	k, err := read(path)
	return k.ID, err
}

// Unwrap opens the key file at path with passphrase and returns the key id
// and the archive key.
func Unwrap(path string, passphrase []byte) (id string, key []byte, err error) {
	k, err := read(path)
	if err != nil {
		return "", nil, err
	}
	salt, err1 := hex.DecodeString(k.Salt)
	nonce, err2 := hex.DecodeString(k.Nonce)
	wrapped, err3 := hex.DecodeString(k.Wrapped)
	if err1 != nil || err2 != nil || err3 != nil || len(salt) != saltLen || len(nonce) != chacha20poly1305.NonceSizeX {
		return "", nil, fmt.Errorf("key file %s: malformed", path)
	}
	aead, err := chacha20poly1305.NewX(derive(passphrase, salt, k))
	if err != nil {
		return "", nil, err
	}
	key, err = aead.Open(nil, nonce, wrapped, k.aad())
	if err != nil || len(key) != Size {
		return "", nil, ErrWrongPassphrase
	}
	return k.ID, key, nil
}

// Dir returns the directory of unlocked keys on this machine:
// $SESSION_INDEX_KEY_DIR, else ~/.config/session-index/keys.
func Dir() (string, error) {
	if d := os.Getenv("SESSION_INDEX_KEY_DIR"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "session-index", "keys"), nil
}

func cachePath(id string) (string, error) {
	if !idRe.MatchString(id) {
		return "", errors.New("invalid key id")
	}
	d, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, id+".key"), nil
}

// Store caches key under id on this machine (directory 0700, file 0600).
func Store(id string, key []byte) error {
	if len(key) != Size {
		return errors.New("invalid key length")
	}
	p, err := cachePath(id)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".key-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err = tmp.Chmod(0o600); err == nil {
		_, err = tmp.WriteString(hex.EncodeToString(key) + "\n")
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p)
}

// Load returns the key cached under id, or ErrLocked when there is none.
func Load(id string) ([]byte, error) {
	p, err := cachePath(id)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrLocked
	}
	if err != nil {
		return nil, err
	}
	key, err := hex.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(key) != Size {
		return nil, fmt.Errorf("cached key %s is malformed", p)
	}
	return key, nil
}

// Forget removes the key cached under id and reports whether there was one.
func Forget(id string) (bool, error) {
	p, err := cachePath(id)
	if err != nil {
		return false, err
	}
	err = os.Remove(p)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// ForArchive returns the unlocked key of the archive at dbPath and its id.
// The error is ErrNoKeyFile when the archive has no key file and ErrLocked
// (with the id still returned) when its key is not cached on this machine.
func ForArchive(dbPath string) (key []byte, id string, err error) {
	id, err = ID(Path(dbPath))
	if err != nil {
		return nil, "", err
	}
	key, err = Load(id)
	return key, id, err
}
