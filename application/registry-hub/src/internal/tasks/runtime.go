package tasks

import (
	"slices"
	"time"
)

type RuntimeStatus struct {
	Concurrency             int       `json:"concurrency"`
	ActiveTasks             []string  `json:"activeTasks"`
	LastHeartbeatAt         time.Time `json:"lastHeartbeatAt"`
	LastProgressAt          time.Time `json:"lastProgressAt"`
	LastDispatchErrorAt     time.Time `json:"lastDispatchErrorAt"`
	LastDispatchSucceededAt time.Time `json:"lastDispatchSucceededAt"`
}

func (m *Manager) Runtime() RuntimeStatus {
	m.runtimeMu.Lock()
	defer m.runtimeMu.Unlock()
	status := m.runtime
	status.Concurrency = m.workers
	status.ActiveTasks = append([]string{}, m.runtime.ActiveTasks...)
	slices.Sort(status.ActiveTasks)
	return status
}

func (m *Manager) runtimeTask(id string, active bool) {
	m.runtimeMu.Lock()
	defer m.runtimeMu.Unlock()
	index := slices.Index(m.runtime.ActiveTasks, id)
	if active && index < 0 {
		m.runtime.ActiveTasks = append(m.runtime.ActiveTasks, id)
	}
	if !active && index >= 0 {
		m.runtime.ActiveTasks = slices.Delete(m.runtime.ActiveTasks, index, index+1)
	}
}

func (m *Manager) runtimeHeartbeat() {
	m.runtimeMu.Lock()
	defer m.runtimeMu.Unlock()
	m.runtime.LastHeartbeatAt = time.Now().UTC()
}

func (m *Manager) runtimeDispatch(err error) {
	m.runtimeMu.Lock()
	defer m.runtimeMu.Unlock()
	if err != nil {
		m.runtime.LastDispatchErrorAt = time.Now().UTC()
	} else {
		m.runtime.LastDispatchSucceededAt = time.Now().UTC()
	}
}

func (m *Manager) runtimeProgress(at time.Time) {
	m.runtimeMu.Lock()
	defer m.runtimeMu.Unlock()
	if at.After(m.runtime.LastProgressAt) {
		m.runtime.LastProgressAt = at
	}
}
