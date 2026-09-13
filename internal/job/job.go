package job

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"palpanel/internal/event"
)

// ErrDuplicate 表示同实例同 kind 的任务已在执行。
var ErrDuplicate = errors.New("该实例已有同类任务在执行")

// 保留策略：任务总数超过 maxJobs 时，删除已结束且结束超过 retainPeriod 的任务。
const (
	maxJobs      = 200
	retainPeriod = time.Hour
)

// 任务状态。
const (
	StateRunning = "running"
	StateDone    = "done"
	StateFailed  = "failed"
)

// Job 是一个异步任务的快照。字段更新全在 Manager.mu 内，Get/List 返回拷贝。
type Job struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	InstanceID int64  `json:"instance_id"`
	State      string `json:"state"`
	Progress   int    `json:"progress"`
	Message    string `json:"message"`
	Error      string `json:"error,omitempty"`

	finishedAt time.Time // 完成时刻（保留策略用；不对外序列化）
}

// Manager 管理安装/更新等长任务：去重、进度上报、状态流转与事件广播。
type Manager struct {
	hub     *event.Hub
	mu      sync.Mutex
	jobs    map[string]*Job
	running map[string]string // "instanceID:kind" -> jobID
	seq     int
	// Now 可注入时钟（nil → time.Now），测试用。
	Now func() time.Time
}

// NewManager 创建任务管理器，任务状态变更通过 hub 广播（Type: "job"）。
func NewManager(hub *event.Hub) *Manager {
	return &Manager{hub: hub, jobs: map[string]*Job{}, running: map[string]string{}}
}

// Start 启动一个新任务；同实例同 kind 已有任务在执行时返回 ErrDuplicate。
// fn 在独立 goroutine 中执行，通过 report 上报进度，返回值决定任务成败。
func (m *Manager) Start(kind string, instanceID int64, fn func(ctx context.Context, report func(progress int, message string)) error) (string, error) {
	key := fmt.Sprintf("%d:%s", instanceID, kind)
	m.mu.Lock()
	if _, dup := m.running[key]; dup {
		m.mu.Unlock()
		return "", ErrDuplicate
	}
	m.seq++
	id := fmt.Sprintf("job-%d", m.seq)
	j := &Job{ID: id, Kind: kind, InstanceID: instanceID, State: StateRunning, Message: "任务已开始"}
	m.jobs[id] = j
	m.running[key] = id
	m.gcLocked(m.now()) // 惰性清理：超过保留量时删除过期任务
	m.mu.Unlock()
	m.broadcast(j)
	report := func(progress int, message string) {
		m.mu.Lock()
		j.Progress, j.Message = progress, message
		m.mu.Unlock()
		m.broadcast(j)
	}
	go func() {
		// recover 兜底：fn panic 不能带崩进程，按失败任务收尾并释放 running 键
		err := func() (err error) {
			defer func() {
				if r := recover(); r != nil {
					err = fmt.Errorf("内部错误: %v", r)
				}
			}()
			return fn(context.Background(), report)
		}()
		m.mu.Lock()
		delete(m.running, key)
		j.finishedAt = m.now()
		if err != nil {
			j.State, j.Error = StateFailed, err.Error()
		} else {
			j.State, j.Progress, j.Message = StateDone, 100, "完成"
		}
		m.gcLocked(m.now())
		m.mu.Unlock()
		m.broadcast(j)
	}()
	return id, nil
}

func (m *Manager) broadcast(j *Job) {
	m.hub.Broadcast(event.Event{Type: "job", InstanceID: j.InstanceID, Payload: *j})
}

// now 返回当前时间（注入时钟或系统时间）。
func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// gcLocked 惰性清理（须持 m.mu）：任务总数超 maxJobs 时，
// 删除已结束且结束超过 retainPeriod 的任务；运行中任务一律保留。
func (m *Manager) gcLocked(now time.Time) {
	if len(m.jobs) <= maxJobs {
		return
	}
	for id, j := range m.jobs {
		if j.State == StateRunning {
			continue
		}
		if !j.finishedAt.IsZero() && now.Sub(j.finishedAt) > retainPeriod {
			delete(m.jobs, id)
		}
	}
}

// RunningOfKind 报告该实例是否有指定 kind 的任务正在执行。
func (m *Manager) RunningOfKind(instanceID int64, kind string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.running[fmt.Sprintf("%d:%s", instanceID, kind)]
	return ok
}

// Get 按 ID 返回任务快照（值拷贝）。
func (m *Manager) Get(id string) (Job, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return Job{}, false
	}
	return *j, true
}

// List 返回全部任务快照（值拷贝）。
func (m *Manager) List() []Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Job, 0, len(m.jobs))
	for _, j := range m.jobs {
		out = append(out, *j)
	}
	return out
}
