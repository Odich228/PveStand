package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"pvestand/internal/pve"
	"pvestand/internal/stand"
)

// ---------- Стили ----------

var (
	titleStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))
	selStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212"))
	dimStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	okStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	errStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("196"))
)

// ---------- Состояния ----------

type state int

const (
	stConnect  state = iota // экран ввода подключения (если host/token не заданы)
	stFilePick              // выбор YAML-файла стенда из текущей папки
	stMenu
	stPlan
	stLoading // фоновая проверка/загрузка (preflight, превью удаления, статус)
	stConfirm // подтверждение действия (с готовым текстом/списком)
	stRun
	stDone
	stStatus
)

var menu = []struct{ Title, Action string }{
	{"Показать план развёртывания", "plan"},
	{"Развернуть стенд", "deploy"},
	{"Создать снапшот «чистое состояние»", "snapshot"},
	{"Сбросить стенд к снапшоту", "reset"},
	{"Удалить стенд", "destroy"},
	{"Статус ВМ", "status"},
	{"Выход", "quit"},
}

// ---------- Сообщения ----------

type eventMsg stand.Event

type preflightMsg struct {
	report *stand.PreflightReport
	err    error
}

type destroyPreviewMsg struct {
	targets  []pve.Member
	leftover int
	err      error
}

type statusRow struct {
	Name   string
	VMID   int
	Status string
	IPs    []string
}

type statusMsg struct {
	rows []statusRow
	err  error
}

// confirmData описывает то, что нужно показать на экране подтверждения, и
// какое действие запустить по «y»/Enter.
type confirmData struct {
	title   string
	lines   []string
	blocked bool // true — действие недоступно (только Esc назад)
	action  string
}

// ---------- Модель ----------

type model struct {
	cfgPath string
	host    string
	token   string
	insecure bool
	parallel int
	logFile  *os.File

	cfg *stand.Config
	cli *pve.Client

	st     state
	cursor int
	action string

	// экран подключения
	connInputs   [2]string
	connFocus    int
	connInsecure bool
	connErr      string

	// выбор файла
	fileList   []string
	fileCursor int
	fileErr    string

	plan []string
	log  []string
	done int
	total int
	err   error

	cancel    context.CancelFunc
	cancelled bool
	events    chan stand.Event
	height    int

	confirm *confirmData

	statusRows []statusRow
	statusErr  string

	credTable string
}

func newModel(cfgPath string, cfgPathSet bool, host, token string, insecure bool, parallel int, lf *os.File) *model {
	m := &model{
		cfgPath:      cfgPath,
		host:         host,
		token:        token,
		insecure:     insecure,
		parallel:     parallel,
		logFile:      lf,
		height:       24,
		connInputs:   [2]string{host, token},
		connInsecure: insecure,
	}
	if cfgPathSet {
		if cfg, err := stand.Load(cfgPath); err == nil {
			m.cfg = cfg
			if host != "" && token != "" {
				if cli, err := pve.New(host, token, insecure); err == nil {
					m.cli = cli
					m.st = stMenu
					return m
				}
			}
			m.st = stConnect
			return m
		}
		// Явно указанный файл не читается — всё равно даём выбрать другой.
	}
	m.fileList, m.fileErr = scanYAMLFiles()
	m.st = stFilePick
	return m
}

func (m *model) Init() tea.Cmd { return nil }

// ---------- Переходы между экранами ----------

