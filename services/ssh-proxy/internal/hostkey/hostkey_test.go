package hostkey_test

import (
	"os"
	"path/filepath"
	"testing"

	"hybridcloud/services/ssh-proxy/internal/hostkey"
)

func TestLoadOrGenerate_CreatesWhenMissing(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "hostkey")
	signer, err := hostkey.LoadOrGenerate(path)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if signer == nil {
		t.Fatal("signer nil")
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("perms: got %v, want 0600", info.Mode().Perm())
	}
}

func TestLoadOrGenerate_IsStable(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "hostkey")
	first, err := hostkey.LoadOrGenerate(path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := hostkey.LoadOrGenerate(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(first.PublicKey().Marshal()) != string(second.PublicKey().Marshal()) {
		t.Fatal("public key changed across reloads — host-key stability broken")
	}
}

func TestLoadOrGenerate_RejectsCorrupt(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "hostkey")
	if err := os.WriteFile(path, []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := hostkey.LoadOrGenerate(path); err == nil {
		t.Fatal("expected parse error on garbage")
	}
}
