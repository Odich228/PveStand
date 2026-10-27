package main

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

func TestSplashSkipConsumesEnterAndIgnoresLateTick(t *testing.T) {
	m := &model{st: stMenu, splash: true, width: 80, height: 24, cursor: 1}
	if cmd := m.Init(); cmd == nil {
		t.Fatal("animation timer missing")
	}
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil || m.splash || m.st != stMenu || m.action != "" {
		t.Fatal("skip key triggered a deployment or changed destination")
	}
	_, cmd = m.Update(splashTickMsg(m.splashStart.Add(50 * time.Millisecond)))
	if cmd != nil || m.splash {
		t.Fatal("late tick restarted animation")
	}
}

func TestSplashFinishesAtDeadlineAndPreservesDestination(t *testing.T) {
	for _, destination := range []state{stFilePick, stConnect, stMenu} {
		m := &model{st: destination, splash: true}
		m.Init()
		_, cmd := m.Update(splashTickMsg(m.splashStart.Add(100 * time.Millisecond)))
		if cmd == nil || !m.splash {
			t.Fatal("animation stopped early")
		}
		_, cmd = m.Update(splashTickMsg(m.splashStart.Add(splashDuration)))
		if cmd != nil || m.splash || m.st != destination {
			t.Fatal("incorrect destination")
		}
	}
	m := &model{splash: false}
	if m.Init() != nil {
		t.Fatal("disabled splash scheduled animation")
	}
	m.splash = true
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd == nil {
		t.Fatal("Ctrl+C ignored")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("Ctrl+C did not quit")
	}
}

func TestSplashFramesFitTerminalAndContainNoSecrets(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	for _, size := range [][2]int{{100, 30}, {80, 24}, {61, 16}, {60, 16}, {40, 12}, {20, 8}, {8, 3}, {1, 1}, {0, 0}} {
		m := &model{splash: true, host: "private-host", token: "private-token"}
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		before := m.View()
		m.splashElapsed = 900 * time.Millisecond
		after := m.View()
		if lipgloss.Width(after) > max(1, size[0]) || lipgloss.Height(after) > max(1, size[1]) {
			t.Fatalf("overflow at %v: %dx%d", size, lipgloss.Width(after), lipgloss.Height(after))
		}
		if strings.Contains(after, "private-") || strings.Contains(after, "\x1b") {
			t.Fatal("secret or color escaped")
		}
		if size[0] >= 20 && before == after {
			t.Fatal("no visible monochrome animation")
		}
		if size[0] >= 40 && size[0] < splashLogoWidth && !strings.Contains(after, "PROXMOX") {
			t.Fatal("compact logo missing")
		}
	}
}
