package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"pvestand/internal/stand"
	"sort"
	"strings"
	"time"
)

func actionTitle(action string) string {
	switch action {
	case "deploy":
		return "Развёртывание"
	case "destroy":
		return "Удаление"
	case "snapshot":
		return "Снапшот"
	case "reset":
		return "Сброс к снапшоту"
	default:
		return action
	}
}

func (m *model) screenTitle() string {
	switch m.st {
	case stConnect:
		return "ПОДКЛЮЧЕНИЕ"
	case stFilePick:
		return "ВЫБОР КОНФИГУРАЦИИ"
	case stParticipantPick:
		return "ВЫБОР СТЕНДА"
	case stMenu:
		return "ГЛАВНОЕ МЕНЮ"
	case stPlan:
		return "ПЛАН РАЗВЁРТЫВАНИЯ"
	case stLoading:
		return "ПРОВЕРКА"
	case stConfirm:
		if m.confirm != nil {
			return m.confirm.title
		}
	case stRun:
		return strings.ToUpper(actionTitle(m.action))
	case stDone:
		if m.err != nil {
			return "ОПЕРАЦИЯ ПРЕРВАНА"
		}
		return "ОПЕРАЦИЯ ЗАВЕРШЕНА"
	case stStatus:
		return "СТАТУС ВИРТУАЛЬНЫХ МАШИН"
	}
	return "PVE STAND"
}

func (m *model) contextLine() string {
	if m.cfg == nil {
		return "REGION LAB  /  PVE Stand Deployer"
	}
	if m.cfg.HasParticipants() {
		selected := m.cfg.ParticipantOnly
		if selected == "" {
			selected = fmt.Sprintf("все стенды · %d", len(m.cfg.Instances()))
		}
		return m.cfg.Name + "  /  " + selected
	}
	return fmt.Sprintf("%s  /  %s  /  пул %s", m.cfg.Name, m.cfg.Node, m.cfg.Pool)
}

func (m *model) keyHints() []string {
	switch m.st {
	case stConnect:
		return []string{"Tab  поле    Enter  подключить", "Ctrl+K  TLS    Esc  выход"}
	case stFilePick:
		return []string{"↑↓  выбрать    Enter  открыть", "r  обновить    q  выход"}
	case stParticipantPick:
		return []string{"↑↓  выбрать    Enter  применить", "Esc  назад"}
	case stMenu:
		second := "q  выход"
		if m.cfg != nil && m.cfg.HasParticipants() {
			second = "p  стенды    q  выход"
		}
		return []string{"↑↓  выбрать    Enter  открыть", second}
	case stConfirm:
		if m.confirm == nil || m.confirm.blocked {
			return []string{"Esc  назад", "PgUp / PgDn  прокрутка"}
		}
		return []string{"y / Enter  подтвердить    n / Esc  отменить", "PgUp / PgDn  прокрутка"}
	case stPlan:
		return []string{"Enter / Esc  назад", "PgUp / PgDn  прокрутка"}
	case stLoading:
		return []string{"◌  Подождите завершения проверки", "Ctrl+C  выход"}
	case stRun:
		return []string{"l  журнал / ВМ    f  ошибки    ↑↓  выбрать", "Esc  отменить ожидание    PgUp / PgDn  журнал"}
	case stDone:
		second := "f  ошибки    PgUp / PgDn  журнал"
		if m.action == "deploy" {
			second = "r  повторить ВМ    " + second
		}
		return []string{"Enter / Esc  меню    l  журнал    ↑↓  выбрать", second}
	case stStatus:
		return []string{"Enter / Esc  меню    r  обновить", "PgUp / PgDn  прокрутка"}
	}
	return []string{"", ""}
}

func (m *model) View() string {
	if m.splash {
		return m.splashView()
	}
	w, h := m.width, m.height
	if w <= 0 {
		w = 80
	}
	if h <= 0 {
		h = 24
	}
	if w < 36 || h < 14 {
		return m.compactView(w, h)
	}
	fw, fh := min(104, w-4), min(30, h-2)
	bw, bh := fw-6, fh-11
	body := m.screenBody(bw, bh)
	// Reveal content over three short frames. Input and background work are
	// handled immediately, independently of this presentation effect.
	if m.motion && m.revealFrame < 3 {
		body = body[:min(len(body), max(0, bh*m.revealFrame/3))]
	}
	for len(body) < bh {
		body = append(body, "")
	}
	var rows []string
	rows = append(rows, m.frameRule(fw, "╭", "╮", " ◆ PVE STAND ", 0))
	context := dimStyle.Render(m.contextLine())
	version := "v" + Version
	if bw >= 72 && lipgloss.Width(context)+len(version)+3 <= bw {
		context = fitLine(context, bw-len(version)) + dimStyle.Render(version)
	}
	rows = append(rows, m.frameRow(context, fw, 1))
	rows = append(rows, m.frameRule(fw, "├", "┤", "", 2))
	rows = append(rows, m.frameRow("", fw, 3))
	title := titleStyle.Render(m.screenTitle())
	if m.st == stDone && m.err == nil {
		title = okStyle.Render("✓ " + m.screenTitle())
	}
	if (m.st == stDone && m.err != nil) || (m.st == stConfirm && m.confirm != nil && m.confirm.blocked) {
		title = errStyle.Render(m.screenTitle())
	}
	rows = append(rows, m.frameRow(title, fw, 4), m.frameRow("", fw, 5))
	for i, row := range body[:bh] {
		rows = append(rows, m.frameRow(row, fw, 6+i))
	}
	rows = append(rows, m.frameRow("", fw, 6+bh), m.frameRule(fw, "├", "┤", "", 7+bh))
	for i, hint := range m.keyHints() {
		rows = append(rows, m.frameRow(hintLine(hint), fw, 8+bh+i))
	}
	rows = append(rows, m.frameRule(fw, "╰", "╯", " P V E  S T A N D  /  R E G I O N  L A B ", fh-1))
	card := paint(strings.Join(rows, "\n"), text, graphite, false)
	canvas := lipgloss.Place(w, h, lipgloss.Center, lipgloss.Center, card)
	return paint(canvas, text, graphite, false)
}

