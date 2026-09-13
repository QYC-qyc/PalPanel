package auth

import (
	"bytes"
	"errors"
	"fmt"
	"sync"
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

// TestSetupConcurrentOnlyOne：并发 Setup 只允许恰好一个成功（TOCTOU 防护，
// 计数检查必须在事务内完成）。
func TestSetupConcurrentOnlyOne(t *testing.T) {
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

	const n = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	success, setupDone := 0, 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := s.Setup(fmt.Sprintf("admin-%d", i), "good-pass-1")
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				success++
			case errors.Is(err, ErrSetupDone):
				setupDone++
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if success != 1 || setupDone != n-1 {
		t.Fatalf("want 1 success + %d ErrSetupDone, got %d + %d", n-1, success, setupDone)
	}
}
