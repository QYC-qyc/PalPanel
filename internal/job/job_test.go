package job

import (
	"context"
	"errors"
	"testing"
	"time"

	"palpanel/internal/event"
)

func TestRunToDone(t *testing.T) {
	m := NewManager(event.NewHub())
	var gotProgress int
	id, err := m.Start("install", 1, func(ctx context.Context, report func(int, string)) error {
		report(40, "下载中")
		gotProgress = 40
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		j, ok := m.Get(id)
		if !ok {
			t.Fatal("job missing")
		}
		if j.State == StateDone {
			if j.Progress != 100 {
				t.Fatalf("final progress %d", j.Progress)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job not done: %+v", j)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if gotProgress != 40 {
		t.Fatal("report not invoked")
	}
}

func TestDuplicateRejected(t *testing.T) {
	m := NewManager(event.NewHub())
	started := make(chan struct{})
	block := make(chan struct{})
	_, err := m.Start("install", 1, func(ctx context.Context, report func(int, string)) error {
		close(started)
		<-block
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if _, err := m.Start("install", 1, func(ctx context.Context, report func(int, string)) error { return nil }); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("want ErrDuplicate got %v", err)
	}
	close(block)
}

func TestFailCarriesError(t *testing.T) {
	m := NewManager(event.NewHub())
	id, _ := m.Start("update", 2, func(ctx context.Context, report func(int, string)) error {
		return errors.New("boom")
	})
	deadline := time.Now().Add(2 * time.Second)
	for {
		j, _ := m.Get(id)
		if j.State == StateFailed {
			if j.Error != "boom" {
				t.Fatalf("error %q", j.Error)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("not failed in time")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestPanicMarksFailed：fn panic 不带崩进程——任务置 failed（Error=内部错误: 值）、
// running 键释放（同实例同 kind 可再次启动）。
func TestPanicMarksFailed(t *testing.T) {
	m := NewManager(event.NewHub())
	id, err := m.Start("install", 1, func(ctx context.Context, report func(int, string)) error {
		panic("boom")
	})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		j, ok := m.Get(id)
		if ok && j.State == StateFailed {
			if j.Error != "内部错误: boom" {
				t.Fatalf("error %q", j.Error)
			}
			if _, err := m.Start("install", 1, func(ctx context.Context, report func(int, string)) error {
				return nil
			}); err != nil {
				t.Fatalf("running 键未释放: %v", err)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("job not failed: %+v", j)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestJobEventsBroadcast(t *testing.T) {
	hub := event.NewHub()
	ch, cancel := hub.Subscribe()
	defer cancel()
	m := NewManager(hub)
	m.Start("install", 5, func(ctx context.Context, report func(int, string)) error { return nil })
	select {
	case e := <-ch:
		if e.Type != "job" || e.InstanceID != 5 {
			t.Fatalf("event %+v", e)
		}
	case <-time.After(time.Second):
		t.Fatal("no job event")
	}
}
