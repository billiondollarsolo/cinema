package evidence

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// One key per machine (plan §7.3). ponytail: a 0600 file in the data dir, like
// an unencrypted SSH key; OS keychain storage when someone needs it.
const keyFile = "signing.key"

var ErrNoKey = errors.New("evidence: no signing key (run `cinema keys init`)")

// InitKey creates this machine's signing key. It never overwrites one.
func InitKey(dataDir string) (ed25519.PublicKey, error) {
	dir := filepath.Join(dataDir, "keys")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, keyFile), os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if errors.Is(err, os.ErrExist) {
		return nil, errors.New("evidence: a signing key already exists; refusing to replace it")
	}
	if err != nil {
		return nil, err
	}
	_, err = f.WriteString(base64.StdEncoding.EncodeToString(priv.Seed()) + "\n")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return pub, err
}

// LoadKey reads this machine's signing key, refusing files others can read.
func LoadKey(dataDir string) (ed25519.PrivateKey, error) {
	p := filepath.Join(dataDir, "keys", keyFile)
	fi, err := os.Lstat(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoKey
	}
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("evidence: %s must be a regular file with mode 0600", p)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("evidence: %s is not a signing key", p)
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// FormatPublicKey is the exported form: "cinema-ed25519 <base64>".
// CINEMA_TRUSTED_KEYS holds one per line (plan §7.3).
func FormatPublicKey(pub ed25519.PublicKey) string {
	return "cinema-ed25519 " + base64.StdEncoding.EncodeToString(pub)
}

// ParseTrustedKeys parses CINEMA_TRUSTED_KEYS: one key per line, '#' comments
// and blank lines ignored, an optional label after the key. Any bad line
// fails the whole set: a typo must not silently drop a key or add one.
func ParseTrustedKeys(s string) ([]ed25519.PublicKey, error) {
	var keys []ed25519.PublicKey
	for i, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 2 || f[0] != "cinema-ed25519" {
			return nil, fmt.Errorf("trusted keys line %d: want \"cinema-ed25519 <base64> [label]\"", i+1)
		}
		b, err := base64.StdEncoding.Strict().DecodeString(f[1])
		if err != nil || len(b) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("trusted keys line %d: bad key", i+1)
		}
		keys = append(keys, ed25519.PublicKey(b))
	}
	return keys, nil
}
