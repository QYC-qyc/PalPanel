// Package supervisor 是面板核心引擎：实例进程守护。
// 职责：启停状态机（idle/starting/running）、探活、崩溃退避重启、
// 日志流（stdout → console.log + 事件广播）、优雅停机链（REST→RCON→强杀）。
//
// 并发纪律（M2 裁决）：
//   - 任何 DB 调用不持有 m.mu（SQLite MaxOpenConns(1) 防死锁）；
//   - 每轮进程的 done 通道只在 loop goroutine 内关闭；
//   - procs 表的增改全在 m.mu 内；
//   - procInfo 在 Start 时创建、跨重启轮保留 restarts 计数，进程终结后
//     以 terminated 标记留在表中，供 Status 查询最近一次运行的重启计数，
//     且不阻塞下一轮 Start。
package supervisor

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"palpanel/internal/event"
	"palpanel/internal/instance"
)

// 状态机状态值（与 instances.status 列一致）。
const (
	StateIdle     = "idle"
	StateStarting = "starting"
	StateRunning  = "running"
)

var (
	ErrAlreadyRunning = errors.New("实例已在运行")
	ErrNotRunning     = errors.New("实例未在运行")
	ErrNotFound       = errors.New("实例不存在")
	// ErrNoStarter 表示 Manager.StartFn 未注入（生产装配遗漏），启动被拒绝。
	ErrNoStarter = errors.New("进程启动器未配置")
)

// RunningProcess 是被守护的进程句柄抽象（生产由 os/exec 适配，测试用假实现）。
type RunningProcess interface {
	Pid() int
	Wait() error
	Kill() error
}

// Starter 启动一个进程，返回句柄与其 stdout（逐行读为日志流）。
type Starter func(inst instance.Instance, args []string) (RunningProcess, io.ReadCloser, error)

// StopHooks 是优雅停机钩子，由 API 层按实例构造（解密密码 → gateway/rcon），
// supervisor 不直接依赖 gateway/rcon（避免依赖环）。
type StopHooks struct {
	REST func(ctx context.Context) error
	RCON func(ctx context.Context) error
}

// Prober 是启动探活器（如 REST /healthz 轮询）。
type Prober interface {
	Probe(ctx context.Context, inst instance.Instance) error
}

