package instance

import (
	"errors"
	"path/filepath"
	"testing"

	"palpanel/internal/db"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	d, err := db.Open(t.TempDir()) // db.Open 收数据目录，库文件固定为目录下 panel.db
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if err := db.Migrate(d); err != nil {
		t.Fatal(err)
	}
	return New(d)
}

func sample(name string) Instance {
	return Instance{Name: name, GameDir: filepath.Join("d", "srv", name),
		GamePort: 8211, QueryPort: 27015, RestPort: 8211,
		AdminPasswordEnc: []byte{1, 2, 3}}
}

func TestPortConflict(t *testing.T) {
	s := newStore(t)
	if _, err := s.Create(sample("a")); err != nil {
		t.Fatal(err)
	}
	b := sample("b")
	if _, err := s.Create(b); !errors.Is(err, ErrPortConflict) {
		t.Fatalf("want ErrPortConflict got %v", err)
	}
	b.QueryPort = 27016
	b.GamePort = 8212
	b.RestPort = 8212
	if _, err := s.Create(b); err != nil {
		t.Fatal(err)
	}
}

func TestGetUpdateDelete(t *testing.T) {
	s := newStore(t)
	id, _ := s.Create(sample("a"))
	got, err := s.Get(id)
	if err != nil || got.Name != "a" || got.RestPort != 8211 {
		t.Fatalf("get: %+v %v", got, err)
	}
	got.GamePort = 8213
	got.RestPort = 8214
	if err := s.Update(got); err != nil {
		t.Fatal(err)
	}
	got2, _ := s.Get(id)
	if got2.GamePort != 8213 || got2.RestPort != 8214 {
		t.Fatalf("update lost: %+v", got2)
	}
	// Update 不应修改 status 列（进程守护属 M2）
	if got2.Status != "idle" {
		t.Fatalf("status should stay idle, got %q", got2.Status)
	}
	if err := s.Delete(id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound got %v", err)
	}
}
