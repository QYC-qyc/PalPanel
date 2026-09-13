package job

import (
	"context"
	"errors"
	"sync"
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

// TestRunningOfKind：RunningOfKind 按实例+kind 报告运行中任务；完成后转 false。
func TestRunningOfKind(t *testing.T) {
	m := NewManager(event.NewHub())
	started := make(chan struct{})
	block := make(chan struct{})
	_, err := m.Start("update", 7, func(ctx context.Context, report func(int, string)) error {
		close(started)
		<-block
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if !m.RunningOfKind(7, "update") {
		t.Fatal("want running update job for instance 7")
	}
	if m.RunningOfKind(7, "install") || m.RunningOfKind(8, "update") {
		t.Fatal("unexpected running job for other kind/instance")
	}
	close(block)
	deadline := time.Now().Add(2 * time.Second)
	for {
		if !m.RunningOfKind(7, "update") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("job finished but RunningOfKind still true")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestJobRetention：总数超 200 时惰性清理已结束超 1 小时的任务，
// 运行中任务不被清理；注入假时钟避免真实等待。
func TestJobRetention(t *testing.T) {
	m := NewManager(event.NewHub())
	var mu sync.Mutex
	cur := time.Unix(1700000000, 0)
	advance := func(d time.Duration) { mu.Lock(); cur = cur.Add(d); mu.Unlock() }
	m.Now = func() time.Time { mu.Lock(); defer mu.Unlock(); return cur }

	ok := func(ctx context.Context, report func(int, string)) error { return nil }
	waitDone := func(id string) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for {
			j, _ := m.Get(id)
			if j.State == StateDone {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("job not done: %+v", j)
			}
			time.Sleep(2 * time.Millisecond)
		}
	}

	// 造 201 个已完成任务，每次完成后把时钟推 2 小时（全部过期）
	for i := 1; i <= 201; i++ {
		id, err := m.Start("install", int64(i), ok)
		if err != nil {
			t.Fatalf("start #%d: %v", i, err)
		}
		waitDone(id)
		advance(2 * time.Hour)
	}

	// 运行中任务：不应被清理
	block := make(chan struct{})
	if _, err := m.Start("update", 999, func(ctx context.Context, report func(int, string)) error {
		<-block
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	advance(2 * time.Hour) // 此刻全部 done 任务已过期超 1 小时

	// 再起一个任务触发惰性清理（Start 与完成路径均会执行）
	id, err := m.Start("install", 5000, ok)
	if err != nil {
		t.Fatal(err)
	}
	waitDone(id)

	jobs := m.List()
	if len(jobs) > 201 {
		t.Fatalf("清理后残留 %d 个任务", len(jobs))
	}
	if !m.RunningOfKind(999, "update") {
		t.Fatal("运行中任务被误清理")
	}
	found := false
	for _, j := range jobs {
		if j.ID == id {
			found = true
		}
	}
	if !found {
		t.Fatalf("新任务 %s 不在列表: %d 个", id, len(jobs))
	}
	close(block)
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
