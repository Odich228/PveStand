package main

import (
	"context"
	"errors"
	tea "github.com/charmbracelet/bubbletea"
	"os"
	"path/filepath"
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

func TestParticipantPickerScopesAllActions(t *testing.T) {
	cfg := &stand.Config{Name: "batch", Node: "n", Pool: "region", Participants: []stand.Participant{{ID: "A0"}, {ID: "A1", Node: "second"}}}
	m := &model{cfg: cfg, st: stMenu, height: 24}
	m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}})
	if m.st != stParticipantPick {
		t.Fatal(m.st)
	}
	m.handleKey(tea.KeyMsg{Type: tea.KeyDown})
	m.handleKey(tea.KeyMsg{Type: tea.KeyDown})
	m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if m.st != stMenu || cfg.ParticipantOnly != "A1" || len(cfg.Instances()) != 1 || cfg.Instances()[0].Node != "second" {
		t.Fatal(cfg, m.st)
	}
	if !strings.Contains(m.View(), "A1") {
		t.Fatal(m.View())
	}
	m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}})
	m.handleKey(tea.KeyMsg{Type: tea.KeyUp})
	m.handleKey(tea.KeyMsg{Type: tea.KeyUp})
	m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if cfg.ParticipantOnly != "" || len(cfg.Instances()) != 2 {
		t.Fatal("all selector not restored")
	}
}

func TestParticipantPickerReloadsOnlySelectedSecrets(t *testing.T) {
	t.Setenv("PICKER_A0_PASSWORD", "SecretForA0!")
	t.Setenv("PICKER_A1_PASSWORD", "SecretForA1!")
	path := filepath.Join(t.TempDir(), "stands.yaml")
	os.WriteFile(path, []byte("name: batch\nnode: n\npool: region\nvms:\n  - name: gw\n    template: 1001\nparticipants:\n  - id: A0\n    users:\n      - id: a0@pve\n        password_env: PICKER_A0_PASSWORD\n  - id: A1\n    users:\n      - id: a1@pve\n        password_env: PICKER_A1_PASSWORD\n"), 0600)
	cfg, err := stand.LoadForParticipant(path, "A0")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Participants[1].Users[0].Password != "" {
		t.Fatal("unselected secret read")
	}
	m := &model{cfg: cfg, cfgPath: path, st: stParticipantPick, participantCursor: 2}
	m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if m.st != stMenu || m.cfg.ParticipantOnly != "A1" || m.cfg.Instances()[0].Users[0].Password != "SecretForA1!" {
		t.Fatal(m.participantErr)
	}
}
