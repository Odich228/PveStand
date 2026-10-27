package main

import (
	"fmt"
	tea "github.com/charmbracelet/bubbletea"
	"pvestand/internal/stand"
	"strings"
	"time"
)

// ---------- Обновление ----------

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.height = msg.Height
		return m, nil

	case tickMsg:
		if m.st == stRun {
			return m, tickCmd()
		}
		return m, nil
	case planMsg:
		m.plan = msg.lines
		if msg.err != nil {
			m.plan = []string{msg.err.Error()}
		}
		m.logOffset = 0
		m.st = stPlan
		return m, nil
	case preflightMsg:
		return m.handlePreflight(msg)

	case destroyPreviewMsg:
		return m.handleDestroyPreview(msg)

	case statusMsg:
		m.statusRows = msg.rows
		m.statusErr = ""
		if msg.err != nil {
			m.statusErr = msg.err.Error()
		}
		m.st = stStatus
		return m, nil

	case eventMsg:
		e := stand.Event(msg)
		if e.Final {
			m.err = e.Err
			if e.Err != nil {
				m.log = append(m.log, strings.Split(e.Err.Error(), "\n")...)
				if len(m.rows) == 0 {
					m.showLog = true
				}
			}
			m.st = stDone
			m.credTable = ""
			if m.action == "deploy" && e.Err == nil && m.cfg.HasParticipants() {
				m.credTable = "Учётные данные доступны через CLI: -run deploy -credentials файл"
			}
			return m, nil
		}
		if e.Total > 0 || e.Done > 0 {
			m.done, m.total = e.Done, e.Total
		}
		if e.Resource != "" {
			if m.rows == nil {
				m.rows = map[string]stand.Event{}
			}
			if e.Stage == "running" {
				m.rowStarted[e.Resource] = time.Now()
			}
			m.rows[e.Resource] = e
		}
		m.log = append(m.log, strings.Split(e.Msg, "\n")...)
		return m, waitEvent(m.events)

	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m *model) handlePreflight(msg preflightMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.confirm = &confirmData{title: "Ошибка проверки", lines: []string{msg.err.Error()}, blocked: true}
		m.st = stConfirm
		return m, nil
	}
	var lines []string
	for _, info := range msg.report.Info {
		lines = append(lines, info)
	}
	for _, e := range msg.report.Errors {
		lines = append(lines, errStyle.Render("✗ "+e))
	}
	for _, w := range msg.report.Warnings {
		lines = append(lines, dimStyle.Render("⚠ "+w))
	}
	if len(lines) == 0 {
		lines = []string{okStyle.Render("Узел доступен, шаблоны и бриджи в порядке, места на хранилище достаточно.")}
	}
	m.logOffset = 0
	m.confirm = &confirmData{
		title:   "Проверка перед развёртыванием",
		lines:   lines,
		blocked: len(msg.report.Errors) > 0,
		action:  "deploy",
	}
	m.st = stConfirm
	return m, nil
}

func (m *model) handleDestroyPreview(msg destroyPreviewMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.confirm = &confirmData{title: "Ошибка", lines: []string{msg.err.Error()}, blocked: true}
		m.st = stConfirm
		return m, nil
	}
	if len(msg.targets) == 0 && len(msg.resources) == 0 {
		m.confirm = &confirmData{
			title:   "Удалить стенд",
			lines:   []string{"В журнале нет принадлежащих стенду ресурсов для удаления."},
			blocked: true,
		}
		m.st = stConfirm
		return m, nil
	}
	lines := append([]string{}, msg.resources...)
	for _, t := range msg.targets {
		lines = append(lines, fmt.Sprintf("%s  (VMID %d, узел %s)", t.Name, t.VMID, t.Node))
	}
	if msg.leftover > 0 {
		lines = append(lines, dimStyle.Render(fmt.Sprintf("⚠ в пуле(ах) останется %d посторонних объектов — пул(ы) удалены не будут", msg.leftover)))
	}
	m.confirm = &confirmData{
		title:  fmt.Sprintf("Удалить %d ВМ?", len(msg.targets)),
		lines:  lines,
		action: "destroy",
	}
	m.st = stConfirm
	return m, nil
}

