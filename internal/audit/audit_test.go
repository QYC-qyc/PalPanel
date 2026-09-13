package audit

import (
	"bytes"
	"log"
	"os"
	"strings"
	"testing"

	"palpanel/internal/db"
)

// TestRecordWriteFailureLogged：DB 不可写（已关闭）时 Record 不得吞错，
// 须以 "[audit] write failed" 前缀记录失败日志，且不 panic。
func TestRecordWriteFailureLogged(t *testing.T) {
	d, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(d); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	r := New(d)

	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	r.Record(1, "root", 0, "test.action", "", "127.0.0.1") // 不 panic
	r.Record(1, "root", 7, "test.action", "", "127.0.0.1") // 带 instanceID 分支同样不 panic

	if !strings.Contains(buf.String(), "[audit] write failed") {
		t.Fatalf("want [audit] write failed logged, got %q", buf.String())
	}
}
