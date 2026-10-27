package stand

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"pvestand/internal/pve"
)

type Step struct {
	Name     string
	Run      func(context.Context) error
	VMID     *int
	Resource string
}
type Event struct {
	Msg             string
	Done, Total     int
	Err             error
	Final           bool
	Resource, Stage string
	VMID            int
	Duration        time.Duration
}
type Chain struct {
	Label     string
	DependsOn []string
	Steps     []Step
}
type Plan struct {
	Pre      []Step
	Parallel []Chain
	Post     []Step
}

func (p *Plan) Steps() []Step {
	out := append([]Step{}, p.Pre...)
	for _, ch := range p.Parallel {
		out = append(out, ch.Steps...)
	}
	return append(out, p.Post...)
}
func (p *Plan) Describe() []string {
	var out []string
	for _, s := range p.Pre {
		out = append(out, s.Name)
	}
	for _, ch := range p.Parallel {
		for _, s := range ch.Steps {
			out = append(out, "∥ "+s.Name)
		}
	}
	for _, s := range p.Post {
		out = append(out, s.Name)
	}
	return out
}
func (p *Plan) total() int { return len(p.Steps()) }

const taskTimeout = 30 * time.Minute
const DefaultParallelism = 1
const BaselineSnapshot = "stand-clean"