// Status 是实例的运行时状态快照。
type Status struct {
	State     string    `json:"state"`
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"started_at"`
	Restarts  int       `json:"restarts"`
}

type procInfo struct {
	inst       instance.Instance
	p          RunningProcess
	startedAt  time.Time
	restarts   int
	stopReq    bool
	terminated bool
	state      string
	done       chan struct{} // 本轮进程 Wait 返回后由 loop 关闭
	hooks      StopHooks
}

// Manager 进程守护管理器。StartFn/Killer/Prober/LogRoot 全部可注入。
type Manager struct {
	DB      *sql.DB
	Store   *instance.Store
	Hub     *event.Hub
	StartFn Starter
	Killer  func(pid int) error
	LogRoot string
	// Prober 启动探活器；须在首次 Start 之前注入（Start 后替换不生效）。
	Prober Prober

	// 可调参数（测试注入；零值时经 getter 取生产默认值）。
	ProbeInterval      time.Duration
	ProbeTimeout       time.Duration
	RestartBackoffBase time.Duration
	MaxRestarts        int
	StopGraceWait      time.Duration

	mu    sync.Mutex
	procs map[int64]*procInfo
	hooks map[int64]StopHooks
}

// New 创建管理器。StartFn 需调用方注入（未注入时 Start 返回 ErrNoStarter）。
func New(db *sql.DB, store *instance.Store, hub *event.Hub) *Manager {
	return &Manager{
		DB:    db,
		Store: store,
		Hub:   hub,
		Killer: func(pid int) error {
			return defaultKill(pid)
		},
		procs: map[int64]*procInfo{},
		hooks: map[int64]StopHooks{},
	}
}

// 可调参数 getter：字段为零值时取生产默认（裁决④）。

func (m *Manager) probeInterval() time.Duration {
	if m.ProbeInterval > 0 {
		return m.ProbeInterval
	}
	return 3 * time.Second
}

func (m *Manager) probeTimeout() time.Duration {
	if m.ProbeTimeout > 0 {
		return m.ProbeTimeout
	}
	return 90 * time.Second
}

func (m *Manager) restartBackoffBase() time.Duration {
	if m.RestartBackoffBase > 0 {
		return m.RestartBackoffBase
	}
	return 10 * time.Second
}

func (m *Manager) maxRestarts() int {
	if m.MaxRestarts > 0 {
		return m.MaxRestarts
	}
	return 5
}

func (m *Manager) stopGraceWait() time.Duration {
	if m.StopGraceWait > 0 {
		return m.StopGraceWait
	}
	return 35 * time.Second
}

// SetProber 注入启动探活器。必须在首次 Start 之前调用（Start 后替换不生效）。
func (m *Manager) SetProber(p Prober) { m.Prober = p }

// SetHooks 注册实例的优雅停机钩子（API 层在实例配置就绪后调用）。
func (m *Manager) SetHooks(id int64, h StopHooks) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.hooks[id] = h
}

// ArgsFor 生成 PalWorld 服务器启动参数。
func ArgsFor(inst instance.Instance) []string {
	return []string{
		fmt.Sprintf("-port=%d", inst.GamePort),
		fmt.Sprintf("-queryport=%d", inst.QueryPort),
		"-useperfthreads",
		"-NoAsyncLoadingThread",
		"-UseMultithreadForDS",
	}
}

// setState 持久化状态并广播（不得在持 m.mu 时调用——DB 调用纪律）。
func (m *Manager) setState(id int64, state string) {
	_, _ = m.DB.Exec(`UPDATE instances SET status=? WHERE id=?`, state, id)
	m.Hub.Broadcast(event.Event{Type: "instance.status", InstanceID: id, Payload: state})
}

// transition 在 m.mu 内更新内存状态，再无锁落库+广播。
func (m *Manager) transition(pi *procInfo, state string) {
	m.mu.Lock()
	pi.state = state
	m.mu.Unlock()
	m.setState(pi.inst.ID, state)
}

// Status 返回实例运行时状态快照；进程终结后仍可查（保留最近一轮计数）。
func (m *Manager) Status(id int64) (Status, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	pi, ok := m.procs[id]
	if !ok {
		return Status{}, false
	}
	st := Status{State: pi.state, StartedAt: pi.startedAt, Restarts: pi.restarts}
	if pi.p != nil && !pi.terminated {
		st.PID = pi.p.Pid()
	}
	return st, true
}

// Start 异步启动实例进程：starting → 探活成功 running / 失败回 idle。
func (m *Manager) Start(id int64) error {
	m.mu.Lock()
	if pi, ok := m.procs[id]; ok && !pi.terminated {
		m.mu.Unlock()
		return ErrAlreadyRunning
	}
	m.mu.Unlock()

	if m.StartFn == nil { // 兑现 New 契约：未注入启动器时同步拒绝启动
		return ErrNoStarter
	}

	inst, err := m.Store.Get(id) // 不持锁做 DB 调用
	if err != nil {
		if errors.Is(err, instance.ErrNotFound) {
			return ErrNotFound
		}
		return err
	}

	m.mu.Lock()
	if pi, ok := m.procs[id]; ok && !pi.terminated { // 双检：并发 Start
		m.mu.Unlock()
		return ErrAlreadyRunning
	}
	pi := &procInfo{
		inst:  inst,
		state: StateStarting,
		done:  make(chan struct{}),
		hooks: m.hooks[id],
	}
	m.procs[id] = pi
	m.mu.Unlock()

	m.setState(id, StateStarting)
	go m.loop(pi)
	return nil
}

// Stop 优雅停机链：REST(StopGraceWait) → RCON(5s) → 等 StopGraceWait → Killer 强杀。
// stopReq 标志 + 等 done + Killer 兜底；done 只由 loop 关闭。
// stopReq 的消费点：StartFn 返回后立即、探活每轮迭代、退避等待期（均可中断中止）。
func (m *Manager) Stop(id int64) error {
	m.mu.Lock()
	pi, ok := m.procs[id]
	if !ok || pi.terminated {
		m.mu.Unlock()
		return ErrNotRunning
	}
	pi.stopReq = true
	p := pi.p
	done := pi.done
	hooks := pi.hooks
	if h, ok := m.hooks[id]; ok { // 以最新注册为准
		hooks = h
	}
	var pid int
	if p != nil {
		pid = p.Pid()
	}
	m.mu.Unlock()

	if p == nil { // StartFn 尚未返回：loop 会自行收尾并关闭 done
		select {
		case <-done:
		case <-time.After(10 * time.Second):
		}
		return nil
	}

	// 优雅链：REST 失败或缺失 → RCON；两者皆缺/皆败 → 无优雅手段，直接等/杀。
	graceful := false
	if hooks.REST != nil {
		ctx, cancel := context.WithTimeout(context.Background(), m.stopGraceWait())
		err := hooks.REST(ctx)
		cancel()
		if err == nil {
			graceful = true
		} else if hooks.RCON != nil {
			ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
			_ = hooks.RCON(ctx2)
			cancel2()
			graceful = true
		}
	} else if hooks.RCON != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = hooks.RCON(ctx)
		cancel()
		graceful = true
	}
	if graceful {
		select {
		case <-done:
			return nil
		case <-time.After(m.stopGraceWait()):
		}
	}

	// 强杀兜底（taskkill /T /F 树杀）
	_ = m.Killer(pid)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
	}
	return nil
}

// probeResult 是启动期（StartFn 返回后）的结论。
type probeResult int

const (
	probeSuccess probeResult = iota // 探活通过，进入 running
	probeTimeout                    // 探活超时（probe 内已记 stopReq 并强杀）
	probeStopped                    // Stop 已发起（stopReq 命中，probe 内已强杀）
)

// loop 是守护循环：外层 for 为重启轮（attempt 计数），内层为单轮生命周期。
// done 通道按轮管理：轮 0 沿用 Start 创建的占位，后续每轮在轮顶重建；
// 每轮恰有一条互斥出口关闭本轮 done 一次——不存在跨轮 double close。
// done 只在本函数（含其直接调用路径）关闭。
func (m *Manager) loop(pi *procInfo) {
	firstRound := true
	for {
		// 轮顶：重建本轮 done（杜绝上一轮已关闭的通道被复用），
		// 并检查 Stop 是否已在退避期发起（首轮 stopReq 恒为 false）。
		if !firstRound {
			m.mu.Lock()
			pi.done = make(chan struct{})
			m.mu.Unlock()
		}
		firstRound = false
		m.mu.Lock()
		stopReq := pi.stopReq
		m.mu.Unlock()
		if stopReq { // 退避期 Stop：不再拉起，直接收尾
			m.finishIdle(pi)
			close(pi.done)
			return
		}

		var p RunningProcess
		var out io.ReadCloser
		err := error(nil)
		if m.StartFn == nil { // 兜底：Start 后 StartFn 被置 nil 的竞态注入时序
			err = ErrNoStarter
		} else {
			p, out, err = m.StartFn(pi.inst, ArgsFor(pi.inst))
		}
		if err != nil {
			m.Hub.Broadcast(event.Event{Type: "instance.error", InstanceID: pi.inst.ID,
				Payload: "启动失败: " + err.Error()})
			m.finishIdle(pi)
			close(pi.done) // 唤醒可能等待的 Stop
			return
		}

		// 登记本轮进程（restarts 计数保留在占位对象上，跨轮累计）
		m.mu.Lock()
		pi.p = p
		pi.startedAt = time.Now()
		m.mu.Unlock()
		m.transition(pi, StateStarting)

		m.pipeLogs(pi.inst.ID, out)

		// StartFn 返回后立即消费 stopReq：覆盖“StartFn 阻塞期间的 Stop”，
		// 避免进程照常进入 running。
		m.mu.Lock()
		stopReq = pi.stopReq
		m.mu.Unlock()
		res := probeSuccess
		if stopReq {
			_ = m.Killer(p.Pid())
			res = probeStopped
		} else {
			// 探活：成功 → running；超时/Stop 命中 → 已强杀，走 idle 收尾
			res = m.probe(pi, p)
		}

		if res == probeSuccess {
			m.transition(pi, StateRunning)
		}

		// 每轮 Wait 恰好调用一次；收尾（状态落定 + terminate）后关闭本轮 done，
		// 保证 Stop 等 done 返回时状态已最终一致。
		waitErr := p.Wait()

		if res == probeTimeout {
			m.Hub.Broadcast(event.Event{Type: "instance.error", InstanceID: pi.inst.ID,
				Payload: "启动探活超时"})
			m.finishIdle(pi)
			close(pi.done)
			return
		}

		m.mu.Lock()
		stopReq = pi.stopReq
		m.mu.Unlock()
		if stopReq || waitErr == nil { // 主动停机（含探活期 Stop）或正常退出 → idle
			m.finishIdle(pi)
			close(pi.done)
			return
		}

		// 崩溃路径
		if !pi.inst.Autostart {
			m.Hub.Broadcast(event.Event{Type: "instance.crashed", InstanceID: pi.inst.ID,
				Payload: fmt.Sprintf("进程异常退出: %v", waitErr)})
			m.finishIdle(pi)
			close(pi.done)
			return
		}
		m.mu.Lock()
		giveUp := pi.restarts >= m.maxRestarts()
		if !giveUp {
			pi.restarts++
		}
		restarts := pi.restarts
		m.mu.Unlock()
		if giveUp {
			m.Hub.Broadcast(event.Event{Type: "instance.crashed", InstanceID: pi.inst.ID,
				Payload: fmt.Sprintf("连续崩溃 %d 次，放弃重启: %v", restarts, waitErr)})
			m.finishIdle(pi)
			close(pi.done)
			return
		}
		m.transition(pi, StateStarting)
		close(pi.done) // 本轮结束，进入退避等待
		if m.abortedDuringBackoff(pi, m.backoff(restarts)) {
			// 退避期 Stop：立即收尾落 idle，不睡满退避（本轮 done 已关闭）
			m.finishIdle(pi)
			return
		}
	}
}

// abortedDuringBackoff 可中断退避：限时等待，每 100ms 轮询 stopReq，
// 命中返回 true（调用方立即收尾），到点返回 false（继续下一轮）。
func (m *Manager) abortedDuringBackoff(pi *procInfo, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		m.mu.Lock()
		sr := pi.stopReq
		m.mu.Unlock()
		if sr {
			return true
		}
		select {
		case <-timer.C:
			return false
		case <-ticker.C:
		}
	}
}

// backoff 计算第 restarts 次重启前的退避时长：base, 2base, 4base... 上限 5 分钟。
func (m *Manager) backoff(restarts int) time.Duration {
	d := m.restartBackoffBase()
	if k := restarts - 1; k > 0 {
		if k > 20 {
			k = 20
		}
		d = d * (1 << uint(k))
	}
	if d > 5*time.Minute {
		d = 5 * time.Minute
	}
	return d
}

// finishIdle 终态收尾：锁内原子落 state 与 terminated，
// 保证外部观察到 State==idle 时 Status 必已无 PID、且 Start 不再被视为运行中。
func (m *Manager) finishIdle(pi *procInfo) {
	m.mu.Lock()
	pi.state = StateIdle
	pi.terminated = true
	m.mu.Unlock()
	m.setState(pi.inst.ID, StateIdle)
}

// probe 启动探活：Prober 未注入时等待 2s 即视为就绪；
// 注入后按 ProbeInterval 轮询，每轮迭代先消费 stopReq（Stop 命中 → 取消探活、
// 强杀，返回 probeStopped），累计超过 ProbeTimeout 记 stopReq 并强杀（probeTimeout）。
func (m *Manager) probe(pi *procInfo, p RunningProcess) probeResult {
	if m.Prober == nil {
		time.Sleep(2 * time.Second)
		return probeSuccess
	}
	ctx, cancel := context.WithTimeout(context.Background(), m.probeTimeout())
	defer cancel() // 探活 ctx 必须取消，防泄漏
	for {
		m.mu.Lock()
		sr := pi.stopReq
		m.mu.Unlock()
		if sr { // 探活期 Stop：取消探活、杀进程
			_ = m.Killer(p.Pid())
			return probeStopped
		}
		err := m.Prober.Probe(ctx, pi.inst)
		if err == nil {
			return probeSuccess
		}
		select {
		case <-ctx.Done(): // 探活超时：杀进程，回 idle
			m.mu.Lock()
			pi.stopReq = true
			m.mu.Unlock()
			_ = m.Killer(p.Pid())
			return probeTimeout
		case <-time.After(m.probeInterval()):
		}
	}
}

// pipeLogs 把进程 stdout 逐行写 console.log（每次启动截断重开）并广播 Type:"log"。
// 文件句柄与 scanner 由本 goroutine 自持自关。
func (m *Manager) pipeLogs(id int64, out io.ReadCloser) {
	if out == nil {
		return
	}
	logPath := m.logPath(id)
	_ = os.MkdirAll(filepath.Dir(logPath), 0o755)
	lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		// 日志文件打不开也必须消费 stdout，否则管道写端可能阻塞进程
		lf = nil
	}
	go func() {
		if lf != nil {
			defer lf.Close()
		}
		defer out.Close()
		sc := bufio.NewScanner(out)
		sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
		for sc.Scan() {
			line := sc.Text()
			if lf != nil {
				fmt.Fprintln(lf, line)
			}
			m.Hub.Broadcast(event.Event{Type: "log", InstanceID: id, Payload: line})
		}
	}()
}

func (m *Manager) logPath(id int64) string {
	return filepath.Join(m.LogRoot, "instances", strconv.FormatInt(id, 10), "logs", "console.log")
}
