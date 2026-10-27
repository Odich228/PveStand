package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"pvestand/internal/stand"
)

func TestThemedScreensFitAndKeepImportantContent(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	cfg := &stand.Config{Name: "region", Node: "SF-4", Pool: "region1", Participants: []stand.Participant{{ID: "A0"}}}
	for _, size := range [][2]int{{120, 40}, {100, 30}, {80, 24}, {60, 18}, {40, 14}, {25, 10}, {8, 3}, {1, 1}} {
		for _, state := range []state{stMenu, stFilePick, stConnect, stPlan, stLoading, stConfirm, stRun, stDone, stStatus, stParticipantPick} {
			m := &model{cfg: cfg, width: size[0], height: size[1], st: state, revealFrame: 3,
				fileList: []string{"stand.yaml"}, connInputs: [2]string{"https://node:8006", "NEVER-PRINT-THIS-TOKEN"},
				plan:    []string{strings.Repeat("Длинное сообщение ", 25)},
				confirm: &confirmData{title: "Проверка", lines: []string{"Доступ разрешён"}},
				action:  "deploy", rows: map[string]stand.Event{}, rowStarted: map[string]time.Time{},
				statusRows: []statusRow{{Name: "GW-CORE", Status: "running", VMID: 101}}}
			view := m.View()
			if lipgloss.Width(view) > size[0] || lipgloss.Height(view) > size[1] {
				t.Fatalf("screen %v at %v overflows: %dx%d", state, size, lipgloss.Width(view), lipgloss.Height(view))
			}
			if strings.Contains(view, "NEVER-PRINT") || strings.Contains(view, "\x1b") {
				t.Fatal("secret or color escaped in monochrome mode")
			}
		}
	}
}

func TestWrappedErrorsRemainScrollableInsideFrame(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	var messages []string
	for i := 0; i < 35; i++ {
		messages = append(messages, fmt.Sprintf("Проверка %02d: %s", i, strings.Repeat("длинный текст ", 8)))
	}
	m := &model{st: stConfirm, width: 60, height: 18, confirm: &confirmData{title: "Проверка", blocked: true, lines: messages}}
	first := m.View()
	if !strings.Contains(first, "Проверка 00") || !strings.Contains(first, "Esc") {
		t.Fatal(first)
	}
	for i := 0; i < 30; i++ {
		m.handleKey(tea.KeyMsg{Type: tea.KeyPgDown})
	}
	if last := m.View(); !strings.Contains(last, "Проверка 34") {
		t.Fatal(last)
	}
	if scrollingLines([]string{"one", "two", "three"}, 10, 2, 999, true)[0] != "one" {
		t.Fatal("tail overscroll")
	}
}

func TestVisualTimerStopsAndNeverStartsAnOperation(t *testing.T) {
	m := &model{st: stMenu, motion: true, cursor: 1, cfg: &stand.Config{}}
	if m.Init() == nil {
		t.Fatal("missing entrance animation")
	}
	if m.queueVisualTick() != nil {
		t.Fatal("duplicate timer scheduled")
	}
	var cmd tea.Cmd
	for i := 0; i < 3; i++ {
		_, cmd = m.Update(visualTickMsg(time.Now()))
	}
	if cmd != nil || m.visualPending || m.action != "" || m.st != stMenu {
		t.Fatal("idle loop or action triggered")
	}
	m.st = stRun
	if m.queueVisualTick() == nil {
		t.Fatal("missing running animation")
	}
	m.Update(eventMsg{Final: true})
	if m.st != stDone {
		t.Fatal("final event delayed")
	}
	for i := 0; i < 12; i++ {
		_, cmd = m.Update(visualTickMsg(time.Now()))
	}
	if cmd != nil || m.visualPending {
		t.Fatal("completion animation never stopped")
	}
	m.motion = false
	m.st = stLoading
	if m.queueVisualTick() != nil {
		t.Fatal("no-animation ignored")
	}

	m = &model{st: stRun, motion: true, rows: map[string]stand.Event{}, events: make(chan stand.Event)}
	m.Update(eventMsg{Final: true, Err: errors.New("QEMU failed")})
	if m.st != stDone || m.err == nil {
		t.Fatal("lost failure")
	}
	if strings.Contains(m.View(), "✓ ГОТОВО") {
		t.Fatal("failure presented as success")
	}
}
