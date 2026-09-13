// Package scheduler 实现 schedules 表驱动的定时任务调度：
// 以 cron 表达式（internal/cronexpr）计算触发时刻，落库 last_run_at/next_run_at，
// 具体执行体经 RunFunc 注入（backup/重启/广播/更新由 API 层按 Kind 分派），
// 调度器自身不依赖任何业务执行体（裁决 ①）。
//
// 核心语义（控制器裁决）：
//   - 触发即先落库 last_run_at=now、next_run_at=Next(now)，再异步执行 RunFunc；
//     执行失败不回滚时间，下个周期自然重试；
//   - 启动/Reload 时补偿：enabled 且（last_run_at 为空或 next_run_at 已过期）的行
//     立即补跑一次——无论落后多少个周期只补 1 次；
//   - 同一 schedule 内存 running set 并发防重：锁内判定+登记，执行完成移除；
//   - RunFunc panic 由调度器 recover 并记日志，防止单次失败击穿整个循环；
//   - cronexpr.Next 永不匹配返回零值：此时 next_run_at 置空（NULL）并记日志提示。
package scheduler

import (
	"context"
	"database/sql"
	"log"
	"sync"
	"time"

	"palpanel/internal/cronexpr"
)

// RunFunc 是单次任务执行体，由 API 层注入并按 Kind 分派
// （backup→备份；restart→停止+启动；broadcast→RCON/REST 广播；update→更新）。
type RunFunc func(row SchedulesRow) error

// SchedulesRow 是 schedules 表的一行。
// LastRunAt/NextRunAt 为 NULL 或解析失败时为零值 time.Time{}。
type SchedulesRow struct {
	ID         int64
	InstanceID int64
	Kind       string // backup / restart / broadcast / update
	CronExpr   string
	Payload    string
	Enabled    bool
	LastRunAt  time.Time
	NextRunAt  time.Time
}

// timeLayout 与 backup 存储层的时间文本格式保持一致（SQLite datetime 惯例，UTC）。
const timeLayout = "2006-01-02 15:04:05"

// jobEntry 是内存中的调度条目：表行 + 已解析的 cron 计划。
type jobEntry struct {
	row   SchedulesRow
	sched cronexpr.Schedule // Schedule 不可变，可并发复用
}

// Scheduler 是 schedules 表驱动的定时任务调度器。
type Scheduler struct {
	db  *sql.DB
	run RunFunc

	// now 与 tickInterval 可注入（裁决 ②）：测试用假时钟 + 毫秒级 tick；
	// 生产 tickInterval<=0 表示 tick 对齐整分钟。
	now          func() time.Time
	tickInterval time.Duration

	mu      sync.Mutex
	jobs    []jobEntry     // Reload 全量替换（锁内）；fire 按 ID 更新
	running map[int64]bool // 并发防重：正在执行的 schedule ID（锁内判定+登记）
}

// New 基于已迁移的 *sql.DB 构造调度器。run 为执行体（API 层分派）。
func New(db *sql.DB, run RunFunc) *Scheduler {
	return &Scheduler{
		db:      db,
		run:     run,
		now:     time.Now,
		running: make(map[int64]bool),
	}
}

// Start 阻塞运行调度循环直到 ctx 取消（应由调用方 goroutine 运行）。
// 启动先 Reload（含错过调度的补偿），随后 tick：测试注入 tickInterval>0 时
// 按该间隔 tick；生产先对齐到下一个整分钟，再以 time.Ticker 每分钟 tick。
func (s *Scheduler) Start(ctx context.Context) {
	s.Reload()
	if s.tickInterval > 0 {
		ticker := time.NewTicker(s.tickInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.tick()
			}
		}
	}
	// 生产模式：对齐整分钟后以分钟为周期 tick。
	now := s.now()
	timer := time.NewTimer(now.Truncate(time.Minute).Add(time.Minute).Sub(now))
	select {
	case <-ctx.Done():
		timer.Stop()
		return
	case <-timer.C:
	}
	s.tick()
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.tick()
		}
	}
}

// Reload 从 schedules 表全量重载内存列表（锁内替换，裁决 ⑥），并对 enabled 行
// 做补偿判定（裁决 ④）：last_run_at 为空或 next_run_at 已过期 → 立即补跑一次；
// next_run_at 缺失但 last_run_at 存在 → 仅修复 next_run_at（不触发）。
// cron 表达式非法的行记日志后跳过。schedules 增删改后由 API 层调用以热加载。
func (s *Scheduler) Reload() {
	rows, err := s.loadRows()
	if err != nil {
		log.Printf("scheduler: 读取 schedules 失败: %v", err)
		return
	}
	now := s.now()
	entries := make([]jobEntry, 0, len(rows))
	for _, r := range rows {
		sched, err := cronexpr.Parse(r.CronExpr)
		if err != nil {
			log.Printf("scheduler: schedule %d(%s) cron 表达式无效 %q: %v（跳过）",
				r.ID, r.Kind, r.CronExpr, err)
			continue
		}
		entries = append(entries, jobEntry{row: r, sched: sched})
	}
	// 锁内替换内存列表；正在执行的行以旧列表中的最新时间为准——fire 在登记
	// running 的同一把锁内更新内存时间，避免 Reload 用过期的库读值覆盖，
	// 导致执行完成后被 tick 再次补跑。
	s.mu.Lock()
	old := s.jobs
	for i := range entries {
		if s.running[entries[i].row.ID] {
			for j := range old {
				if old[j].row.ID == entries[i].row.ID {
					entries[i].row.LastRunAt = old[j].row.LastRunAt
					entries[i].row.NextRunAt = old[j].row.NextRunAt
					break
				}
			}
		}
	}
	s.jobs = entries
	s.mu.Unlock()

	for _, e := range entries {
		if !e.row.Enabled {
			continue
		}
		switch {
		case e.row.LastRunAt.IsZero() ||
			(!e.row.NextRunAt.IsZero() && !e.row.NextRunAt.After(now)):
			// 新行（从未执行）或错过调度：补跑一次。
			s.fire(e, now)
		case e.row.NextRunAt.IsZero():
			// 已执行过但 next_run_at 缺失：修复之；永不匹配则保持为空并提示。
			if next := e.sched.Next(now); next.IsZero() {
				log.Printf("scheduler: schedule %d(%s) 表达式 %q 永不匹配，next_run_at 保持为空",
					e.row.ID, e.row.Kind, e.row.CronExpr)
			} else if _, err := s.db.Exec(`UPDATE schedules SET next_run_at=? WHERE id=?`,
				formatTime(next), e.row.ID); err != nil {
				log.Printf("scheduler: schedule %d 修复 next_run_at 失败: %v", e.row.ID, err)
			} else {
				e.row.NextRunAt = next
				s.mu.Lock()
				s.updateEntryLocked(e)
				s.mu.Unlock()
			}
		}
	}
}

