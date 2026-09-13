package job

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"palpanel/internal/event"
)

// ErrDuplicate 表示同实例同 kind 的任务已在执行。
var ErrDuplicate = errors.New("该实例已有同类任务在执行")

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
}

// Manager 管理安装/更新等长任务：去重、进度上报、状态流转与事件广播。
type Manager struct {
	hub     *event.Hub
	mu      sync.Mutex
	jobs    map[string]*Job
	running map[string]string // "instanceID:kind" -> jobID
	seq     int
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
	m.mu.Unlock()
	m.broadcast(j)
	report := func(progress int, message string) {
		m.mu.Lock()
		j.Progress, j.Message = progress, message
		m.mu.Unlock()
		m.broadcast(j)
	}
	go func() {
		err := fn(context.Background(), report)
		m.mu.Lock()
		delete(m.running, key)
		if err != nil {
			j.State, j.Error = StateFailed, err.Error()
		} else {
			j.State, j.Progress, j.Message = StateDone, 100, "完成"
		}
		m.mu.Unlock()
		m.broadcast(j)
	}()
	return id, nil
}

func (m *Manager) broadcast(j *Job) {
	m.hub.Broadcast(event.Event{Type: "job", InstanceID: j.InstanceID, Payload: *j})
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