func scanYAMLFiles() ([]string, string) {
	entries, err := os.ReadDir(".")
	if err != nil {
		return nil, err.Error()
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if strings.HasSuffix(n, ".yaml") || strings.HasSuffix(n, ".yml") {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	if len(out) == 0 {
		return nil, "YAML-файлов не найдено в текущей папке."
	}
	return out, ""
}

func (m *model) loadConfigAndProceed() (tea.Model, tea.Cmd) {
	cfg, err := stand.Load(m.cfgPath)
	if err != nil {
		m.fileErr = err.Error()
		m.st = stFilePick
		return m, nil
	}
	m.cfg = cfg
	m.fileErr = ""
	return m.afterConfigLoaded()
}

func (m *model) afterConfigLoaded() (tea.Model, tea.Cmd) {
	if m.host == "" || m.token == "" {
		m.st = stConnect
		return m, nil
	}
	return m.afterConnect()
}

func (m *model) afterConnect() (tea.Model, tea.Cmd) {
	cli, err := pve.New(m.host, m.token, m.insecure)
	if err != nil {
		m.connErr = err.Error()
		m.st = stConnect
		return m, nil
	}
	m.cli = cli
	m.connErr = ""
	m.st = stMenu
	return m, nil
}

// ---------- Фоновые команды ----------

func waitEvent(ch <-chan stand.Event) tea.Cmd {
	return func() tea.Msg {
		e, ok := <-ch
		if !ok {
			return nil
		}
		return eventMsg(e)
	}
}

func (m *model) cmdPreflight() tea.Cmd {
	cfg, cli := m.cfg, m.cli
	copies := len(cfg.Instances())
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return preflightMsg{report: stand.Preflight(ctx, cli, cfg, copies)}
	}
}

func (m *model) cmdDestroyPreview() tea.Cmd {
	cfg, cli := m.cfg, m.cli
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var all []pve.Member
		leftover := 0
		for _, inst := range cfg.Instances() {
			t, l, err := stand.DestroyPreview(ctx, cli, inst)
			if err != nil {
				return destroyPreviewMsg{err: err}
			}
			all = append(all, t...)
			leftover += l
		}
		return destroyPreviewMsg{targets: all, leftover: leftover}
	}
}

func (m *model) cmdStatus() tea.Cmd {
	cfg, cli := m.cfg, m.cli
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var rows []statusRow
		for _, inst := range cfg.Instances() {
			members, err := cli.PoolMembers(ctx, inst.Pool)
			if err != nil {
				return statusMsg{err: err}
			}
			for _, mm := range members {
				if mm.Type != "qemu" {
					continue
				}
				st, _ := cli.VMStatus(ctx, mm.Node, mm.VMID)
				row := statusRow{Name: mm.Name, VMID: mm.VMID, Status: st}
				if st == "running" {
					row.IPs, _ = cli.AgentIPs(ctx, mm.Node, mm.VMID)
				}
				rows = append(rows, row)
			}
		}
		return statusMsg{rows: rows}
	}
}

// start запускает выбранное действие в фоне; прогресс приходит сообщениями.
// Для стенда с несколькими участниками (Config.Instances() > 1) копии
// обрабатываются последовательно одна за другой, каждая — со своим планом.
func (m *model) start(action string) tea.Cmd {
	m.st = stRun
	m.action = action
	m.log, m.done, m.total, m.err = nil, 0, 0, nil
	m.cancelled = false

	ch := make(chan stand.Event, 128)
	m.events = ch
	cfg, cli, parallel, lf := m.cfg, m.cli, m.parallel, m.logFile

	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel

	go func() {
		defer close(ch)
		instances := cfg.Instances()
		emit := func(prefix string) func(stand.Event) {
			return func(e stand.Event) {
				if prefix != "" {
					e.Msg = prefix + e.Msg
				}
				if lf != nil {
					fmt.Fprintf(lf, "%s %s\n", time.Now().Format("15:04:05"), e.Msg)
				}
				ch <- e
			}
		}

		var runErr error
		for idx, inst := range instances {
			if ctx.Err() != nil {
				runErr = ctx.Err()
				break
			}
			prefix := ""
			if len(instances) > 1 {
				prefix = "[" + inst.Name + "] "
				ch <- stand.Event{Msg: fmt.Sprintf("═══ Участник %s (%d/%d) ═══", inst.Name, idx+1, len(instances))}
			}

			var (
				plan *stand.Plan
				err  error
			)
			switch action {
			case "deploy":
				plan = stand.DeploySteps(cli, inst)
			case "destroy":
				plan, err = stand.DestroyPlan(ctx, cli, inst)
			case "snapshot":
				plan, err = stand.SnapshotPlan(ctx, cli, inst)
			case "reset":
				plan, err = stand.ResetPlan(ctx, cli, inst)
			default:
				err = fmt.Errorf("неизвестное действие %q", action)
			}
			if err == nil && plan != nil {
				err = stand.Run(ctx, plan, parallel, emit(prefix))
			}
			if err != nil {
				runErr = err
				break
			}
		}
		ch <- stand.Event{Final: true, Err: runErr}
	}()
	return waitEvent(ch)
}

