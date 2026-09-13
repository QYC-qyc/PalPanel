package auth

import (
	"bytes"
	"testing"

	"palpanel/internal/db"
)

func TestLoadOrCreateSecretStable(t *testing.T) {
	dir := t.TempDir()
	a, err := LoadOrCreateSecret(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadOrCreateSecret(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) || len(a) != 32 {
		t.Fatalf("secret unstable or wrong size: %d", len(a))
	}
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	key, _ := LoadOrCreateSecret(t.TempDir())
	ct, err := Encrypt(key, []byte("pass-word"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ct, []byte("pass-word")) {
		t.Fatal("ciphertext leaks plaintext")
	}
	pt, err := Decrypt(key, ct)
	if err != nil || string(pt) != "pass-word" {
		t.Fatalf("roundtrip: %q %v", pt, err)
	}
}

func TestSetupOnlyOnce(t *testing.T) {
	d, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := db.Migrate(d); err != nil {
		t.Fatal(err)
	}
	if err := Seed(d); err != nil {
		t.Fatal(err)
	}
	key, _ := LoadOrCreateSecret(t.TempDir())
	s := New(d, key)
	id, err := s.Setup("admin1", "good-pass-1")
	if err != nil {
		t.Fatal(err)
	}
	if id == 0 {
		t.Fatal("want user id")
	}
	if _, err := s.Setup("admin2", "good-pass-2"); err != ErrSetupDone {
		t.Fatalf("want ErrSetupDone got %v", err)
	}
	if _, err := s.Setup("admin3", "123"); err != ErrWeakPassword {
		t.Fatalf("want ErrWeakPassword got %v", err)
	}
}
