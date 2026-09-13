package supervisor

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"palpanel/internal/db"
	"palpanel/internal/event"
	"palpanel/internal/instance"
)

// ---- fake 进程 ----

type fakeProc struct {
	mu        sync.Mutex
	waitErr   error
	done      chan struct{}
	killed    bool
	killCount int
}

func newFakeProc() *fakeProc { return &fakeProc{done: make(chan struct{})} }

func (f *fakeProc) Pid() int { return 4242 }

// Kill 记录强杀并立即让 Wait 返回（模拟 taskkill 成功后进程消失）。
func (f *fakeProc) Kill() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.killed = true
	f.killCount++
	select {
	case <-f.done:
	default:
		close(f.done)
	}
	return nil
}

func (f *fakeProc) Wait() error {
	<-f.done
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.waitErr
}

// finish 进程以 err 退出（幂等关闭 done，避免与 Kill 双 close panic）。
func (f *fakeProc) finish(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.waitErr = err
	select {
	case <-f.done:
	default:
		close(f.done)
	}
}

func (f *fakeProc) isKilled() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.killed
}

// ---- 测试管理器装配（全注入，零真进程） ----

type fakeProber struct{ err error }

func (p fakeProber) Probe(ctx context.Context, inst instance.Instance) error { return p.err }

type testEnv struct {
	m       *Manager
	store   *instance.Store
	hub     *event.Hub
	logRoot string

	mu         sync.Mutex
	startArgs  []string // 每轮 StartFn 收到的参数首项
	procs      []*fakeProc
	killerPIDs []int
}

func newTestManager(t *testing.T, makeOut func() io.ReadCloser) *testEnv {
	t.Helper()
	dir := t.TempDir()
	database, err := db.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	if err := db.Migrate(database); err != nil {
		t.Fatal(err)
	}
	st := instance.New(database)
	hub := event.NewHub()
	env := &testEnv{m: New(database, st, hub), store: st, hub: hub, logRoot: dir}
	env.m.LogRoot = dir
	env.m.Killer = func(pid int) error {
		env.mu.Lock()
		env.killerPIDs = append(env.killerPIDs, pid)
		var p *fakeProc
		if n := len(env.procs); n > 0 {
			p = env.procs[n-1] // 模拟真 Killer：杀掉当前进程，Wait 随之返回
		}
		env.mu.Unlock()
		if p != nil {
			return p.Kill()
		}
		return nil
	}
	if makeOut == nil {
		makeOut = func() io.ReadCloser { return io.NopCloser(strings.NewReader("")) }
	}
	env.m.StartFn = func(inst instance.Instance, args []string) (RunningProcess, io.ReadCloser, error) {
		p := newFakeProc()
		env.mu.Lock()
		env.startArgs = append(env.startArgs, args[0])
		env.procs = append(env.procs, p)
		env.mu.Unlock()
		return p, makeOut(), nil
	}
	return env
}