// ---------- Обновление ----------

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.height = msg.Height
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
			m.st = stDone
			m.credTable = ""
			if m.action == "deploy" && e.Err == nil && m.cfg.HasParticipants() {
				m.credTable = stand.CredentialsTable(m.cfg.Instances())
			}
			return m, nil
		}
		if e.Total > 0 || e.Done > 0 {
			m.done, m.total = e.Done, e.Total
		}
		m.log = append(m.log, e.Msg)
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
	for _, e := range msg.report.Errors {
		lines = append(lines, errStyle.Render("✗ "+e))
	}
	for _, w := range msg.report.Warnings {
		lines = append(lines, dimStyle.Render("⚠ "+w))
	}
	if len(lines) == 0 {
		lines = []string{okStyle.Render("Узел доступен, шаблоны и бриджи в порядке, места на хранилище достаточно.")}
	}
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
	if len(msg.targets) == 0 {
		m.confirm = &confirmData{
			title:   "Удалить стенд",
			lines:   []string{"Подходящих ВМ не найдено (по имени или тегу стенда) — удалять нечего."},
			blocked: true,
		}
		m.st = stConfirm
		return m, nil
	}
	lines := make([]string, 0, len(msg.targets)+1)
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
		return m, tea.Quit
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
				p := stand.DeploySteps(m.cli, m.cfg)
				m.plan = p.Describe()
				m.st = stPlan
			case "deploy":
				m.st = stLoading
				return m, m.cmdPreflight()
			case "snapshot":
				m.confirm = &confirmData{
					title:  "Создать снапшот?",
					lines:  []string{"Будет создан (или перезаписан) снапшот «" + stand.BaselineSnapshot + "» для всех ВМ стенда."},
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
		if key == "esc" && m.cancel != nil && !m.cancelled {
			m.cancelled = true
			m.cancel()
		}
		return m, nil

	case stDone:
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
		shown := m.plan
		if len(shown) > limit && limit > 3 {
			shown = shown[:limit]
		}
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
		b.WriteString("\n" + dimStyle.Render("Enter — назад"))

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
			lines := m.confirm.lines
			if len(lines) > limit && limit > 3 {
				lines = lines[:limit]
			}
			for _, l := range lines {
				b.WriteString(l + "\n")
			}
			b.WriteString("\n")
			if m.confirm.blocked {
				b.WriteString(dimStyle.Render("Esc — назад"))
			} else {
				b.WriteString("Продолжить: y / отмена: n")
			}
		}

	case stRun, stDone:
		b.WriteString(actionTitle(m.action) + "  " + bar(m.done, m.total, 30) + "\n\n")
		for _, l := range lastLines(m.log, m.height-9) {
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
			b.WriteString("\n" + dimStyle.Render("Esc — отменить"))
		}
		if m.st == stDone {
			b.WriteString("\n")
			switch {
			case m.err != nil && errors.Is(m.err, context.Canceled):
				b.WriteString(errStyle.Render("Отменено пользователем.") + "\n")
			case m.err != nil:
				b.WriteString(errStyle.Render("Ошибка: "+m.err.Error()) + "\n")
			default:
				b.WriteString(okStyle.Render("Готово.") + "\n")
			}
			if m.credTable != "" {
				b.WriteString("\n" + m.credTable + "\n")
			}
			b.WriteString(dimStyle.Render("Enter — в меню"))
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

// ---------- Точка входа ----------

func openLogFile(path string) (*os.File, error) {
	if path == "" {
		path = "pvestand-" + time.Now().Format("20060102-150405") + ".log"
	}
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
}

func main() {
	cfgPath := flag.String("c", "stand.yaml", "путь к YAML-описанию стенда")
	host := flag.String("host", os.Getenv("PVE_HOST"), "адрес Proxmox (или PVE_HOST)")
	token := flag.String("token", os.Getenv("PVE_TOKEN"), "API-токен user@realm!id=secret (или PVE_TOKEN)")
	insecure := flag.Bool("insecure", false, "не проверять TLS-сертификат (самоподписанный)")
	run := flag.String("run", "", "без TUI: plan | deploy | destroy | snapshot | reset | status")
	parallel := flag.Int("parallel", stand.DefaultParallelism, "сколько ВМ клонировать/удалять одновременно")
	logPath := flag.String("log", "", "писать журнал событий в файл (в TUI по умолчанию pvestand-<время>.log)")
	flag.Parse()

	cfgPathSet := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "c" {
			cfgPathSet = true
		}
	})

	if *run != "" {
		cfg, err := stand.Load(*cfgPath)
		if err != nil {
			fatal(err)
		}
		cli, err := pve.New(*host, *token, *insecure)
		if err != nil {
			fatal(err)
		}
		var lf *os.File
		if *logPath != "" {
			lf, err = openLogFile(*logPath)
			if err != nil {
				fmt.Fprintln(os.Stderr, "предупреждение: не удалось открыть файл журнала:", err)
				lf = nil
			} else {
				defer lf.Close()
			}
		}
		headless(*run, cfg, cli, *parallel, lf)
		return
	}

	lf, err := openLogFile(*logPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "предупреждение: не удалось открыть файл журнала:", err)
		lf = nil
	} else {
		defer lf.Close()
	}

	m := newModel(*cfgPath, cfgPathSet, *host, *token, *insecure, *parallel, lf)
	if _, err := tea.NewProgram(m, tea.WithAltScreen()).Run(); err != nil {
		fatal(err)
	}
}