func (m *model) frameInk(value string, col, row, width int) string {
	color := border
	if m.motion && m.st == stDone && m.err == nil && m.doneFrame < 12 {
		head := m.doneFrame * (width + 10) / 12
		if col+row/2 >= head-7 && col+row/2 <= head {
			color = green
		}
	}
	return paint(value, color, "", false)
}

func (m *model) frameRule(width int, left, right, label string, row int) string {
	label = ansi.Truncate(label, width-4, "…")
	var b strings.Builder
	b.WriteString(m.frameInk(left, 0, row, width))
	if label != "" {
		b.WriteString(m.frameInk("─", 1, row, width))
		color := muted
		if row == 0 {
			color = orange
		}
		b.WriteString(paint(label, color, "", row == 0))
	}
	for col := lipgloss.Width(b.String()); col < width-1; col++ {
		b.WriteString(m.frameInk("─", col, row, width))
	}
	b.WriteString(m.frameInk(right, width-1, row, width))
	return b.String()
}

func (m *model) frameRow(value string, width, row int) string {
	return m.frameInk("│", 0, row, width) + "  " + fitLine(value, width-6) + "  " + m.frameInk("│", width-1, row, width)
}

func (m *model) compactView(width, height int) string {
	rows := []string{titleStyle.Render("◆ PVE STAND"), titleStyle.Render(m.screenTitle())}
	rows = append(rows, m.screenBody(width, max(1, height-4))...)
	for len(rows) < height-2 {
		rows = append(rows, "")
	}
	rows = append(rows, dimStyle.Render(m.keyHints()[0]), dimStyle.Render(m.keyHints()[1]))
	rows = rows[:min(len(rows), height)]
	for i := range rows {
		rows[i] = fitLine(rows[i], width)
	}
	return paint(strings.Join(rows, "\n"), text, graphite, false)
}

func visibleChoices(labels []string, cursor, width, height int) []string {
	height = max(1, height)
	start := max(0, cursor-height+1)
	var rows []string
	for i := start; i < len(labels) && len(rows) < height; i++ {
		rows = append(rows, choiceRow(labels[i], width, i == cursor))
	}
	return rows
}

func (m *model) screenBody(width, height int) []string {
	height = max(1, height)
	var lines []string
	switch m.st {
	case stMenu:
		return m.menuBody(width, height)
	case stFilePick:
		count := height
		if m.fileErr != "" {
			count = max(1, height-3)
		}
		lines = visibleChoices(m.fileList, m.fileCursor, width, count)
		if m.fileErr != "" {
			lines = append(lines, "", errStyle.Render(m.fileErr))
		}
		if len(lines) == 0 {
			lines = []string{dimStyle.Render("YAML-файлов не найдено в текущей папке.")}
		}
	case stParticipantPick:
		labels := []string{"Все стенды"}
		if m.cfg != nil {
			for _, p := range m.cfg.Participants {
				node := p.Node
				if node == "" {
					node = m.cfg.Node
				}
				labels = append(labels, fmt.Sprintf("%s  /  %s  /  %s-%s", p.ID, node, m.cfg.Pool, p.ID))
			}
		}
		count := height
		if m.participantErr != "" {
			count = max(1, height-3)
		}
		lines = visibleChoices(labels, m.participantCursor, width, count)
		if m.participantErr != "" {
			lines = append(lines, "", errStyle.Render(m.participantErr))
		}
	case stConnect:
		values := m.connInputs
		if values[1] != "" {
			values[1] = strings.Repeat("•", len([]rune(values[1])))
		}
		for i, label := range []string{"Адрес", "API-токен"} {
			value := values[i]
			if value == "" {
				value = "…"
			}
			lines = append(lines, choiceRow(label+"  "+value, width, m.connFocus == i), "")
		}
		tls := "TLS · проверка сертификата включена"
		if m.connInsecure {
			tls = "TLS · проверка сертификата отключена"
		}
		lines = append(lines, dimStyle.Render(tls))
		if m.connErr != "" {
			lines = append(lines, "", errStyle.Render(m.connErr))
		}
	case stLoading:
		lines = []string{"", titleStyle.Render(m.spinner() + "  Проверка… подождите."), "", m.lightTrack(min(32, width)), "", dimStyle.Render("Подготовка данных для выбранного действия")}
	case stPlan:
		for i, line := range m.plan {
			lines = append(lines, fmt.Sprintf("%02d  %s", i+1, line))
		}
		lines = append(lines, "", dimStyle.Render("∥ — параллельные шаги в пределах -parallel"))
		return scrollingLines(lines, width, height, m.logOffset, false)
	case stConfirm:
		if m.confirm != nil {
			lines = append(lines, m.confirm.lines...)
			if !m.confirm.blocked {
				lines = append(lines, "", titleStyle.Render("◆ Подтвердите выполнение действия"))
			}
		}
		return scrollingLines(lines, width, height, m.logOffset, false)
	case stRun, stDone:
		return m.executionBody(width, height)
	case stStatus:
		if m.statusErr != "" {
			lines = append(lines, errStyle.Render(m.statusErr), "")
		}
		if len(m.statusRows) == 0 && m.statusErr == "" {
			lines = append(lines, dimStyle.Render("ВМ не найдены."))
		}
		for _, r := range m.statusRows {
			ip := strings.Join(r.IPs, ", ")
			if ip == "" {
				ip = "—"
			}
			status := dimStyle.Render("○ " + r.Status)
			if r.Status == "running" {
				status = okStyle.Render("● " + r.Status)
			}
			lines = append(lines, fmt.Sprintf("%-20s %5d  %s  %s", r.Name, r.VMID, status, ip))
		}
		return scrollingLines(lines, width, height, m.logOffset, false)
	}
	return scrollingLines(lines, width, height, 0, false)
}

