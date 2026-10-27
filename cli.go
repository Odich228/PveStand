package main

import (
	"context"
	"flag"
	"fmt"
	tea "github.com/charmbracelet/bubbletea"
	"os"
	"os/signal"
	"pvestand/internal/pve"
	"pvestand/internal/stand"
	"strings"
	"syscall"
	"time"
)

var Version = "dev"

func openLogFile(path string) (*os.File, error) {
	if path == "" {
		path = "pvestand-" + time.Now().Format("20060102-150405") + ".log"
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err == nil {
		if err = f.Chmod(0600); err != nil {
			f.Close()
			return nil, err
		}
	}
	return f, err
}
func main() {
	cfgPath := flag.String("c", "stand.yaml", "путь к YAML-описанию стенда")
	host := flag.String("host", os.Getenv("PVE_HOST"), "адрес Proxmox или PVE_HOST")
	token := flag.String("token", "", "API-токен (предпочтительно PVE_TOKEN или -token-file)")
	tokenFile := flag.String("token-file", "", "файл API-токена с правами 0600")
	insecure := flag.Bool("insecure", false, "не проверять TLS-сертификат")
	run := flag.String("run", "", "validate | doctor | plan | deploy | destroy | snapshot | reset | status | tasklog | l2manifest")
	parallel := flag.Int("parallel", stand.DefaultParallelism, "число одновременно обрабатываемых ВМ")
	logPath := flag.String("log", "", "файл журнала, в TUI создаётся автоматически")
	profile := flag.String("profile", "", "имя профиля подключения")
	profiles := flag.String("profiles", "profiles.yaml", "файл профилей")
	stateDir := flag.String("state-dir", ".pvestand", "каталог журнала состояния; сохраняйте между запусками")
	participant := flag.String("participant", "", "участник A0 и т.п.; по умолчанию все стенды")
	manifestOut := flag.String("out", "region-l2.manifest.json", "выходной JSON для -run l2manifest")
	taskNode := flag.String("task-node", "", "узел задачи для -run tasklog; иначе узел выбранного стенда")
	retryClone := flag.String("retry-clone", "", "явно повторить неподтверждённый clone выбранной ВМ в тот же сохранённый VMID")
	only := flag.String("only", "", "для deploy: имя ВМ или группы; зависимости включаются автоматически")
	credentials := flag.String("credentials", "", "после deploy сохранить пароли в файл 0600")
	upid := flag.String("upid", "", "UPID задачи для tasklog")
	offline := flag.Bool("offline", false, "статический plan без подключения к Proxmox")
	noAnimation := flag.Bool("no-animation", false, "отключить анимации TUI и стартовую заставку")
	noSplash := flag.Bool("no-splash", false, "пропустить анимированный стартовый экран TUI")
	version := flag.Bool("version", false, "версия программы")
	flag.Parse()
	if *version {
		fmt.Println(Version)
		return
	}
	set := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if *parallel < 1 {
		fatal(fmt.Errorf("parallel должен быть >= 1"))
	}
	stand.DefaultStateDir = *stateDir
	if *run == "validate" || *run == "l2manifest" {
		cfg, err := stand.LoadStructure(*cfgPath)
		if err != nil {
			fatal(err)
		}
		if err := cfg.SelectParticipant(*participant); err != nil {
			fatal(err)
		}
		if *run == "l2manifest" {
			if err := stand.WriteL2Manifest(*manifestOut, cfg); err != nil {
				fatal(err)
			}
			fmt.Println("Карта L2 сохранена: " + *manifestOut)
			return
		}
		fmt.Printf("Структура конфига корректна: %s; копий %d\n", cfg.Name, len(cfg.Instances()))
		return
	}
	if *run != "" && !strings.Contains("|doctor|plan|deploy|destroy|snapshot|reset|status|tasklog|l2manifest|", "|"+*run+"|") {
		fatal(fmt.Errorf("неизвестное действие %q", *run))
	}
	if *retryClone != "" && *run != "deploy" {
		fatal(fmt.Errorf("-retry-clone требует -run deploy"))
	}
	if *only != "" && *run != "deploy" {
		fatal(fmt.Errorf("-only требует -run deploy"))
	}
	if *offline && *run != "plan" {
		fatal(fmt.Errorf("-offline требует -run plan"))
	}
	if *offline {
		cfg, err := stand.LoadStructure(*cfgPath)
		if err != nil {
			fatal(err)
		}
		if err := cfg.SelectParticipant(*participant); err != nil {
			fatal(err)
		}
		for _, line := range cfg.InstanceSummary() {
			fmt.Println(line)
		}
		for _, inst := range cfg.Instances() {
			for _, line := range stand.DeploySteps(nil, inst).Describe() {
				fmt.Println(line)
			}
		}
		return
	}
	if *profile != "" {
		p, err := stand.LoadProfile(*profiles, *profile)
		if err != nil {
			fatal(err)
		}
		if !set["host"] && *host == "" {
			*host = p.Host
		}
		if !set["insecure"] {
			*insecure = p.Insecure
		}
		if !set["state-dir"] && p.StateDir != "" {
			stand.DefaultStateDir = p.StateDir
		}
		if !set["token"] && !set["token-file"] {
			if p.TokenFile != "" {
				*tokenFile = p.TokenFile
			} else if p.TokenEnv != "" {
				*token = os.Getenv(p.TokenEnv)
			}
		}
	}
	if *tokenFile != "" {
		if set["token"] {
			fatal(fmt.Errorf("используйте -token или -token-file"))
		}
		v, err := stand.ReadSecret(*tokenFile)
		if err != nil {
			fatal(err)
		}
		*token = v
	}
	if *token == "" {
		*token = os.Getenv("PVE_TOKEN")
	}
	if *run != "" {
		cfg, err := stand.LoadForParticipant(*cfgPath, *participant)
		if err != nil {
			fatal(err)
		}
		if err := cfg.SelectParticipant(*participant); err != nil {
			fatal(err)
		}
		cfg.Only = *only
		cfg.RetryClone = *retryClone
		cli, err := pve.New(*host, *token, *insecure)
		if err != nil {
			fatal(err)
		}
		cli.SetSecrets(cfg.Secrets())
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		var lf *os.File
		if *logPath != "" {
			lf, err = openLogFile(*logPath)
			if err != nil {
				fatal(err)
			}
			defer lf.Close()
		}
		log := func(s string) {
			s = cli.Redact(s)
			fmt.Println(s)
			if lf != nil {
				fmt.Fprintln(lf, time.Now().Format(time.RFC3339), s)
			}
		}
		switch *run {
		case "doctor":
			r := stand.Check(ctx, cli, cfg)
			for _, line := range r.Info {
				log(line)
			}
			for _, line := range r.Warnings {
				log("⚠ " + line)
			}
			for _, line := range r.Errors {
				log("✗ " + line)
			}
			if !r.OK() {
				fatal(fmt.Errorf("проверка не пройдена"))
			}
		case "plan":
			lines, err := stand.LivePlan(ctx, cli, cfg)
			if err != nil {
				fatal(err)
			}
			for _, s := range lines {
				log(s)
			}
		case "tasklog":
			if *upid == "" {
				fatal(fmt.Errorf("укажите -upid"))
			}
			node := *taskNode
			if node == "" {
				instances := cfg.Instances()
				if len(instances) == 1 {
					node = instances[0].Node
				} else {
					fatal(fmt.Errorf("для tasklog выберите -participant или задайте -task-node"))
				}
			}
			s, err := cli.TaskLog(ctx, node, *upid)
			if err != nil {
				fatal(err)
			}
			log(s)
		case "status":
			for _, inst := range cfg.Instances() {
				if err := printStatus(ctx, cli, inst, log); err != nil {
					fatal(err)
				}
			}
		default:
			err := stand.Execute(ctx, cli, cfg, *run, *parallel, func(e stand.Event) {
				id := ""
				if e.VMID > 0 {
					id = fmt.Sprintf(" VMID %d", e.VMID)
				}
				log(fmt.Sprintf("[%d/%d%s] %s (%s)", e.Done, e.Total, id, e.Msg, e.Duration.Round(time.Millisecond)))
			})
			if err != nil {
				fatal(fmt.Errorf("%s", cli.Redact(err.Error())))
			}
			if *run == "deploy" && *credentials != "" {
				if err = stand.WriteCredentialsFile(*credentials, cfg.Instances()); err != nil {
					fatal(err)
				}
				log("Учётные данные сохранены: " + *credentials)
			}
		}
		return
	}
	lf, err := openLogFile(*logPath)
	if err != nil {
		fatal(err)
	}
	defer lf.Close()
	m := newModel(*cfgPath, set["c"], *host, *token, *insecure, *parallel, lf, *participant)
	m.splash = !*noSplash && !*noAnimation
	m.motion = !*noAnimation
	m.participant = *participant
	if m.cfg != nil {
		if err := m.cfg.SelectParticipant(*participant); err != nil {
			fatal(err)
		}
	}
	if _, err = tea.NewProgram(m, tea.WithAltScreen()).Run(); err != nil {
		fatal(err)
	}
}
func printStatus(ctx context.Context, cli *pve.Client, cfg *stand.Config, log func(string)) error {
	members, err := cli.PoolMembers(ctx, cfg.Pool)
	if err != nil {
		return err
	}
	for _, m := range members {
		if m.Type != "qemu" {
			continue
		}
		status, err := cli.VMStatus(ctx, m.Node, m.VMID)
		if err != nil {
			return err
		}
		ips := []string{}
		if status == "running" {
			ips, _ = cli.AgentIPs(ctx, m.Node, m.VMID)
		}
		log(fmt.Sprintf("%-24s VMID %-6d %-10s %s", m.Name, m.VMID, status, strings.Join(ips, ", ")))
	}
	return nil
}
func fatal(err error) { fmt.Fprintln(os.Stderr, "Ошибка:", err); os.Exit(1) }
