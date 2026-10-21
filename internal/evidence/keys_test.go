package evidence

import (
	"os"
	"path/filepath"
	"testing"
)

func TestKeys(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadKey(dir); err != ErrNoKey {
		t.Fatalf("no key: %v", err)
	}
	pub, err := InitKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := InitKey(dir); err == nil {
		t.Fatal("replaced an existing key")
	}
	priv, err := LoadKey(dir)
	if err != nil || !pub.Equal(priv.Public()) {
		t.Fatalf("load: %v", err)
	}
	os.Chmod(filepath.Join(dir, "keys", keyFile), 0o644)
	if _, err := LoadKey(dir); err == nil {
		t.Fatal("loaded a world-readable key")
	}

	keys, err := ParseTrustedKeys("# laptop\n" + FormatPublicKey(pub) + " laptop\n\n" + FormatPublicKey(pub) + "\n")
	if err != nil || len(keys) != 2 {
		t.Fatalf("parse: %v %d", err, len(keys))
	}
	for _, bad := range []string{"ssh-ed25519 AAAA", "cinema-ed25519 not-base64!", "cinema-ed25519 AAAA", FormatPublicKey(pub) + "\ngarbage"} {
		if _, err := ParseTrustedKeys(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}