func (env *testEnv) seedInstance(t *testing.T, autostart bool) int64 {
	t.Helper()
	id, err := env.store.Create(instance.Instance{
		Name:             "test",
		GameDir:          filepath.Join(t.TempDir(), "PalServer"), // game_dir UNIQUE
		GamePort:         8211,
		QueryPort:        27015,
		AdminPasswordEnc: []byte{1},
		Autostart:        autostart,
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (env *testEnv) firstArgs() string {
	env.mu.Lock()
	defer env.mu.Unlock()
	if len(env.startArgs) == 0 {
		return ""
	}
	return env.startArgs[0]
}

func (env *testEnv) proc(i int) *fakeProc {
	env.mu.Lock()
	defer env.mu.Unlock()
	return env.procs[i]
}

func (env *testEnv) procCount() int {
	env.mu.Lock()
	defer env.mu.Unlock()
	return len(env.procs)
}

func (env *testEnv) killerCalls() int {
	env.mu.Lock()
	defer env.mu.Unlock()
	return len(env.killerPIDs)
}

func (env *testEnv) dbStatus(t *testing.T, id int64) string {
	t.Helper()
	var s string
	if err := env.store.DB.QueryRow(`SELECT status FROM instances WHERE id=?`, id).Scan(&s); err != nil {
		t.Fatalf("读 DB status: %v", err)
	}
	return s
}

func waitState(t *testing.T, env *testEnv, id int64, want string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if st, ok := env.m.Status(id); ok && st.State == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	st, ok := env.m.Status(id)
	t.Fatalf("状态未到达 %q：Status=(%+v,%v) DB status=%q", want, st, ok, env.dbStatus(t, id))
}

// waitEvent 等待 hub 上出现指定类型事件（带超时）。
func waitEvent(t *testing.T, ch <-chan event.Event, typ string, d time.Duration) event.Event {
	t.Helper()
	deadline := time.After(d)
	for {
		select {
		case e := <-ch:
			if e.Type == typ {
				return e
			}
		case <-deadline:
			t.Fatalf("超时未收到事件 %q", typ)
		}
	}
}

// ---- 断言点 1+5：running 达成 + 参数首项 -port= ----

func TestStartProbeOKRunning(t *testing.T) {
	env := newTestManager(t, nil)
	id := env.seedInstance(t, false)
	// running 后 200ms 进程以 nil 退出（正常退出）
	go func() {
		time.Sleep(200 * time.Millisecond)
		env.proc(0).finish(nil)
	}()
	sub, cancel := env.hub.Subscribe()
	defer cancel()

	env.m.SetProber(fakeProber{nil})
	if err := env.m.Start(id); err != nil {
		t.Fatal(err)
	}
	// 重复 Start → ErrAlreadyRunning
	if err := env.m.Start(id); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("重复 Start 应 ErrAlreadyRunning，got %v", err)
	}
	waitState(t, env, id, "running", 3*time.Second)
	if got := env.firstArgs(); got != "-port=8211" {
		t.Fatalf("args[0]=%q，want -port=8211", got)
	}
	waitState(t, env, id, "idle", 3*time.Second) // 正常退出回 idle
	waitEvent(t, sub, "instance.status", time.Second)
	if env.dbStatus(t, id) != "idle" {
		t.Fatalf("DB status=%q", env.dbStatus(t, id))
	}
	// 不存在的实例 → ErrNotFound
	if err := env.m.Start(9999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在实例应 ErrNotFound，got %v", err)
	}
}

// ---- 断言点 2：探活超时杀进程 ----

func TestStartProbeTimeout(t *testing.T) {
	env := newTestManager(t, nil)
	id := env.seedInstance(t, false)
	env.m.SetProber(fakeProber{errors.New("connection refused")})
	env.m.ProbeInterval = 5 * time.Millisecond
	env.m.ProbeTimeout = 60 * time.Millisecond

	if err := env.m.Start(id); err != nil {
		t.Fatal(err)
	}
	waitState(t, env, id, "idle", 3*time.Second) // 超时回 idle
	if !env.proc(0).isKilled() {
		t.Fatal("探活超时后进程应被强杀")
	}
	if env.killerCalls() == 0 {
		t.Fatal("探活超时应触发 Killer 兜底")
	}
	if env.dbStatus(t, id) != "idle" {
		t.Fatalf("DB status=%q", env.dbStatus(t, id))
	}
}

// ---- 断言点 3：崩溃重启上限 ----

func TestCrashAutoRestartGivesUp(t *testing.T) {
	env := newTestManager(t, nil)
	id := env.seedInstance(t, true) // autostart=true 才走重启
	env.m.SetProber(fakeProber{nil})
	env.m.RestartBackoffBase = 5 * time.Millisecond
	env.m.MaxRestarts = 2

	// 每轮进程（StartFn 每轮返回新 fakeProc）启动 20ms 后崩溃
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		seen := map[int]bool{}
		for {
			select {
			case <-stop:
				return
			default:
			}
			for i := 0; i < env.procCount(); i++ {
				if seen[i] {
					continue
				}
				seen[i] = true
				p := env.proc(i)
				go func() {
					time.Sleep(20 * time.Millisecond)
					p.finish(errors.New("segfault"))
				}()
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()

	sub, cancel := env.hub.Subscribe()
	defer cancel()
	if err := env.m.Start(id); err != nil {
		t.Fatal(err)
	}

	waitState(t, env, id, "idle", 5*time.Second)
	st, ok := env.m.Status(id)
	if !ok {
		t.Fatal("放弃重启后 Status 应仍可查（保留重启计数）")
	}
	if st.Restarts < 2 {
		t.Fatalf("Restarts=%d，want >= MaxRestarts(2)", st.Restarts)
	}
	waitEvent(t, sub, "instance.crashed", 3*time.Second) // 放弃时广播 crashed
	if n := env.procCount(); n != 3 {                    // 首启 + 2 次重启，第 4 次不再启动
		t.Fatalf("启动轮数=%d，want 3", n)
	}
	if env.dbStatus(t, id) != "idle" {
		t.Fatalf("DB status=%q", env.dbStatus(t, id))
	}
	// 放弃后重新 Start 应可用（terminated 条目不阻塞新一轮）
	if err := env.m.Start(id); err != nil {
		t.Fatalf("放弃重启后 Start 应成功，got %v", err)
	}
	waitState(t, env, id, "running", 3*time.Second)
	_ = env.m.Stop(id)
	waitState(t, env, id, "idle", 3*time.Second)
}

// 崩溃且 autostart=false：不重启，广播 crashed，回 idle。
func TestCrashNoAutostartNoRestart(t *testing.T) {
	env := newTestManager(t, nil)
	id := env.seedInstance(t, false)
	env.m.SetProber(fakeProber{nil})
	sub, cancel := env.hub.Subscribe()
	defer cancel()

	if err := env.m.Start(id); err != nil {
		t.Fatal(err)
	}
	waitState(t, env, id, "running", 3*time.Second)
	env.proc(0).finish(errors.New("boom"))
	waitState(t, env, id, "idle", 3*time.Second)
	waitEvent(t, sub, "instance.crashed", 3*time.Second)
	if n := env.procCount(); n != 1 {
		t.Fatalf("非 autostart 不应重启，启动轮数=%d", n)
	}
}

// ---- 断言点 4：优雅停机失败强杀 ----

func TestStopKillsWhenGraceFails(t *testing.T) {
	// 未注入任何 StopHooks：优雅链无事可做，应立即走强杀兜底
	env := newTestManager(t, nil)
	id := env.seedInstance(t, false)
	env.m.SetProber(fakeProber{nil})
	env.m.StopGraceWait = 50 * time.Millisecond

	if err := env.m.Start(id); err != nil {
		t.Fatal(err)
	}
	waitState(t, env, id, "running", 3*time.Second)
	if err := env.m.Stop(id); err != nil {
		t.Fatal(err)
	}
	waitState(t, env, id, "idle", 3*time.Second)
	if !env.proc(0).isKilled() {
		t.Fatal("优雅停机失败（无钩子）应强杀进程")
	}
	if err := env.m.Stop(id); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("Stop 后再 Stop 应 ErrNotRunning，got %v", err)
	}
}

func TestStopGracefulHooksFailForceKill(t *testing.T) {
	env := newTestManager(t, nil)
	id := env.seedInstance(t, false)
	env.m.SetProber(fakeProber{nil})
	env.m.StopGraceWait = 50 * time.Millisecond

	if err := env.m.Start(id); err != nil {
		t.Fatal(err)
	}
	waitState(t, env, id, "running", 3*time.Second)
	env.m.SetHooks(id, StopHooks{
		REST: func(ctx context.Context) error { return errors.New("rest down") },
		RCON: func(ctx context.Context) error { return errors.New("rcon refused") },
	})
	start := time.Now()
	if err := env.m.Stop(id); err != nil {
		t.Fatal(err)
	}
	if el := time.Since(start); el > 3*time.Second {
		t.Fatalf("Stop 耗时 %v，强杀兜底未生效", el)
	}
	waitState(t, env, id, "idle", 3*time.Second)
	if !env.proc(0).isKilled() {
		t.Fatal("优雅钩子全部失败后应强杀进程")
	}
}

// ---- 日志流：行 → 截断写文件 + Hub 广播 Type:"log" ----

func TestLogStreamToFileAndHub(t *testing.T) {
	pr, pw := io.Pipe()
	env := newTestManager(t, func() io.ReadCloser { return pr })
	id := env.seedInstance(t, false)
	env.m.SetProber(fakeProber{nil})
	sub, cancel := env.hub.Subscribe()
	defer cancel()

	if err := env.m.Start(id); err != nil {
		t.Fatal(err)
	}
	waitState(t, env, id, "running", 3*time.Second)

	lines := []string{"Paragon log line 1", "世界你好 line 2"}
	go func() {
		for _, l := range lines {
			if _, err := pw.Write([]byte(l + "\n")); err != nil {
				return
			}
		}
		pw.Close() // EOF，scanner goroutine 退出
	}()

	logPath := filepath.Join(env.logRoot, "instances", "1", "logs", "console.log")
	deadline := time.Now().Add(3 * time.Second)
	for {
		b, err := os.ReadFile(logPath)
		if err == nil && strings.Contains(string(b), "line 2") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("日志文件未写入完整内容：%q err=%v", string(b), err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	e1 := waitEvent(t, sub, "log", 3*time.Second)
	if e1.Payload != lines[0] {
		t.Fatalf("hub log payload=%v", e1.Payload)
	}
	if e1.InstanceID != id {
		t.Fatalf("hub log InstanceID=%d，want %d", e1.InstanceID, id)
	}

	// 进程正常退出收尾，验证无 goroutine/句柄悬挂导致的卡死
	env.proc(0).finish(nil)
	waitState(t, env, id, "idle", 3*time.Second)
}

// ---- Critical 回归：StartFn 未注入时不得 nil panic ----

// Start 同步返回 ErrNoStarter（New 文档契约"未注入时返回启动失败"）。
func TestStartNilStartFnReturnsErrNoStarter(t *testing.T) {
	env := newTestManager(t, nil)
	env.m.StartFn = nil // 显式未注入
	id := env.seedInstance(t, false)

	if err := env.m.Start(id); !errors.Is(err, ErrNoStarter) {
		t.Fatalf("StartFn 未注入时 Start 应返回 ErrNoStarter，got %v", err)
	}
	// 同步拒绝：不创建 procInfo（Status 查无此轮），DB 状态保持 idle
	if _, ok := env.m.Status(id); ok {
		t.Fatal("Start 被拒绝不应留下运行时状态条目")
	}
	if s := env.dbStatus(t, id); s != "idle" {
		t.Fatalf("DB status=%q，want idle", s)
	}
	if env.procCount() != 0 {
		t.Fatalf("未注入 StartFn 不应拉起进程，轮数=%d", env.procCount())
	}
}

// loop 内兜底：覆盖"先注入、运行中被置 nil"的竞态注入时序。
// wrapper 在 loop goroutine 内执行，同 goroutine 内改写 m.StartFn 无数据竞争。
func TestLoopNilStartFnFallback(t *testing.T) {
	env := newTestManager(t, nil)
	id := env.seedInstance(t, true) // autostart=true → 崩溃后进入重启轮
	env.m.SetProber(fakeProber{nil})
	env.m.RestartBackoffBase = 5 * time.Millisecond // 缩短退避，加速进入重启轮
	origStart := env.m.StartFn
	env.m.StartFn = func(inst instance.Instance, args []string) (RunningProcess, io.ReadCloser, error) {
		env.m.StartFn = nil // 首轮成功启动后置 nil，下一轮 loop 观察到
		return origStart(inst, args)
	}
	sub, cancel := env.hub.Subscribe()
	defer cancel()

	if err := env.m.Start(id); err != nil {
		t.Fatal(err)
	}
	waitState(t, env, id, "running", 3*time.Second)
	env.proc(0).finish(errors.New("boom")) // 崩溃 → 重启轮 StartFn 为 nil

	// 修复前此处 loop goroutine nil 函数调用 panic 击穿测试进程
	waitState(t, env, id, "idle", 3*time.Second)
	e := waitEvent(t, sub, "instance.error", 3*time.Second)
	if !strings.Contains(fmt.Sprint(e.Payload), ErrNoStarter.Error()) {
		t.Fatalf("instance.error payload=%v，应含 ErrNoStarter 文案", e.Payload)
	}
	if env.dbStatus(t, id) != "idle" {
		t.Fatalf("DB status=%q", env.dbStatus(t, id))
	}
}

// ---- Important 回归：优雅停机成功路径零强杀（存档安全） ----

func TestStopGracefulSuccessNoKill(t *testing.T) {
	env := newTestManager(t, nil)
	id := env.seedInstance(t, false)
	env.m.SetProber(fakeProber{nil})
	env.m.StopGraceWait = time.Second
	sub, cancel := env.hub.Subscribe()
	defer cancel()

	if err := env.m.Start(id); err != nil {
		t.Fatal(err)
	}
	waitState(t, env, id, "running", 3*time.Second)
	env.m.SetHooks(id, StopHooks{
		REST: func(ctx context.Context) error {
			env.proc(0).finish(nil) // 模拟进程收到 REST 停服请求后自行退出
			return nil
		},
	})
	if err := env.m.Stop(id); err != nil {
		t.Fatal(err)
	}
	waitState(t, env, id, "idle", 3*time.Second)
	if env.killerCalls() != 0 {
		t.Fatalf("优雅停机成功不应触发 Killer，实际 %d 次", env.killerCalls())
	}
	if env.proc(0).isKilled() {
		t.Fatal("优雅停机成功路径进程不应被 Kill")
	}
	// 存档安全关键断言：成功停机不广播 instance.crashed
	deadline := time.After(200 * time.Millisecond)
	for {
		select {
		case e := <-sub:
			if e.Type == "instance.crashed" {
				t.Fatalf("优雅停机成功不应广播 instance.crashed：%v", e.Payload)
			}
			continue
		case <-deadline:
		}
		break
	}
	if env.dbStatus(t, id) != "idle" {
		t.Fatalf("DB status=%q", env.dbStatus(t, id))
	}
}

// ---- ArgsFor 纯函数 ----

func TestArgsFor(t *testing.T) {
	args := ArgsFor(instance.Instance{GamePort: 8211, QueryPort: 27015})
	want := []string{"-port=8211", "-queryport=27015", "-useperfthreads", "-NoAsyncLoadingThread", "-UseMultithreadForDS"}
	if len(args) != len(want) {
		t.Fatalf("args=%v", args)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("args[%d]=%q，want %q", i, args[i], want[i])
		}
	}
}

// ---- 零值默认参数（裁决④：字段为零值时取默认，显式设置不被覆盖） ----

func TestDefaultsWhenFieldsZero(t *testing.T) {
	m := New(&sql.DB{}, instance.New(&sql.DB{}), event.NewHub())
	if m.probeInterval() != 3*time.Second || m.probeTimeout() != 90*time.Second {
		t.Fatalf("probe 默认值错误：%v %v", m.probeInterval(), m.probeTimeout())
	}
	if m.restartBackoffBase() != 10*time.Second || m.maxRestarts() != 5 || m.stopGraceWait() != 35*time.Second {
		t.Fatalf("restart/stop 默认值错误：%v %v %v", m.restartBackoffBase(), m.maxRestarts(), m.stopGraceWait())
	}
	m.MaxRestarts = 2
	if m.maxRestarts() != 2 {
		t.Fatalf("MaxRestarts=2 被默认值覆盖为 %d", m.maxRestarts())
	}
}

// ---- 回归：重启轮 StartFn 失败不 double close panic ----

func TestRestartRoundStartFnFailureNoPanic(t *testing.T) {
	env := newTestManager(t, nil)
	id := env.seedInstance(t, true) // autostart=true → 进入重启轮
	env.m.SetProber(fakeProber{nil})
	env.m.RestartBackoffBase = 5 * time.Millisecond
	env.m.MaxRestarts = 5

	origStart := env.m.StartFn
	var mu sync.Mutex
	var n int
	env.m.StartFn = func(inst instance.Instance, args []string) (RunningProcess, io.ReadCloser, error) {
		mu.Lock()
		n++
		call := n
		mu.Unlock()
		if call == 2 {
			// 仅重启轮（第 2 次调用）拉起失败；后续调用正常（供重新 Start 用）
			return nil, nil, errors.New("port already in use")
		}
		return origStart(inst, args)
	}
	sub, cancel := env.hub.Subscribe()
	defer cancel()

	if err := env.m.Start(id); err != nil {
		t.Fatal(err)
	}
	waitState(t, env, id, "running", 3*time.Second)
	env.proc(0).finish(errors.New("boom")) // 崩溃 → 重启轮 StartFn 失败

	// 修复前此处 loop goroutine double close panic 击穿测试进程
	waitState(t, env, id, "idle", 3*time.Second)
	waitEvent(t, sub, "instance.error", 3*time.Second)
	if env.dbStatus(t, id) != "idle" {
		t.Fatalf("DB status=%q", env.dbStatus(t, id))
	}
	st, ok := env.m.Status(id)
	if !ok || st.Restarts != 1 {
		t.Fatalf("Status=%+v ok=%v，want Restarts=1", st, ok)
	}
	if n := env.procCount(); n != 1 {
		t.Fatalf("启动轮数=%d，want 1（重启轮未拉起）", n)
	}
	// 终止后仍可重新 Start
	if err := env.m.Start(id); err != nil {
		t.Fatalf("重新 Start 应成功，got %v", err)
	}
	waitState(t, env, id, "running", 3*time.Second)
	_ = env.m.Stop(id)
	waitState(t, env, id, "idle", 3*time.Second)
}

// ---- 回归：StartFn 返回前 Stop，进程不得进入 running ----

func TestStopDuringStartFnAbortsStart(t *testing.T) {
	env := newTestManager(t, nil)
	id := env.seedInstance(t, false)
	env.m.SetProber(fakeProber{nil})
	release := make(chan struct{})
	origStart := env.m.StartFn
	env.m.StartFn = func(inst instance.Instance, args []string) (RunningProcess, io.ReadCloser, error) {
		<-release // 模拟启动器阻塞（解压/校验等）
		return origStart(inst, args)
	}
	sub, cancel := env.hub.Subscribe()
	defer cancel()

	if err := env.m.Start(id); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond) // 确保 Stop 落在 StartFn 返回之前
	stopErr := make(chan error, 1)
	go func() { stopErr <- env.m.Stop(id) }()
	time.Sleep(20 * time.Millisecond) // Stop 已设 stopReq、阻塞在 StartFn 窗口
	close(release)                    // StartFn 此时才返回，loop 应立即中止启动
	if err := <-stopErr; err != nil {
		t.Fatal(err)
	}

	waitState(t, env, id, "idle", 2*time.Second)
	// 进程从未进入 running：Stop 返回前广播序列中不应出现 running
	for {
		select {
		case e := <-sub:
			if e.Type == "instance.status" && e.Payload == "running" {
				t.Fatal("StartFn 返回前的 Stop 不应让进程进入 running")
			}
			continue
		default:
		}
		break
	}
	if st, ok := env.m.Status(id); !ok || st.PID != 0 {
		t.Fatalf("Status=%+v ok=%v，启动被中止不应残留 PID", st, ok)
	}
	// 状态机仍可用
	if err := env.m.Start(id); err != nil {
		t.Fatalf("中止后 Start 应成功，got %v", err)
	}
	waitState(t, env, id, "running", 3*time.Second)
	_ = env.m.Stop(id)
	waitState(t, env, id, "idle", 3*time.Second)
}

// ---- 回归：退避期 Stop 状态及时落 idle（不睡满退避） ----

func TestStopDuringBackoffSettlesIdle(t *testing.T) {
	env := newTestManager(t, nil)
	id := env.seedInstance(t, true)
	env.m.SetProber(fakeProber{nil})
	env.m.RestartBackoffBase = 1 * time.Second // 退避 1s，期间 Stop 必须能中断
	env.m.MaxRestarts = 1

	if err := env.m.Start(id); err != nil {
		t.Fatal(err)
	}
	waitState(t, env, id, "running", 3*time.Second)
	env.proc(0).finish(errors.New("boom")) // 崩溃 → restarts=1 → 退避 1s
	time.Sleep(30 * time.Millisecond)      // 落入退避窗口

	start := time.Now()
	if err := env.m.Stop(id); err != nil {
		t.Fatal(err)
	}
	waitState(t, env, id, "idle", 500*time.Millisecond) // 修复前需睡满 1s 退避
	if el := time.Since(start); el > 500*time.Millisecond {
		t.Fatalf("退避期 Stop 落定耗时 %v，退避未被中断", el)
	}
	if st, ok := env.m.Status(id); !ok || st.Restarts != 1 {
		t.Fatalf("Status=%+v ok=%v，want Restarts=1", st, ok)
	}
	if n := env.procCount(); n != 1 {
		t.Fatalf("启动轮数=%d，退避期 Stop 不应再拉起新一轮", n)
	}
	if env.dbStatus(t, id) != "idle" {
		t.Fatalf("DB status=%q", env.dbStatus(t, id))
	}
}
