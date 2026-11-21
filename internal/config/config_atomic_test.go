package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestLoadConfigFailsWhenHashedManagementSecretCannotBePersisted(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file write permissions are not enforced equivalently on Windows")
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("port: 8317\nremote-management:\n  secret-key: plaintext\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The config is rewritten in place under a lock, so a read-only file (not
	// directory) is what prevents persisting the hashed secret.
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
	if file, errOpen := os.OpenFile(path, os.O_RDWR, 0); errOpen == nil {
		if errClose := file.Close(); errClose != nil {
			t.Fatal(errClose)
		}
		t.Skip("read-only file is writable by this user")
	}

	_, err := LoadConfig(path)
	if err == nil || !strings.Contains(err.Error(), "persist hashed remote management key") {
		t.Fatalf("LoadConfig() error = %v, want hashed-secret persistence failure", err)
	}
	persisted, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatal(errRead)
	}
	if !strings.Contains(string(persisted), "secret-key: plaintext") {
		t.Fatalf("failed publication modified source: %s", persisted)
	}
}
