package main

import (
	"context"
	"errors"
	tea "github.com/charmbracelet/bubbletea"
	"pvestand/internal/stand"
	"strings"
	"testing"
	"time"
)

func TestUICancellationDoesNotQuitBeforeSavingState(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	m := &model{st: stRun, cancel: cancel}
	_, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd != nil || !m.cancelled || !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatal("quit before cancellation completed")
	}
}
func TestUIErrorLogAndFailedFilter(t *testing.T) {
	m := &model{st: stRun, height: 24, rows: map[string]stand.Event{}, rowStarted: map[string]time.Time{}, events: make(chan stand.Event)}
	m.Update(eventMsg{Resource: "gw", Stage: "failed", VMID: 200, Msg: "QEMU failed\nCPU unsupported"})
	if len(m.log) != 2 || m.rows["gw"].VMID != 200 {
		t.Fatal(m.log, m.rows)
	}
	m.rows["app"] = stand.Event{Resource: "app", Stage: "done"}
	m.failedOnly = true
	if rows := m.vmRows(); len(rows) != 1 || rows[0].Resource != "gw" {
		t.Fatal(rows)
	}
	m.Update(eventMsg{Final: true, Err: errors.New("QEMU failed\nCPU unsupported")})
	m.showLog = true
	if !strings.Contains(m.View(), "CPU unsupported") {
		t.Fatal(m.View())
	}
}
func TestLogWindowAndPlanScrolling(t *testing.T) {
	lines := []string{"a", "b", "c", "d"}
	if s := strings.Join(logWindow(lines, 2, 0), ""); s != "cd" {
		t.Fatal(s)
	}
	if s := strings.Join(logWindow(lines, 2, 2), ""); s != "ab" {
		t.Fatal(s)
	}
	if s := strings.Join(windowLines(lines, 2, 0), ""); s != "ab" {
		t.Fatal(s)
	}
}
