package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

const (
	graphite = "#101318"
	surface  = "#171C23"
	border   = "#37404D"
	text     = "#DCE1E9"
	muted    = "#8893A3"
	orange   = "#FF8A24"
	amber    = "#FFD08A"
	green    = "#83CDA5"
	red      = "#ED8B8B"
)

type textStyle struct {
	fg, bg string
	bold   bool
}

func (s textStyle) Render(value string) string { return paint(value, s.fg, s.bg, s.bold) }

var (
	titleStyle = textStyle{fg: orange, bold: true}
	dimStyle   = textStyle{fg: muted}
	okStyle    = textStyle{fg: green}
	errStyle   = textStyle{fg: red, bold: true}
)

func paint(value, fg, bg string, bold bool) string {
	if os.Getenv("NO_COLOR") != "" {
		return ansi.Strip(value)
	}
	s := lipgloss.NewStyle().Bold(bold)
	if fg != "" {
		s = s.Foreground(lipgloss.Color(fg))
	}
	if bg != "" {
		s = s.Background(lipgloss.Color(bg))
	}
	return s.Render(value)
}

func fitLine(value string, width int) string {
	width = max(1, width)
	value = ansi.Truncate(value, width, "…")
	return value + strings.Repeat(" ", max(0, width-lipgloss.Width(value)))
}

func choiceRow(value string, width int, selected bool) string {
	if selected {
		return paint(fitLine("▌ ◆  "+value, width), orange, "#2C241D", true)
	}
	return paint(fitLine("  ◇  "+value, width), text, "", false)
}

func wrapLines(lines []string, width int) []string {
	var result []string
	for _, line := range lines {
		result = append(result, strings.Split(ansi.Hardwrap(line, max(1, width), false), "\n")...)
	}
	return result
}

func scrollingLines(lines []string, width, height, offset int, tail bool) []string {
	lines = wrapLines(lines, width)
	height = max(1, height)
	if len(lines) <= height {
		return lines
	}
	count := max(1, height-1)
	start := min(max(0, offset), max(0, len(lines)-count))
	if tail {
		start = max(0, len(lines)-count-max(0, offset))
	}
	end := min(len(lines), start+count)
	shown := append([]string{}, lines[start:end]...)
	if height > 1 {
		shown = append(shown, dimStyle.Render(fmt.Sprintf("· %d–%d / %d · PgUp / PgDn", start+1, end, len(lines))))
	}
	return shown
}

// There is at most one presentation timer. It stops on idle screens;
// animation messages never invoke deployment commands or consume keys.
type visualTickMsg time.Time

func (m *model) queueVisualTick() tea.Cmd {
	active := m.revealFrame < 3 || m.st == stLoading || m.st == stRun ||
		(m.st == stDone && m.err == nil && m.doneFrame < 12)
	if !m.motion || m.splash || m.visualPending || !active {
		return nil
	}
	m.visualPending = true
	return tea.Tick(80*time.Millisecond, func(t time.Time) tea.Msg { return visualTickMsg(t) })
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if _, ok := msg.(visualTickMsg); ok {
		m.visualPending = false
		if !m.motion || m.splash {
			return m, nil
		}
		m.visualFrame++
		m.revealFrame++
		if m.st == stDone && m.err == nil {
			m.doneFrame++
		}
		return m, m.queueVisualTick()
	}
	previous, wasSplash := m.st, m.splash
	updated, cmd := m.updateCore(msg)
	if !m.splash && (previous != m.st || wasSplash) {
		m.revealFrame, m.visualFrame, m.doneFrame = 0, 0, 0
		return updated, tea.Batch(cmd, m.queueVisualTick())
	}
	return updated, cmd
}

func (m *model) spinner() string {
	frames := []string{"◌", "◔", "◑", "◕"}
	if !m.motion {
		return "◌"
	}
	return frames[m.visualFrame%len(frames)]
}

func (m *model) progressBar(done, total, width int) string {
	if total <= 0 {
		return m.lightTrack(width)
	}
	width = max(1, width)
	filled := width * min(max(done, 0), total) / total
	var b strings.Builder
	for i := 0; i < width; i++ {
		color, char := border, "─"
		if i < filled {
			color, char = orange, "━"
			if m.st == stDone && m.err == nil {
				color = green
			} else if m.motion && (i-m.visualFrame+width)%width < 3 {
				color = amber
			}
		}
		b.WriteString(paint(char, color, "", false))
	}
	return b.String() + dimStyle.Render(fmt.Sprintf("  %d / %d", done, total))
}

func (m *model) lightTrack(width int) string {
	width = max(1, width)
	head := m.visualFrame % (width + 6)
	var b strings.Builder
	for i := 0; i < width; i++ {
		color := border
		if m.motion && i <= head && i > head-6 {
			color = orange
		}
		b.WriteString(paint("─", color, "", false))
	}
	return b.String()
}

func hintLine(hint string) string {
	var parts []string
	for _, part := range strings.Split(hint, "    ") {
		key, label, ok := strings.Cut(part, "  ")
		if !ok {
			parts = append(parts, dimStyle.Render(part))
			continue
		}
		parts = append(parts, paint(" "+key+" ", text, "#252D38", false)+" "+dimStyle.Render(label))
	}
	return strings.Join(parts, "  ")
}

func (m *model) menuBody(width, height int) []string {
	leftWidth := width
	if width >= 86 {
		leftWidth = width - 31
	}
	labels := make([]string, len(menu))
	for i, it := range menu {
		labels[i] = it.Title
	}
	var lines []string
	if height >= 16 {
		for i, label := range labels {
			lines = append(lines, choiceRow(label, leftWidth, i == m.cursor))
			if i < len(labels)-1 {
				lines = append(lines, "")
			}
		}
	} else {
		lines = visibleChoices(labels, m.cursor, leftWidth, height)
	}
	if len(lines)+3 <= height {
		captions := []string{"Просмотр предстоящих операций", "Создание и запуск виртуальных машин", "Сохранение исходного состояния", "Возврат к сохранённому состоянию", "Удаление ресурсов выбранного стенда", "Состояние виртуальных машин", "Завершение работы"}
		lines = append(lines, "", dimStyle.Render(captions[min(max(0, m.cursor), len(captions)-1)]))
	}
	if width >= 86 {
		art := []string{
			"", "XX       XX", " XXX   XXX ", "   XXXXX   ", "    XXX    ",
			"   XXXXX   ", " XXX   XXX ", "XX       XX", "", "P R O X M O X", "R E G I O N  L A B",
		}
		for len(lines) < len(art) && len(lines) < height {
			lines = append(lines, "")
		}
		for i := range lines {
			ornament := ""
			if i < len(art) {
				ornament = art[i]
			}
			color := "#5A3D28"
			if i == 9 || i == 10 {
				color = muted
			}
			right := lipgloss.PlaceHorizontal(27, lipgloss.Center, paint(ornament, color, "", false))
			lines[i] = fitLine(lines[i], leftWidth) + "    " + right
		}
	}
	return lines
}