func (m *model) executionBody(width, height int) []string {
	done := m.done
	if m.st == stDone && m.err == nil && m.total > 0 {
		done = m.total
	}
	lines := []string{m.progressBar(done, m.total, max(1, min(42, width-16))), ""}
	if m.st == stDone {
		switch {
		case errors.Is(m.err, context.Canceled):
			lines = append(lines, errStyle.Render("◇ Ожидание отменено. Задачи Proxmox могут продолжать работу."))
		case m.err != nil:
			lines = append(lines, errStyle.Render("✗ "+strings.Split(m.err.Error(), "\n")[0]+" · полный текст: l"))
		default:
			lines = append(lines, okStyle.Render("✓ ГОТОВО · "+actionTitle(m.action)))
		}
		if m.credTable != "" {
			lines = append(lines, dimStyle.Render(m.credTable))
		}
		lines = wrapLines(lines, width)
		// Keep enough room for log detail even for a long top-level error.
		if len(lines) > max(1, height/2) {
			lines = lines[:max(1, height/2)]
		}
		lines = append(lines, "")
	}
	remaining := max(1, height-len(lines))
	if m.showLog {
		var logs []string
		for _, line := range m.log {
			switch {
			case strings.HasPrefix(line, "✗"):
				logs = append(logs, errStyle.Render(line))
			case strings.HasPrefix(line, "✓"):
				logs = append(logs, okStyle.Render(line))
			default:
				logs = append(logs, dimStyle.Render(line))
			}
		}
		lines = append(lines, scrollingLines(logs, width, remaining, m.logOffset, true)...)
	} else {
		rows := m.vmRows()
		labels := make([]string, len(rows))
		for i, e := range rows {
			icon := "◇"
			switch e.Stage {
			case "running":
				icon = m.spinner()
			case "failed":
				icon = "✗"
			case "done":
				icon = "●"
			}
			labels[i] = fmt.Sprintf("%s %-18s %5d  %-8s %s", icon, e.Resource, e.VMID, e.Stage, strings.Split(e.Msg, "\n")[0])
		}
		if len(labels) == 0 {
			labels = []string{m.spinner() + " Ожидание событий"}
		}
		lines = append(lines, visibleChoices(labels, m.rowCursor, width, remaining)...)
	}
	return lines[:min(len(lines), height)]
}

func (m *model) vmRows() []stand.Event {
	var rows []stand.Event
	for _, e := range m.rows {
		if !m.failedOnly || e.Stage == "failed" {
			if e.Stage == "running" {
				e.Duration = time.Since(m.rowStarted[e.Resource])
			}
			rows = append(rows, e)
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Resource < rows[j].Resource })
	return rows
}
func windowLines(lines []string, limit, offset int) []string {
	if limit < 1 {
		limit = 1
	}
	if offset < 0 {
		offset = 0
	}
	if offset > len(lines) {
		offset = len(lines)
	}
	start := offset
	if start+limit > len(lines) {
		start = len(lines) - limit
	}
	if start < 0 {
		start = 0
	}
	end := start + limit
	if end > len(lines) {
		end = len(lines)
	}
	return lines[start:end]
}

func logWindow(lines []string, limit, offset int) []string {
	if limit < 1 {
		limit = 1
	}
	end := len(lines) - offset
	if end < 0 {
		end = 0
	}
	start := end - limit
	if start < 0 {
		start = 0
	}
	return lines[start:end]
}
