package main

import (
	"context"
	"errors"
	"fmt"
	"pvestand/internal/stand"
	"sort"
	"strings"
	"time"
)

// ---------- Отображение ----------

func bar(done, total, width int) string {
	if total == 0 {
		return ""
	}
	filled := width * done / total
	return fmt.Sprintf("[%s%s] %d/%d",
		strings.Repeat("█", filled), strings.Repeat("░", width-filled), done, total)
}

// lastLines возвращает последние n строк (n не меньше 3).
func lastLines(lines []string, n int) []string {
	if n < 3 {
		n = 3
	}
	if len(lines) <= n {
		return lines
	}
	return lines[len(lines)-n:]
}

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

func (m *model) View() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("PVE Stand Deployer"))
	if m.cfg != nil {
		b.WriteString("  " + dimStyle.Render(fmt.Sprintf("%s → узел %s, пул %s", m.cfg.Name, m.cfg.Node, m.cfg.Pool)))
	}
	b.WriteString("\n\n")

	switch m.st {
	case stConnect:
		b.WriteString("Подключение к Proxmox:\n\n")
		labels := []string{"Адрес (host)", "API-токен"}
		for i, l := range labels {
			cursor := "  "
			val := m.connInputs[i]
			if i == 1 && val != "" {
				val = strings.Repeat("•", len(val))
			}
			if i == m.connFocus {
				cursor = selStyle.Render("▸ ")
			}
			fmt.Fprintf(&b, "%s%s: %s\n", cursor, l, val)
		}
		tlsLabel := "проверять"
		if m.connInsecure {
			tlsLabel = "не проверять (insecure)"
		}
		fmt.Fprintf(&b, "\nTLS-сертификат: %s  (Ctrl+K — переключить)\n", tlsLabel)
		if m.connErr != "" {
			b.WriteString("\n" + errStyle.Render(m.connErr) + "\n")
		}
		b.WriteString("\n" + dimStyle.Render("Tab — след. поле · Enter — продолжить · Esc — выход"))

	case stFilePick:
		b.WriteString("Выбери файл стенда:\n\n")
		if len(m.fileList) == 0 {
			b.WriteString(dimStyle.Render("YAML-файлов не найдено в текущей папке.") + "\n")
		}
		for i, f := range m.fileList {
			if i == m.fileCursor {
				b.WriteString(selStyle.Render("▸ "+f) + "\n")
			} else {
				b.WriteString("  " + f + "\n")
			}
		}
		if m.fileErr != "" {
			b.WriteString("\n" + errStyle.Render(m.fileErr) + "\n")
		}
		b.WriteString("\n" + dimStyle.Render("↑/↓ — выбор · Enter — открыть · r — обновить список · q — выход"))

	case stMenu:
		for i, it := range menu {
			if i == m.cursor {
				b.WriteString(selStyle.Render("▸ "+it.Title) + "\n")
			} else {
				b.WriteString("  " + it.Title + "\n")
			}
		}
		if m.cfg.HasParticipants() {
			b.WriteString("\n" + dimStyle.Render(fmt.Sprintf("Участников: %d — действия применяются ко всем копиям стенда по очереди.", len(m.cfg.Instances()))) + "\n")
		}
		b.WriteString("\n" + dimStyle.Render("↑/↓ — выбор · Enter — подтвердить · q — выход"))

	case stPlan:
		b.WriteString("План развёртывания:\n\n")
		limit := m.height - 8
		shown := windowLines(m.plan, limit, m.logOffset)

		for i, s := range shown {
			fmt.Fprintf(&b, "%3d. %s\n", i+1, s)
		}
		if len(shown) < len(m.plan) {
			b.WriteString(dimStyle.Render(fmt.Sprintf("     … и ещё %d шагов\n", len(m.plan)-len(shown))))
		}
		if m.cfg.HasParticipants() {
			b.WriteString(dimStyle.Render(fmt.Sprintf("\n(план одинаков для каждого из %d участников — с суффиксом в пуле и именах ВМ)\n", len(m.cfg.Instances()))))
		}
		b.WriteString(dimStyle.Render("\nШаги, отмеченные «∥», при выполнении идут параллельно (до -parallel штук одновременно).\n"))
		b.WriteString("\n" + dimStyle.Render("Enter — назад · PgUp/PgDn — прокрутка"))

	case stLoading:
		b.WriteString(dimStyle.Render("Проверка… подождите."))

	case stConfirm:
		if m.confirm != nil {
			style := titleStyle
			if m.confirm.blocked {
				style = errStyle
			}
			b.WriteString(style.Render(m.confirm.title) + "\n\n")
			limit := m.height - 10
			lines := windowLines(m.confirm.lines, limit, m.logOffset)
			if len(lines) > limit && limit > 3 {
				lines = lines[:limit]
			}
			for _, l := range lines {
				b.WriteString(l + "\n")
			}
			b.WriteString("\n")
			if m.confirm.blocked {
				b.WriteString(dimStyle.Render("Esc — назад · PgUp/PgDn — прокрутка"))
			} else {
				b.WriteString("Продолжить: y / отмена: n")
			}
		}

	case stRun, stDone:
		b.WriteString(actionTitle(m.action) + "  " + bar(m.done, m.total, 30) + "\n\n")
		if !m.showLog {
			rows := m.vmRows()
			start := 0
			if m.rowCursor > m.height-13 {
				start = m.rowCursor - (m.height - 13)
			}
			for i, e := range rows {
				if i < start || i >= start+m.height-11 {
					continue
				}
				prefix := "  "
				if i == m.rowCursor {
					prefix = "▸ "
				}
				fmt.Fprintf(&b, "%s%-22s VMID %-6d %-8s %s (%s)\n", prefix, e.Resource, e.VMID, e.Stage, strings.Split(e.Msg, "\n")[0], e.Duration.Round(time.Second))
			}
		}
		logLines := []string{}
		if m.showLog {
			logLines = logWindow(m.log, m.height-12, m.logOffset)
		}
		for _, l := range logLines {
			switch {
			case strings.HasPrefix(l, "✓"):
				b.WriteString(okStyle.Render(l) + "\n")
			case strings.HasPrefix(l, "✗"):
				b.WriteString(errStyle.Render(l) + "\n")
			default:
				b.WriteString(dimStyle.Render(l) + "\n")
			}
		}
		if m.st == stRun {
			b.WriteString("\n" + dimStyle.Render("Esc — отменить ожидание · l — журнал/ВМ · f — только ошибки · PgUp/PgDn — журнал"))
		}
		if m.st == stDone {
			b.WriteString("\n")
			switch {
			case m.err != nil && errors.Is(m.err, context.Canceled):
				b.WriteString(errStyle.Render("Ожидание отменено. Задачи в Proxmox могут продолжать работу; повторный запуск проверит их состояние.") + "\n")
			case m.err != nil:
				b.WriteString(errStyle.Render("Ошибка: "+strings.Split(m.err.Error(), "\n")[0]+" (полный текст: l)") + "\n")
			default:
				b.WriteString(okStyle.Render("Готово.") + "\n")
			}
			if m.credTable != "" {
				b.WriteString("\n" + m.credTable + "\n")
			}
			b.WriteString(dimStyle.Render("Enter — в меню · ↑/↓ — ВМ · r — повторить выбранную ВМ · l — журнал · f — ошибки"))
		}

	case stStatus:
		b.WriteString("Статус ВМ:\n\n")
		if m.statusErr != "" {
			b.WriteString(errStyle.Render(m.statusErr) + "\n\n")
		}
		if len(m.statusRows) == 0 && m.statusErr == "" {
			b.WriteString(dimStyle.Render("ВМ не найдены.") + "\n")
		}
		for _, r := range m.statusRows {
			ip := strings.Join(r.IPs, ", ")
			if ip == "" {
				ip = "-"
			}
			statStyled := dimStyle.Render(fmt.Sprintf("%-10s", r.Status))
			if r.Status == "running" {
				statStyled = okStyle.Render(fmt.Sprintf("%-10s", r.Status))
			}
			fmt.Fprintf(&b, "%-24s VMID %-6d %s %s\n", r.Name, r.VMID, statStyled, ip)
		}
		b.WriteString("\n" + dimStyle.Render("Enter/Esc — в меню · r — обновить"))
	}
	return b.String() + "\n"
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
