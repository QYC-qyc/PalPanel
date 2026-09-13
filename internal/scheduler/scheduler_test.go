package scheduler

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"palpanel/internal/db"
)

// baseTime 是所有测试共用的假时钟起点（秒级整点，便于断言落库文本）。
var baseTime = time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)

// newTestDB 打开临时库并应用迁移（含 003 schedules 表）。
func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	d, err := db.Open(t.TempDir()) // db.Open 收数据目录，单连接，避免锁竞争
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() }) // Windows 下必须先关闭连接，t.TempDir 才能清理文件
	if err := db.Migrate(d); err != nil {
		t.Fatal(err)
	}
	return d
}

// addSchedule 插入一条调度记录；lastRun/nextRun 传 nil 表示 NULL。
func addSchedule(t *testing.T, d *sql.DB, kind, expr string, enabled bool, lastRun, nextRun *time.Time) int64 {
	t.Helper()
	res, err := d.Exec(`INSERT INTO schedules(instance_id, kind, cron_expr, payload, enabled, last_run_at, next_run_at)
		VALUES(1, ?, ?, '{"keep":3}', ?, ?, ?)`,
		kind, expr, boolToInt(enabled), timeArg(lastRun), timeArg(nextRun))
	if err != nil {
		t.Fatalf("insert schedule(%s,%s): %v", kind, expr, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func timeArg(t *time.Time) any {
	if t == nil {
		return nil
	}
	return formatTime(*t)
}

// fakeClock 可手动推进的假 Now 时钟（裁决 ②：Now 可注入）。
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// recorder 记录 RunFunc 调用；block 非 nil 时阻塞每次执行（构造并发场景）；
// panicAll 时每次执行 panic（验证 recover 防击穿）。
type recorder struct {
	mu       sync.Mutex
	calls    []SchedulesRow
	block    chan struct{}
	panicAll bool
}

func (r *recorder) run(row SchedulesRow) error {
	r.mu.Lock()
	r.calls = append(r.calls, row)
	r.mu.Unlock()
	if r.panicAll {
		panic("run-panic")
	}
	if r.block != nil {
		<-r.block
	}
	return nil
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func (r *recorder) lastCall() SchedulesRow {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[len(r.calls)-1]
}

// startScheduler 以假时钟 + 2ms tick 启动调度器（Start 内部先 Reload 补偿）。
func startScheduler(t *testing.T, d *sql.DB, rec *recorder, clock *fakeClock) *Scheduler {
	t.Helper()
	s := New(d, rec.run)
	s.now = clock.Now
	s.tickInterval = 2 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go s.Start(ctx)
	return s
}

// waitFor 轮询等待条件成立（异步 run 在独立 goroutine 中完成）。
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("等待超时: %s", what)
}

// assertCountStays 在若干轮询周期内断言执行次数保持不变（防多触发/误触发）。
func assertCountStays(t *testing.T, r *recorder, want int) {
	t.Helper()
	for i := 0; i < 10; i++ {
		time.Sleep(3 * time.Millisecond)
		if got := r.count(); got != want {
			t.Fatalf("执行次数应保持 %d，实际 %d", want, got)
		}
	}
}

// queryTimes 读取某行的 last_run_at/next_run_at 原始文本（空串 = NULL）。
func queryTimes(t *testing.T, d *sql.DB, id int64) (last, next string) {
	t.Helper()
	var l, n sql.NullString
	if err := d.QueryRow(`SELECT last_run_at, next_run_at FROM schedules WHERE id=?`, id).Scan(&l, &n); err != nil {
		t.Fatal(err)
	}
	return l.String, n.String
}

// 组 1：cron 到点触发——next_run_at 过期后 tick 触发一次，
// 落库 last_run_at=now、next_run_at 推进一个周期，RunFunc 收到完整行。
func TestTickFiresDueSchedule(t *testing.T) {
	d := newTestDB(t)
	clock := &fakeClock{t: baseTime}
	next := baseTime.Add(time.Minute)
	id := addSchedule(t, d, "backup", "* * * * *", true, &baseTime, &next)
	rec := &recorder{}
	_ = startScheduler(t, d, rec, clock)

	clock.Advance(2 * time.Minute) // now=10:02 ≥ next_run_at=10:01
	waitFor(t, "到期触发", func() bool { return rec.count() == 1 })
	assertCountStays(t, rec, 1)

	row := rec.lastCall()
	if row.ID != id || row.Kind != "backup" || row.CronExpr != "* * * * *" ||
		row.Payload != `{"keep":3}` || row.InstanceID != 1 || !row.Enabled {
		t.Errorf("RunFunc 收到的行不正确: %+v", row)
	}
	// 触发即先推进时间再异步执行：交给 RunFunc 的行里 next_run_at 已是下个周期（10:03）。
	if got, want := row.NextRunAt.Format("2006-01-02 15:04:05"), "2026-09-14 10:03:00"; got != want {
		t.Errorf("行内 next_run_at = %q, want %q", got, want)
	}
	last, nextDB := queryTimes(t, d, id)
	if last != "2026-09-14 10:02:00" || nextDB != "2026-09-14 10:03:00" {
		t.Errorf("落库时间 last=%q next=%q", last, nextDB)
	}
}

// 组 2：禁用行不触发（补偿与 tick 均跳过）。
func TestDisabledRowNeverFires(t *testing.T) {
	d := newTestDB(t)
	clock := &fakeClock{t: baseTime}
	addSchedule(t, d, "backup", "* * * * *", false, nil, nil)
	rec := &recorder{}
	_ = startScheduler(t, d, rec, clock)

	clock.Advance(3 * time.Minute)
	assertCountStays(t, rec, 0)
}

// 组 3：补偿——last_run_at 落后两个周期（next_run_at 已过期）启动时只补跑 1 次。
func TestCompensationFiresOnce(t *testing.T) {
	d := newTestDB(t)
	clock := &fakeClock{t: baseTime}
	last := baseTime.Add(-2 * time.Minute)
	expired := baseTime.Add(-1 * time.Minute)
	id := addSchedule(t, d, "restart", "* * * * *", true, &last, &expired)
	rec := &recorder{}
	_ = startScheduler(t, d, rec, clock)

	waitFor(t, "补偿触发", func() bool { return rec.count() == 1 })
	assertCountStays(t, rec, 1) // 落后两个周期也只补 1 次
	lastDB, nextDB := queryTimes(t, d, id)
	if lastDB != "2026-09-14 10:00:00" || nextDB != "2026-09-14 10:01:00" {
		t.Errorf("补偿落库时间 last=%q next=%q", lastDB, nextDB)
	}
}

// 组 4：Reload 热加载——运行中新增的 enabled 行被拾取并补跑。
func TestReloadPicksUpNewRow(t *testing.T) {
	d := newTestDB(t)
	clock := &fakeClock{t: baseTime}
	rec := &recorder{}
	s := startScheduler(t, d, rec, clock)
	assertCountStays(t, rec, 0) // 空表不触发

	addSchedule(t, d, "broadcast", "* * * * *", true, nil, nil)
	s.Reload()
	waitFor(t, "Reload 拾取新行", func() bool { return rec.count() == 1 })
	if row := rec.lastCall(); row.Kind != "broadcast" {
		t.Errorf("kind = %q, want broadcast", row.Kind)
	}
}

// 组 5：并发防重——run 阻塞期间后续 tick 不重入；执行结束后恢复触发。
func TestConcurrentRunNotReentered(t *testing.T) {
	d := newTestDB(t)
	clock := &fakeClock{t: baseTime}
	rec := &recorder{block: make(chan struct{})}
	id := addSchedule(t, d, "backup", "* * * * *", true, nil, nil)
	_ = startScheduler(t, d, rec, clock)

	waitFor(t, "首次触发", func() bool { return rec.count() == 1 }) // 补偿触发，阻塞在 run 内
	clock.Advance(5 * time.Minute)                                 // 期间多个 tick 均应被防重跳过
	assertCountStays(t, rec, 1)

	close(rec.block) // 执行完成，解除防重
	clock.Advance(1 * time.Minute)
	waitFor(t, "防重解除后再次触发", func() bool { return rec.count() == 2 })
	if row := rec.lastCall(); row.ID != id {
		t.Errorf("id = %d, want %d", row.ID, id)
	}
}

// 组 6：RunFunc panic 被 recover，调度循环继续运行后续周期。
func TestRunPanicDoesNotBreakScheduler(t *testing.T) {
	d := newTestDB(t)
	clock := &fakeClock{t: baseTime}
	rec := &recorder{panicAll: true}
	addSchedule(t, d, "update", "* * * * *", true, nil, nil)
	_ = startScheduler(t, d, rec, clock)

	waitFor(t, "panic 前触发", func() bool { return rec.count() == 1 })
	clock.Advance(1 * time.Minute)
	waitFor(t, "panic 后仍能继续触发", func() bool { return rec.count() == 2 })
	assertCountStays(t, rec, 2)
}

// 组 7：永不匹配的表达式——补偿触发一次后 next_run_at 置空，此后 Reload/tick 均不再触发。
func TestNeverMatchingExprFiresOnceThenStops(t *testing.T) {
	d := newTestDB(t)
	clock := &fakeClock{t: baseTime}
	id := addSchedule(t, d, "backup", "0 0 31 2 *", true, nil, nil) // 2 月 31 日：永不匹配
	rec := &recorder{}
	s := startScheduler(t, d, rec, clock)

	waitFor(t, "首次触发", func() bool { return rec.count() == 1 })
	if _, nextDB := queryTimes(t, d, id); nextDB != "" {
		t.Errorf("next_run_at 应置空, got %q", nextDB)
	}
	s.Reload() // 再次加载：不得重复补偿
	clock.Advance(2 * time.Minute)
	assertCountStays(t, rec, 1)
}

// 组 8：Reload 全量替换——行删除后不再触发；仅修复 next_run_at 时不触发。
func TestReloadDropsDeletedRow(t *testing.T) {
	d := newTestDB(t)
	clock := &fakeClock{t: baseTime}
	rec := &recorder{}
	s := startScheduler(t, d, rec, clock)
	addSchedule(t, d, "backup", "* * * * *", true, &baseTime, nil) // last_run=now → 只修复 next，不触发

	assertCountStays(t, rec, 0)
	if _, nextDB := queryTimes(t, d, 1); nextDB != "2026-09-14 10:01:00" {
		t.Errorf("next_run_at 应被修复为 10:01:00, got %q", nextDB)
	}

	if _, err := d.Exec(`DELETE FROM schedules`); err != nil {
		t.Fatal(err)
	}
	s.Reload()
	clock.Advance(2 * time.Minute)
	assertCountStays(t, rec, 0)
}