// tick 触发所有 enabled 且 next_run_at 已到期的条目。
func (s *Scheduler) tick() {
	now := s.now()
	s.mu.Lock()
	entries := make([]jobEntry, len(s.jobs))
	copy(entries, s.jobs)
	s.mu.Unlock()
	for _, e := range entries {
		if !e.row.Enabled || e.row.NextRunAt.IsZero() {
			continue // 永不匹配（零值）的行永不触发
		}
		if !e.row.NextRunAt.After(now) {
			s.fire(e, now)
		}
	}
}

// fire 触发一次执行：锁内防重判定+登记并同步内存时间（裁决 ③）→
// 落库 last_run_at/next_run_at（裁决 ⑤）→ 异步执行 RunFunc。
// 落库失败仅记日志并解除登记，不触发执行。
func (s *Scheduler) fire(e jobEntry, now time.Time) {
	next := e.sched.Next(now) // 永不匹配时为零值
	s.mu.Lock()
	if s.running[e.row.ID] {
		s.mu.Unlock()
		return // 上一次执行未结束，本周期不重入
	}
	s.running[e.row.ID] = true
	e.row.LastRunAt = now
	e.row.NextRunAt = next
	s.updateEntryLocked(e)
	s.mu.Unlock()

	if _, err := s.db.Exec(`UPDATE schedules SET last_run_at=?, next_run_at=? WHERE id=?`,
		formatTime(now), nullableTime(next), e.row.ID); err != nil {
		log.Printf("scheduler: schedule %d 落库触发时间失败: %v", e.row.ID, err)
		s.mu.Lock()
		delete(s.running, e.row.ID)
		s.mu.Unlock()
		return
	}
	if next.IsZero() {
		log.Printf("scheduler: schedule %d(%s) 表达式 %q 永不匹配，next_run_at 置空",
			e.row.ID, e.row.Kind, e.row.CronExpr)
	}
	go s.exec(e.row)
}

// exec 在独立 goroutine 中执行 RunFunc：panic recover 防击穿，
// 执行失败不回滚已落库的时间（下个周期自然重试），结束后解除防重登记。
func (s *Scheduler) exec(row SchedulesRow) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("scheduler: schedule %d(%s) RunFunc panic: %v", row.ID, row.Kind, r)
		}
		s.mu.Lock()
		delete(s.running, row.ID)
		s.mu.Unlock()
	}()
	if err := s.run(row); err != nil {
		log.Printf("scheduler: schedule %d(%s) 执行失败: %v", row.ID, row.Kind, err)
	}
}

// updateEntryLocked 按 ID 更新内存条目（须持有 s.mu；不存在则忽略——可能已被 Reload 移除）。
func (s *Scheduler) updateEntryLocked(e jobEntry) {
	for i := range s.jobs {
		if s.jobs[i].row.ID == e.row.ID {
			s.jobs[i] = e
			return
		}
	}
}

const scheduleCols = `id, instance_id, kind, cron_expr, payload, enabled, last_run_at, next_run_at`

// loadRows 读取 schedules 全表（时间列为可空文本，解析失败置零值）。
func (s *Scheduler) loadRows() ([]SchedulesRow, error) {
	rows, err := s.db.Query(`SELECT ` + scheduleCols + ` FROM schedules ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SchedulesRow
	for rows.Next() {
		var r SchedulesRow
		var enabled int64
		var last, next sql.NullString
		if err := rows.Scan(&r.ID, &r.InstanceID, &r.Kind, &r.CronExpr, &r.Payload,
			&enabled, &last, &next); err != nil {
			return nil, err
		}
		r.Enabled = enabled != 0
		r.LastRunAt = parseTimestamp(last.String)
		r.NextRunAt = parseTimestamp(next.String)
		out = append(out, r)
	}
	return out, rows.Err()
}

// formatTime 把时间写为库中文本（UTC，沿用 backup 存储层文本格式惯例）。
func formatTime(t time.Time) string { return t.UTC().Format(timeLayout) }

// nullableTime 零值写 NULL（next_run_at 置空），其余写 UTC 文本。
func nullableTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format(timeLayout)
}

// parseTimestamp 把库中 datetime 文本转为 time.Time（UTC）；
// 空值/解析失败置零值不报错（沿用 backup 存储层惯例：时间列容忍脏数据）。
func parseTimestamp(ts string) time.Time {
	if t, err := time.Parse(timeLayout, ts); err == nil {
		return t
	}
	return time.Time{}
}