func (m *model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	if key == "ctrl+c" {
		if m.st == stRun && m.cancel != nil {
			m.cancelled = true
			m.cancel()
			return m, nil
		}
		return m, tea.Quit
	}
	if key == "pgup" || key == "pgdown" {
		direction := 10
		if key == "pgup" {
			direction = -10
		}
		if (m.st == stRun || m.st == stDone) && m.showLog {
			direction = -direction
		}
		m.logOffset += direction
		if m.logOffset < 0 {
			m.logOffset = 0
		}
		return m, nil
	}

	switch m.st {
	case stConnect:
		switch msg.Type {
		case tea.KeyEnter:
			if strings.TrimSpace(m.connInputs[0]) == "" || strings.TrimSpace(m.connInputs[1]) == "" {
				m.connErr = "укажите адрес узла и API-токен"
				return m, nil
			}
			m.host = strings.TrimSpace(m.connInputs[0])
			m.token = strings.TrimSpace(m.connInputs[1])
			m.insecure = m.connInsecure
			return m.afterConnect()
		case tea.KeyTab, tea.KeyDown, tea.KeyUp:
			m.connFocus = (m.connFocus + 1) % 2
		case tea.KeyBackspace:
			s := m.connInputs[m.connFocus]
			if len(s) > 0 {
				m.connInputs[m.connFocus] = s[:len(s)-1]
			}
		case tea.KeyEsc:
			return m, tea.Quit
		case tea.KeyRunes:
			m.connInputs[m.connFocus] += string(msg.Runes)
		default:
			if key == "ctrl+k" {
				m.connInsecure = !m.connInsecure
			}
		}
		return m, nil

	case stFilePick:
		switch key {
		case "up", "k":
			if m.fileCursor > 0 {
				m.fileCursor--
			}
		case "down", "j":
			if m.fileCursor < len(m.fileList)-1 {
				m.fileCursor++
			}
		case "enter":
			if len(m.fileList) > 0 {
				m.cfgPath = m.fileList[m.fileCursor]
				return m.loadConfigAndProceed()
			}
		case "r":
			m.fileList, m.fileErr = scanYAMLFiles()
			m.fileCursor = 0
		case "q":
			return m, tea.Quit
		}
		return m, nil

	case stMenu:
		switch key {
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < len(menu)-1 {
				m.cursor++
			}
		case "q":
			return m, tea.Quit
		case "enter":
			switch menu[m.cursor].Action {
			case "plan":
				m.st = stLoading
				return m, m.cmdPlan()
			case "deploy":
				m.st = stLoading
				return m, m.cmdPreflight()
			case "snapshot":
				m.confirm = &confirmData{
					title:  "Создать снапшот?",
					lines:  []string{"Будет создан отсутствующий снапшот (существующий сохранится) «" + stand.BaselineSnapshot + "» для всех ВМ стенда."},
					action: "snapshot",
				}
				m.st = stConfirm
			case "reset":
				m.confirm = &confirmData{
					title:  "Сбросить стенд?",
					lines:  []string{"Все ВМ стенда будут возвращены к снапшоту «" + stand.BaselineSnapshot + "». Текущие изменения в них будут потеряны."},
					action: "reset",
				}
				m.st = stConfirm
			case "destroy":
				m.st = stLoading
				return m, m.cmdDestroyPreview()
			case "status":
				m.st = stLoading
				return m, m.cmdStatus()
			case "quit":
				return m, tea.Quit
			}
		}
		return m, nil

	case stPlan:
		if key == "enter" || key == "esc" || key == "q" {
			m.st = stMenu
		}
		return m, nil

	case stConfirm:
		switch key {
		case "y", "Y", "enter":
			if m.confirm != nil && !m.confirm.blocked {
				action := m.confirm.action
				m.confirm = nil
				return m, m.start(action)
			}
		case "n", "N", "esc", "q":
			m.confirm = nil
			m.st = stMenu
		}
		return m, nil

	case stRun:
		if key == "l" {
			m.showLog = !m.showLog
			m.logOffset = 0
		}
		if key == "f" {
			m.failedOnly = !m.failedOnly
		}
		if key == "up" && m.rowCursor > 0 {
			m.rowCursor--
		}
		if key == "down" && m.rowCursor < len(m.vmRows())-1 {
			m.rowCursor++
		}
		if key == "esc" && m.cancel != nil && !m.cancelled {
			m.cancelled = true
			m.cancel()
		}
		return m, nil

	case stDone:
		if key == "l" {
			m.showLog = !m.showLog
			m.logOffset = 0
		}
		if key == "f" {
			m.failedOnly = !m.failedOnly
			m.rowCursor = 0
		}
		if key == "up" && m.rowCursor > 0 {
			m.rowCursor--
		}
		if key == "down" && m.rowCursor < len(m.vmRows())-1 {
			m.rowCursor++
		}
		if key == "r" && m.action == "deploy" {
			rows := m.vmRows()
			if len(rows) > 0 {
				if m.rowCursor >= len(rows) {
					m.rowCursor = len(rows) - 1
				}
				m.only = rows[m.rowCursor].Resource
				return m, m.start("deploy")
			}
		}
		if key == "enter" || key == "esc" || key == "q" {
			m.st = stMenu
		}
		return m, nil

	case stStatus:
		switch key {
		case "enter", "esc", "q":
			m.st = stMenu
		case "r":
			m.st = stLoading
			return m, m.cmdStatus()
		}
		return m, nil
	}
	return m, nil
}
