package main

import (
	"context"

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

type tickMsg time.Time

func tickCmd() tea.Cmd { return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) }) }

type eventMsg stand.Event
type planMsg struct {
	lines []string
	err   error
}

type preflightMsg struct {
	report *stand.PreflightReport
	err    error
}

type destroyPreviewMsg struct {
	targets   []pve.Member
	leftover  int
	resources []string
	err       error
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
	cfgPath  string
	host     string
	token    string
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

	plan  []string
	log   []string
	done  int
	total int
	err   error

	cancel    context.CancelFunc
	cancelled bool
	events    chan stand.Event
	height    int

	confirm *confirmData

	statusRows []statusRow
	statusErr  string

	credTable  string
	rows       map[string]stand.Event
	rowStarted map[string]time.Time
	rowCursor  int
	logOffset  int
	showLog    bool
	failedOnly bool
	only       string
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
					cli.SetSecrets(cfg.Secrets())
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
	cli.SetSecrets(m.cfg.Secrets())
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
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		return preflightMsg{report: stand.Preflight(ctx, cli, cfg, copies)}
	}
}

func (m *model) cmdDestroyPreview() tea.Cmd {
	cfg, cli := m.cfg, m.cli
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := stand.Bind(cli, cfg); err != nil {
			return destroyPreviewMsg{err: err}
		}
		var all []pve.Member
		var resources []string
		leftover := 0
		for _, inst := range cfg.Instances() {
			t, l, err := stand.DestroyPreview(ctx, cli, inst)
			if err != nil {
				return destroyPreviewMsg{err: err}
			}
			all = append(all, t...)
			leftover += l
			resources = append(resources, stand.OwnedResourcePreview(inst)...)
		}
		return destroyPreviewMsg{targets: all, leftover: leftover, resources: resources}
	}
}

func (m *model) cmdStatus() tea.Cmd {
	cfg, cli := m.cfg, m.cli
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
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
	m.log = nil
	m.done = 0
	m.total = 0
	m.err = nil
	m.cancelled = false
	m.rows = map[string]stand.Event{}
	m.rowStarted = map[string]time.Time{}
	m.rowCursor = 0
	m.logOffset = 0
	m.showLog = false
	ch := make(chan stand.Event, 128)
	m.events = ch
	cfgCopy := *m.cfg
	cfgCopy.Only = m.only
	m.only = ""
	cfg := &cfgCopy
	cli, parallel, lf := m.cli, m.parallel, m.logFile
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	go func() {
		defer close(ch)
		err := stand.Execute(ctx, cli, cfg, action, parallel, func(e stand.Event) {
			e.Msg = cli.Redact(e.Msg)
			if lf != nil {
				fmt.Fprintf(lf, "%s %s\n", time.Now().Format(time.RFC3339), e.Msg)
			}
			ch <- e
		})
		if err != nil {
			err = fmt.Errorf("%w", err)
		}
		ch <- stand.Event{Final: true, Err: err}
	}()
	return tea.Batch(waitEvent(ch), tickCmd())
}
func (m *model) cmdPlan() tea.Cmd {
	cfg, cli := m.cfg, m.cli
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		lines, err := stand.LivePlan(ctx, cli, cfg)
		return planMsg{lines, err}
	}
}