func Run(ctx context.Context, p *Plan, parallel int, emit func(Event)) error {
	if parallel < 1 {
		return errors.New("parallel должен быть >= 1")
	}
	if emit == nil {
		emit = func(Event) {}
	}
	done := 0
	var mu sync.Mutex
	step := func(ctx context.Context, s Step) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		start := time.Now()
		send := func(stage, msg string, err error) {
			mu.Lock()
			defer mu.Unlock()
			if stage == "done" {
				done++
			}
			id := 0
			if s.VMID != nil {
				id = *s.VMID
			}
			emit(Event{Msg: msg, Done: done, Total: p.total(), Resource: s.Resource, Stage: stage, VMID: id, Duration: time.Since(start), Err: err})
		}
		send("running", "▶ "+s.Name, nil)
		err := s.Run(ctx)
		if err != nil {
			send("failed", "✗ "+s.Name+": "+err.Error(), err)
			return fmt.Errorf("%s: %w", s.Name, err)
		}
		send("done", "✓ "+s.Name, nil)
		return nil
	}
	for _, s := range p.Pre {
		if err := step(ctx, s); err != nil {
			return err
		}
	}
	if err := runParallel(ctx, p.Parallel, parallel, step); err != nil {
		return err
	}
	for _, s := range p.Post {
		if err := step(ctx, s); err != nil {
			return err
		}
	}
	return nil
}
func runParallel(parent context.Context, chains []Chain, limit int, step func(context.Context, Step) error) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	type result struct {
		name string
		err  error
	}
	results := make(chan result, len(chains))
	started, done := map[string]bool{}, map[string]bool{}
	running, finished := 0, 0
	var first error
	for finished < len(chains) {
		if first == nil && ctx.Err() == nil {
			for _, ch := range chains {
				if running >= limit {
					break
				}
				if started[ch.Label] {
					continue
				}
				ready := true
				for _, dep := range ch.DependsOn {
					if !done[dep] {
						ready = false
					}
				}
				if !ready {
					continue
				}
				started[ch.Label] = true
				running++
				ch := ch
				go func() {
					var err error
					for _, s := range ch.Steps {
						if err = step(ctx, s); err != nil {
							break
						}
					}
					results <- result{ch.Label, err}
				}()
			}
		}
		if running == 0 {
			if first != nil {
				return first
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return errors.New("невыполнимые зависимости плана")
		}
		r := <-results
		running--
		finished++
		if r.err != nil && first == nil {
			first = r.err
			cancel()
		} else if r.err == nil {
			done[r.name] = true
		}
		if first != nil && running == 0 {
			return first
		}
	}
	return first
}
func retry(ctx context.Context, tries int, fn func() error) error {
	var err error
	for i := 0; i < tries; i++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err = fn(); err == nil {
			return nil
		}
		if !pve.Transient(err) || i == tries-1 {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(1<<i) * time.Second):
		}
	}
	return err
}
func waitUPID(ctx context.Context, c *pve.Client, node, upid string, err error) error {
	if err != nil {
		return err
	}
	return c.WaitTask(ctx, node, upid, taskTimeout)
}
func hasTag(tags, tag string) bool {
	for _, s := range strings.FieldsFunc(tags, func(r rune) bool { return r == ';' || r == ',' || r == ' ' }) {
		if s == tag {
			return true
		}
	}
	return false
}
func (c *Config) ownedVM(raw map[string]string) bool { return raw["description"] == c.marker() }
func checkVM(ctx context.Context, c *pve.Client, cfg *Config, r Resource) (map[string]string, error) {
	raw, err := c.VMConfigRaw(ctx, cfg.Node, r.VMID)
	if err != nil {
		return nil, err
	}
	if !cfg.ownedVM(raw) || raw["name"] != r.Name {
		return nil, fmt.Errorf("VMID %d: имя или метка владельца не совпадает; ресурс не изменён", r.VMID)
	}
	return raw, nil
}
func task(ctx context.Context, c *pve.Client, cfg *Config, r Resource, operation string, call func() (string, error)) error {
	if r.UPID != "" {
		if err := resumeTask(ctx, c, cfg, r); err != nil {
			return err
		}
		r.UPID = ""
	}
	r.Operation = operation
	if operation == "clone" {
		r.RequestedAt = time.Now().Unix()
	}
	r.Stage = "intent"
	if err := cfg.put(r); err != nil {
		return err
	}
	upid, err := call()
	if operation == "clone" && (err != nil || upid == "") {
		if err == nil {
			err = errors.New("Proxmox не вернул UPID клонирования")
		}
		r.Stage = "uncertain"
		var ae *pve.APIError
		if errors.As(err, &ae) && ae.Status >= 400 && ae.Status < 500 && ae.Status != 408 {
			r.Stage = "rejected"
		}
		if saveErr := cfg.put(r); saveErr != nil {
			return fmt.Errorf("VMID %d: ошибка сохранения результата: %w", r.VMID, saveErr)
		}
		return fmt.Errorf("%s: VMID %d, результат запроса clone не подтверждён: %w", r.Name, r.VMID, err)
	}
	if err != nil {
		return err
	}
	r.UPID = upid
	r.Stage = "waiting"
	if err = cfg.put(r); err != nil {
		return fmt.Errorf("UPID %s: не удалось сохранить задачу: %w", upid, err)
	}
	return resumeTask(ctx, c, cfg, r)
}
func resumeTask(ctx context.Context, c *pve.Client, cfg *Config, r Resource) error {
	if r.UPID == "" {
		return nil
	}
	err := c.WaitTask(ctx, cfg.Node, r.UPID, taskTimeout)
	var te *pve.TaskError
	if err == nil || errors.As(err, &te) {
		r.UPID = ""
		r.Stage = "done"
		if err != nil {
			r.Stage = "failed"
		}
		if e := cfg.put(r); e != nil {
			return e
		}
	}
	return err
}
func ensurePool(ctx context.Context, c *pve.Client, cfg *Config) error {
	info, err := c.PoolInfo(ctx, cfg.Pool)
	if err == nil {
		if info["comment"] != cfg.marker() {
			return fmt.Errorf("пул %s существует без метки владельца; выберите новый pool", cfg.Pool)
		}
		return cfg.put(Resource{Kind: "pool", Name: cfg.Pool, Stage: "done"})
	}
	if !pve.IsMissing(err) {
		return err
	}
	r := Resource{Kind: "pool", Name: cfg.Pool, Stage: "intent"}
	if err = cfg.put(r); err != nil {
		return err
	}
	if err = c.CreatePool(ctx, cfg.Pool, cfg.marker()); err != nil {
		return err
	}
	r.Stage = "done"
	return cfg.put(r)
}
func ensureBridge(ctx context.Context, c *pve.Client, cfg *Config, n Network) error {
	fields, ok, err := c.NetworkIface(ctx, cfg.Node, n.Bridge)
	if err != nil {
		return err
	}
	if ok {
		if fields["type"] != "bridge" {
			return fmt.Errorf("%s существует и не является bridge", n.Bridge)
		}
		if ownedComment(fields["comments"], cfg.marker()) {
			return cfg.put(Resource{Kind: "network", Name: n.Bridge, Stage: "done"})
		}
		return nil
	}
	r := Resource{Kind: "network", Name: n.Bridge, Stage: "intent"}
	if err = cfg.put(r); err != nil {
		return err
	}
	if err = c.CreateBridge(ctx, cfg.Node, n.Bridge, strings.TrimSpace(cfg.marker()+"\n"+n.Comment)); err != nil {
		return err
	}
	r.Stage = "done"
	return cfg.put(r)
}
func cloneVM(ctx context.Context, c *pve.Client, cfg *Config, vm VM, name string, id *int) error {
	if r, ok := cfg.record("vm", name); ok {
		*id = r.VMID
		if pendingClone(r) {
			r.Template = vm.Template
			return recoverClone(ctx, c, cfg, r)
		}
		if r.UPID != "" {
			if err := resumeTask(ctx, c, cfg, r); err != nil {
				return err
			}
		}
		raw, err := checkVM(ctx, c, cfg, r)
		if err == nil {
			if r.Operation == "clone" && r.Stage == "failed" {
				return fmt.Errorf("VMID %d: предыдущая операция clone завершилась ошибкой; проверьте диски или удалите принадлежащий стенду ресурс через destroy", r.VMID)
			}
			if raw["lock"] != "" {
				return fmt.Errorf("VMID %d заблокирована: %s", r.VMID, raw["lock"])
			}
			return nil
		}
		if !pve.IsMissing(err) {
			return err
		}
	}
	// A name match alone never authorizes adoption.
	members, err := c.ClusterVMs(ctx)
	if err != nil {
		return err
	}
	for _, m := range members {
		if m.Name == name && !bool(m.Template) {
			return fmt.Errorf("ВМ %s (VMID %d) уже существует без записи владельца; выберите другое имя", name, m.VMID)
		}
	}
	for attempt := 0; attempt < 5; attempt++ {
		next, err := c.NextID(ctx)
		if err != nil {
			return err
		}
		*id = next
		r := Resource{Kind: "vm", Name: name, VMID: next, Template: vm.Template}
		err = task(ctx, c, cfg, r, "clone", func() (string, error) {
			return c.CloneOwned(ctx, cfg.Node, vm.Template, next, name, cfg.Pool, vm.Storage, cfg.marker(), cfg.FullClone)
		})
		if pve.IDConflict(err) {
			continue
		}
		if err != nil {
			if saved, ok := cfg.record("vm", name); ok && pendingClone(saved) {
				return recoverClone(ctx, c, cfg, saved)
			}
		}
		return err
	}
	return errors.New("не удалось выделить VMID после 5 конфликтов")
}
func DeploySteps(c *pve.Client, cfg *Config) *Plan {
	p := &Plan{}
	p.Pre = append(p.Pre, Step{Name: "Проверить журнал состояния", Run: func(context.Context) error {
		if cfg.Runtime == nil {
			return errors.New("журнал состояния не подключён")
		}
		return nil
	}})
	p.Pre = append(p.Pre, Step{Name: "Создать или проверить пул " + cfg.Pool, Run: func(ctx context.Context) error { return ensurePool(ctx, c, cfg) }})
	for _, n := range cfg.Networks {
		n := n
		p.Pre = append(p.Pre, Step{Name: "Создать или использовать сеть " + n.Bridge, Run: func(ctx context.Context) error { return ensureBridge(ctx, c, cfg, n) }})
	}
	if len(cfg.Networks) > 0 {
		p.Pre = append(p.Pre, Step{Name: "Применить сетевые настройки узла", Run: func(ctx context.Context) error {
			// Re-apply after interruption even if the bridge was already created in pending configuration.
			for _, n := range cfg.Networks {
				if _, ok := cfg.record("network", n.Bridge); ok {
					return applyNetwork(ctx, c, cfg)
				}
			}
			return nil
		}})
	}
	selected := selectedVMs(cfg)
	for _, vm := range cfg.VMs {
		for i := 1; i <= vm.Count; i++ {
			vm, name := vm, vm.InstanceName(i, cfg.suffix)
			if !selected[name] {
				continue
			}
			id := new(int)
			if r, ok := cfg.record("vm", name); ok {
				*id = r.VMID
			}
			ch := Chain{Label: name}
			for _, dep := range vm.DependsOn {
				for _, other := range cfg.VMs {
					if other.Name == dep {
						for j := 1; j <= other.Count; j++ {
							ch.DependsOn = append(ch.DependsOn, other.InstanceName(j, cfg.suffix))
						}
					}
				}
			}
			makeStep := func(title string, fn func(context.Context) error) Step {
				return Step{Name: title + " " + name, Run: fn, Resource: name, VMID: id}
			}
			ch.Steps = append(ch.Steps, makeStep("Клонировать/восстановить", func(ctx context.Context) error { return cloneVM(ctx, c, cfg, vm, name, id) }))
			ch.Steps = append(ch.Steps, makeStep("Настроить", func(ctx context.Context) error {
				r, _ := cfg.record("vm", name)
				raw, err := checkVM(ctx, c, cfg, r)
				if err != nil {
					return err
				}
				params := desiredVM(vm, raw, cfg.Tag())
				if len(configChanges(params, raw)) == 0 {
					return nil
				}
				status, err := c.VMStatus(ctx, cfg.Node, *id)
				if err != nil {
					return err
				}
				if status == "running" {
					return fmt.Errorf("VMID %d работает и требует изменения конфигурации; остановите её перед изменением", *id)
				}
				if err = retry(ctx, 3, func() error { return c.SetVMConfig(ctx, cfg.Node, *id, params) }); err != nil {
					return err
				}
				r.Stage = "configured"
				return cfg.put(r)
			}))
			if vm.Start {
				ch.Steps = append(ch.Steps, makeStep("Запустить", func(ctx context.Context) error {
					r, _ := cfg.record("vm", name)
					if _, err := checkVM(ctx, c, cfg, r); err != nil {
						return err
					}
					st, err := c.VMStatus(ctx, cfg.Node, *id)
					if err != nil {
						return err
					}
					if st != "running" {
						return task(ctx, c, cfg, r, "start", func() (string, error) { return c.StartVM(ctx, cfg.Node, *id) })
					}
					return nil
				}))
				if vm.Ready != "" {
					ch.Steps = append(ch.Steps, makeStep("Дождаться готовности", func(ctx context.Context) error { return waitReady(ctx, c, cfg, vm, *id) }))
				}
			}
			p.Parallel = append(p.Parallel, ch)
		}
	}
	for _, u := range cfg.Users {
		u := u
		p.Post = append(p.Post, Step{Name: "Создать пользователя и выдать роль " + u.ID, Run: func(ctx context.Context) error {
			raw, err := c.UserInfo(ctx, u.ID)
			if err == nil {
				if raw["comment"] != cfg.marker() {
					return fmt.Errorf("пользователь %s существует без метки владельца; доступ не изменён", u.ID)
				}
			} else {
				if !pve.IsMissing(err) {
					return err
				}
				if err = cfg.put(Resource{Kind: "user", Name: u.ID, Stage: "intent"}); err != nil {
					return err
				}
				if err = c.CreateOwnedUser(ctx, u.ID, u.Password, cfg.marker()); err != nil {
					return err
				}
			}
			if err = cfg.put(Resource{Kind: "user", Name: u.ID, Stage: "created"}); err != nil {
				return err
			}
			if err = c.SetACL(ctx, "/pool/"+cfg.Pool, u.Role, u.ID, false); err != nil {
				return err
			}
			return cfg.put(Resource{Kind: "user", Name: u.ID, Stage: "done"})
		}})
	}
	return p
}
func selectedVMs(cfg *Config) map[string]bool {
	out := map[string]bool{}
	groups := map[string]bool{}
	for _, v := range cfg.VMs {
		for i := 1; i <= v.Count; i++ {
			n := v.InstanceName(i, cfg.suffix)
			if cfg.Only == "" || cfg.Only == v.Name || cfg.Only == n {
				out[n] = true
				groups[v.Name] = true
			}
		}
	}
	for changed := true; changed; {
		changed = false
		for _, v := range cfg.VMs {
			if !groups[v.Name] {
				continue
			}
			for _, dep := range v.DependsOn {
				if !groups[dep] {
					groups[dep] = true
					changed = true
				}
				for _, d := range cfg.VMs {
					if d.Name == dep {
						for i := 1; i <= d.Count; i++ {
							out[d.InstanceName(i, cfg.suffix)] = true
						}
					}
				}
			}
		}
	}
	return out
}
func destroyTargets(ctx context.Context, c *pve.Client, cfg *Config) ([]pve.Member, []pve.Member, error) {
	if err := Bind(c, cfg); err != nil {
		return nil, nil, err
	}
	all, err := c.PoolMembers(ctx, cfg.Pool)
	if pve.IsMissing(err) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var targets []pve.Member
	for _, m := range all {
		if m.Type != "qemu" {
			continue
		}
		if r, ok := cfg.record("vm", m.Name); ok && r.VMID == m.VMID && m.Node == cfg.Node {
			if _, err := checkVM(ctx, c, cfg, r); err != nil {
				return nil, nil, err
			}
			targets = append(targets, m)
		}
	}
	return targets, all, nil
}
func DestroyPreview(ctx context.Context, c *pve.Client, cfg *Config) ([]pve.Member, int, error) {
	targets, all, err := destroyTargets(ctx, c, cfg)
	return targets, len(all) - len(targets), err
}
func DestroyPlan(ctx context.Context, c *pve.Client, cfg *Config) (*Plan, error) {
	if err := Bind(c, cfg); err != nil {
		return nil, err
	}
	p := &Plan{}
	records := cfg.resources()
	sort.Slice(records, func(i, j int) bool { return records[i].Name < records[j].Name })
	for _, r := range records {
		if r.Kind != "vm" {
			continue
		}
		r := r
		id := r.VMID
		p.Parallel = append(p.Parallel, Chain{Label: r.Name, Steps: []Step{{Name: "Удалить " + r.Name, Resource: r.Name, VMID: &id, Run: func(ctx context.Context) error {
			if _, err := checkVM(ctx, c, cfg, r); pve.IsMissing(err) {
				return cfg.forget("vm", r.Name)
			} else if err != nil {
				return err
			}
			st, err := c.VMStatus(ctx, cfg.Node, r.VMID)
			if err != nil {
				return err
			}
			if st == "running" {
				if err = task(ctx, c, cfg, r, "stop", func() (string, error) { return c.StopVM(ctx, cfg.Node, r.VMID) }); err != nil {
					return err
				}
			}
			latest, _ := cfg.record("vm", r.Name)
			if err = task(ctx, c, cfg, latest, "delete", func() (string, error) { return c.DeleteVM(ctx, cfg.Node, r.VMID) }); err != nil {
				return err
			}
			return cfg.forget("vm", r.Name)
		}}}})
	}
	for _, r := range records {
		r := r
		switch r.Kind {
		case "user":
			p.Post = append(p.Post, Step{Name: "Удалить созданного пользователя " + r.Name, Run: func(ctx context.Context) error {
				info, err := c.UserInfo(ctx, r.Name)
				if pve.IsMissing(err) {
					return cfg.forget("user", r.Name)
				}
				if err != nil {
					return err
				}
				if info["comment"] != cfg.marker() {
					return fmt.Errorf("пользователь %s сменил владельца", r.Name)
				}
				if err = c.DeleteUser(ctx, r.Name); err != nil {
					return err
				}
				return cfg.forget("user", r.Name)
			}})
		case "network":
			p.Post = append(p.Post, Step{Name: "Удалить созданную сеть " + r.Name, Run: func(ctx context.Context) error {
				info, ok, err := c.NetworkIface(ctx, cfg.Node, r.Name)
				if err != nil {
					return err
				}
				if !ok {
					return cfg.forget("network", r.Name)
				}
				perms, e := c.Permissions(ctx, "/vms")
				if e != nil {
					return e
				}
				if !perms["VM.Audit"] {
					return fmt.Errorf("перед удалением сети %s нужно VM.Audit на /vms для проверки всех подключений", r.Name)
				}
				if ports := info["bridge_ports"]; ports != "" && ports != "none" {
					return fmt.Errorf("у сети %s появились физические порты; удаление заблокировано", r.Name)
				}
				if info["address"] != "" || info["address6"] != "" {
					return fmt.Errorf("у сети %s появился IP узла; удаление заблокировано", r.Name)
				}
				if !ownedComment(info["comments"], cfg.marker()) {
					return fmt.Errorf("сеть %s сменилa владельца", r.Name)
				}
				vms, err := c.ClusterVMs(ctx)
				if err != nil {
					return err
				}
				for _, v := range vms {
					if v.Node != cfg.Node {
						continue
					}
					if v.Type != "qemu" {
						return fmt.Errorf("на узле есть LXC: перед удалением сети %s требуется ручная проверка подключений", r.Name)
					}
					raw, err := c.VMConfigRaw(ctx, v.Node, v.VMID)
					if err != nil {
						return err
					}
					for key, value := range raw {
						if strings.HasPrefix(key, "net") && parseProperties(value)["bridge"] == r.Name {
							return fmt.Errorf("сеть %s используется VMID %d", r.Name, v.VMID)
						}
					}
				}
				if err = c.DeleteNetwork(ctx, cfg.Node, r.Name); err != nil {
					return err
				}
				return nil
			}})
		}
	}
	p.Post = append(p.Post, Step{Name: "Применить удаление сетей", Run: func(ctx context.Context) error {
		has := false
		for _, r := range records {
			if r.Kind == "network" {
				has = true
			}
		}
		if !has {
			return nil
		}
		if err := applyNetwork(ctx, c, cfg); err != nil {
			return err
		}
		for _, r := range records {
			if r.Kind == "network" {
				if err := cfg.forget("network", r.Name); err != nil {
					return err
				}
			}
		}
		return nil
	}})
	p.Post = append(p.Post, Step{Name: "Удалить созданный пул, если пуст", Run: func(ctx context.Context) error {
		if _, ok := cfg.record("pool", cfg.Pool); !ok {
			return nil
		}
		info, err := c.PoolInfo(ctx, cfg.Pool)
		if pve.IsMissing(err) {
			return cfg.forget("pool", cfg.Pool)
		}
		if err != nil {
			return err
		}
		if info["comment"] != cfg.marker() {
			return fmt.Errorf("метка пула не совпадает")
		}
		members, err := c.PoolMembers(ctx, cfg.Pool)
		if err != nil {
			return err
		}
		if len(members) > 0 {
			return nil
		}
		if err = c.DeletePool(ctx, cfg.Pool); err != nil {
			return err
		}
		return cfg.forget("pool", cfg.Pool)
	}})
	return p, nil
}
func SnapshotPlan(ctx context.Context, c *pve.Client, cfg *Config) (*Plan, error) {
	return snapshotPlan(ctx, c, cfg, false)
}
func ResetPlan(ctx context.Context, c *pve.Client, cfg *Config) (*Plan, error) {
	return snapshotPlan(ctx, c, cfg, true)
}
func snapshotPlan(ctx context.Context, c *pve.Client, cfg *Config, reset bool) (*Plan, error) {
	targets, _, err := destroyTargets(ctx, c, cfg)
	if err != nil {
		return nil, err
	}
	p := &Plan{}
	for _, m := range targets {
		m := m
		r, _ := cfg.record("vm", m.Name)
		id := m.VMID
		title := "Снапшот "
		if reset {
			title = "Откатить "
		}
		p.Parallel = append(p.Parallel, Chain{Label: m.Name, Steps: []Step{{Name: title + m.Name, Resource: m.Name, VMID: &id, Run: func(ctx context.Context) error {
			if _, err := checkVM(ctx, c, cfg, r); err != nil {
				return err
			}
			have, err := c.HasSnapshot(ctx, m.Node, m.VMID, BaselineSnapshot)
			if err != nil {
				return err
			}
			if !reset {
				if have {
					return nil
				}
				return task(ctx, c, cfg, r, "snapshot", func() (string, error) {
					return c.CreateSnapshot(ctx, m.Node, m.VMID, BaselineSnapshot, "pvestand: чистое состояние")
				})
			}
			if !have {
				return fmt.Errorf("VMID %d: нет снапшота %s", m.VMID, BaselineSnapshot)
			}
			if err = task(ctx, c, cfg, r, "rollback", func() (string, error) { return c.RollbackSnapshot(ctx, m.Node, m.VMID, BaselineSnapshot) }); err != nil {
				return err
			}
			for _, v := range cfg.VMs {
				for i := 1; i <= v.Count; i++ {
					if v.InstanceName(i, cfg.suffix) == m.Name && v.Start {
						st, e := c.VMStatus(ctx, m.Node, m.VMID)
						if e != nil {
							return e
						}
						if st != "running" {
							latest, _ := cfg.record("vm", m.Name)
							if e = task(ctx, c, cfg, latest, "start", func() (string, error) { return c.StartVM(ctx, m.Node, m.VMID) }); e != nil {
								return e
							}
						}
						return waitReady(ctx, c, cfg, v, m.VMID)
					}
				}
			}
			return nil
		}}}})
	}
	return p, nil
}

func applyNetwork(ctx context.Context, c *pve.Client, cfg *Config) error {
	r, ok := cfg.record("task", "network-apply")
	if !ok {
		r = Resource{Kind: "task", Name: "network-apply"}
	}
	if err := task(ctx, c, cfg, r, "network-apply", func() (string, error) { return c.BeginNetworkApply(ctx, cfg.Node) }); err != nil {
		return err
	}
	return cfg.forget("task", "network-apply")
}

func ownedComment(comment, marker string) bool { return strings.SplitN(comment, "\n", 2)[0] == marker }