func headless(action string, cfg *stand.Config, cli *pve.Client, parallel int, lf *os.File) {
	ctx := context.Background()
	logLine := func(s string) {
		fmt.Println(s)
		if lf != nil {
			fmt.Fprintf(lf, "%s %s\n", time.Now().Format("15:04:05"), s)
		}
	}

	instances := cfg.Instances()
	for idx, inst := range instances {
		if len(instances) > 1 {
			logLine(fmt.Sprintf("=== Участник %s (%d/%d) ===", inst.Name, idx+1, len(instances)))
		}

		if action == "plan" {
			p := stand.DeploySteps(cli, inst)
			for i, s := range p.Describe() {
				logLine(fmt.Sprintf("%3d. %s", i+1, s))
			}
			continue
		}
		if action == "status" {
			printStatus(ctx, cli, inst, logLine)
			continue
		}

		var (
			plan *stand.Plan
			err  error
		)
		switch action {
		case "deploy":
			plan = stand.DeploySteps(cli, inst)
		case "destroy":
			plan, err = stand.DestroyPlan(ctx, cli, inst)
		case "snapshot":
			plan, err = stand.SnapshotPlan(ctx, cli, inst)
		case "reset":
			plan, err = stand.ResetPlan(ctx, cli, inst)
		default:
			err = fmt.Errorf("неизвестное действие %q (нужно plan, deploy, destroy, snapshot, reset или status)", action)
		}
		if err == nil && plan != nil {
			err = stand.Run(ctx, plan, parallel, func(e stand.Event) {
				logLine(fmt.Sprintf("[%d/%d] %s", e.Done, e.Total, e.Msg))
			})
		}
		if err != nil {
			fatal(err)
		}
	}

	if action == "deploy" && len(instances) > 1 {
		fmt.Println()
		fmt.Println(stand.CredentialsTable(instances))
	}
}

func printStatus(ctx context.Context, cli *pve.Client, cfg *stand.Config, logLine func(string)) {
	members, err := cli.PoolMembers(ctx, cfg.Pool)
	if err != nil {
		logLine("Ошибка: " + err.Error())
		return
	}
	for _, mm := range members {
		if mm.Type != "qemu" {
			continue
		}
		st, _ := cli.VMStatus(ctx, mm.Node, mm.VMID)
		ip := "-"
		if st == "running" {
			if ips, _ := cli.AgentIPs(ctx, mm.Node, mm.VMID); len(ips) > 0 {
				ip = strings.Join(ips, ", ")
			}
		}
		logLine(fmt.Sprintf("%-24s VMID %-6d %-10s %s", mm.Name, mm.VMID, st, ip))
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "Ошибка:", err)
	os.Exit(1)
}
