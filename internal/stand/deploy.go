package stand

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"pvestand/internal/pve"
)

// Step — один шаг развёртывания или удаления.
type Step struct {
	Name string
	Run  func(ctx context.Context) error
}

// Event — событие прогресса для TUI или консоли.
type Event struct {
	Msg   string
	Done  int
	Total int
	Err   error
	Final bool
}

// Chain — последовательность шагов, которые должны выполняться строго по
// порядку сами по себе (например, клонировать → настроить → запустить одну
// ВМ), но которую можно выполнять параллельно с другими такими же цепочками.
type Chain struct {
	Label string
	Steps []Step
}

// Plan — план развёртывания/удаления, разбитый на стадии:
//   - Pre — выполняется последовательно, по одному шагу (пул, сети);
//   - Parallel — независимые друг от друга цепочки шагов (например, по
//     одной на ВМ), которые можно выполнять одновременно, но каждая сама по
//     себе — строго по порядку;
//   - Post — снова последовательно (пользователи, финальная зачистка).
type Plan struct {
	Pre      []Step
	Parallel []Chain
	Post     []Step
}

// Steps «расплющивает» план в плоский список шагов в порядке выполнения.
func (p *Plan) Steps() []Step {
	var out []Step
	out = append(out, p.Pre...)
	for _, ch := range p.Parallel {
		out = append(out, ch.Steps...)
	}
	out = append(out, p.Post...)
	return out
}

// Describe возвращает имена шагов для показа плана (dry-run). Шаги, которые
// при выполнении могут идти параллельно с другими, помечаются символом «∥».
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

func (p *Plan) total() int {
	n := len(p.Pre) + len(p.Post)
	for _, ch := range p.Parallel {
		n += len(ch.Steps)
	}
	return n
}

const taskTimeout = 30 * time.Minute

// DefaultParallelism — сколько цепочек (обычно — ВМ) обрабатывать
// одновременно, если пользователь не указал своё значение.
const DefaultParallelism = 1

// Run выполняет план: Pre — последовательно, затем Parallel — до maxWorkers
// цепочек одновременно (каждая цепочка сама по себе выполняется по
// порядку), затем Post — последовательно. emit вызывается из разных
// горутин, но не одновременно (защищено мьютексом внутри).
func Run(ctx context.Context, plan *Plan, maxWorkers int, emit func(Event)) error {
	if maxWorkers < 1 {
		maxWorkers = 1
	}
	total := plan.total()
	var mu sync.Mutex
	done := 0

	step := func(ctx context.Context, s Step) error {
		mu.Lock()
		emit(Event{Msg: "▶ " + s.Name, Done: done, Total: total})
		mu.Unlock()

		err := s.Run(ctx)

		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			emit(Event{Msg: "✗ " + s.Name + ": " + err.Error(), Done: done, Total: total})
			return fmt.Errorf("%s: %w", s.Name, err)
		}
		done++
		emit(Event{Msg: "✓ " + s.Name, Done: done, Total: total})
		return nil
	}

	for _, s := range plan.Pre {
		if err := step(ctx, s); err != nil {
			return err
		}
	}

	if len(plan.Parallel) > 0 {
		if err := runParallel(ctx, plan.Parallel, maxWorkers, step); err != nil {
			return err
		}
	}

	for _, s := range plan.Post {
		if err := step(ctx, s); err != nil {
			return err
		}
	}
	return nil
}

// runParallel запускает цепочки шагов, не больше maxWorkers одновременно.
// При первой ошибке отменяет общий контекст: уже запущенные цепочки
// получат отменённый ctx на следующем шаге и остановятся, новые — не начнутся.
func runParallel(ctx context.Context, chains []Chain, maxWorkers int, step func(context.Context, Step) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	sem := make(chan struct{}, maxWorkers)
	var wg sync.WaitGroup
	var errMu sync.Mutex
	var firstErr error

	for _, ch := range chains {
		if ctx.Err() != nil {
			break
		}
		ch := ch
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			for _, s := range ch.Steps {
				if ctx.Err() != nil {
					return
				}
				if err := step(ctx, s); err != nil {
					errMu.Lock()
					if firstErr == nil {
						firstErr = err
						cancel()
					}
					errMu.Unlock()
					return
				}
			}
		}()
	}
	wg.Wait()
	return firstErr
}

// retry нужен из-за возможной задержки сохранения ресурсов в PVE
// (например, на Альт Виртуализации): несколько попыток с паузой.
func retry(ctx context.Context, tries int, fn func() error) error {
	var err error
	for i := 0; i < tries; i++ {
		if err = fn(); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
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

// DeploySteps строит план развёртывания. Обращений к Proxmox здесь нет,
// поэтому план можно использовать и как «показ плана» (dry-run). Каждая ВМ
// клонируется своей независимой цепочкой шагов — при выполнении (Run) эти
// цепочки идут параллельно (см. DefaultParallelism / флаг -parallel).
func DeploySteps(c *pve.Client, cfg *Config) *Plan {
	node := cfg.Node
	tag := cfg.Tag()
	plan := &Plan{}

	plan.Pre = append(plan.Pre, Step{"Создать пул " + cfg.Pool, func(ctx context.Context) error {
		err := c.CreatePool(ctx, cfg.Pool, "Стенд "+cfg.Name)
		if pve.IsExists(err) {
			return nil
		}
		return err
	}})

	if len(cfg.Networks) > 0 {
		changed := new(bool)
		for _, n := range cfg.Networks {
			n := n
			plan.Pre = append(plan.Pre, Step{"Создать сеть " + n.Bridge, func(ctx context.Context) error {
				ok, err := c.HasNetwork(ctx, node, n.Bridge)
				if err != nil {
					return err
				}
				if ok {
					return nil
				}
				*changed = true
				return c.CreateBridge(ctx, node, n.Bridge, n.Comment)
			}})
		}
		plan.Pre = append(plan.Pre, Step{"Применить сетевые настройки узла", func(ctx context.Context) error {
			if !*changed {
				return nil
			}
			return c.ApplyNetwork(ctx, node)
		}})
	}

	for _, vm := range cfg.VMs {
		for i := 1; i <= vm.Count; i++ {
			vm, name := vm, vm.InstanceName(i, cfg.suffix)
			vmid := new(int)
			var chain Chain
			chain.Label = name

			chain.Steps = append(chain.Steps, Step{"Клонировать " + name, func(ctx context.Context) error {
				// Идемпотентность: если ВМ с таким именем уже в пуле — берём её.
				members, err := c.PoolMembers(ctx, cfg.Pool)
				if err != nil {
					return err
				}
				for _, m := range members {
					if m.Type == "qemu" && m.Name == name {
						*vmid = m.VMID
						return nil
					}
				}
				id, err := c.NextID(ctx)
				if err != nil {
					return err
				}
				upid, err := c.CloneVM(ctx, node, vm.Template, id, name, cfg.Pool, cfg.FullClone)
				if err := waitUPID(ctx, c, node, upid, err); err != nil {
					return err
				}
				*vmid = id
				return nil
			}})

			// Настройку выполняем всегда (даже если cores/memory/nics не
			// заданы) — этим же вызовом проставляется тег стенда, по
			// которому потом безопасно определяется, что можно удалять.
			chain.Steps = append(chain.Steps, Step{"Настроить " + name, func(ctx context.Context) error {
				p := url.Values{}
				if vm.Cores > 0 {
					p.Set("cores", strconv.Itoa(vm.Cores))
				}
				if vm.Memory > 0 {
					p.Set("memory", strconv.Itoa(vm.Memory))
				}
				for k, nic := range vm.NICs {
					p.Set(fmt.Sprintf("net%d", k), nic.Model+",bridge="+nic.Bridge)
				}
				p.Set("tags", tag)
				return retry(ctx, 3, func() error { return c.SetVMConfig(ctx, node, *vmid, p) })
			}})

			if vm.Start {
				chain.Steps = append(chain.Steps, Step{"Запустить " + name, func(ctx context.Context) error {
					return retry(ctx, 3, func() error {
						st, err := c.VMStatus(ctx, node, *vmid)
						if err != nil {
							return err
						}
						if st == "running" {
							return nil
						}
						upid, err := c.StartVM(ctx, node, *vmid)
						return waitUPID(ctx, c, node, upid, err)
					})
				}})
			}

			plan.Parallel = append(plan.Parallel, chain)
		}
	}

	for _, u := range cfg.Users {
		u := u
		plan.Post = append(plan.Post, Step{"Создать пользователя " + u.ID, func(ctx context.Context) error {
			err := c.CreateUser(ctx, u.ID, u.Password)
			if pve.IsExists(err) {
				err = nil
			}
			if err != nil {
				return err
			}
			return c.SetACL(ctx, "/pool/"+cfg.Pool, u.Role, u.ID, false)
		}})
	}
	return plan
}

// hasTag проверяет, входит ли want в список тегов (Proxmox отдаёт их через
// ";", в старых версиях встречается ","; на всякий случай понимаем оба).
func hasTag(tags, want string) bool {
	tags = strings.NewReplacer(",", ";").Replace(tags)
	for _, t := range strings.Split(tags, ";") {
		if strings.TrimSpace(t) == want {
			return true
		}
	}
	return false
}

// destroyTargets возвращает ВМ пула, которые относятся к этому стенду
// (совпадают по имени с ожидаемым списком ВМ из конфига ИЛИ помечены тегом
// стенда), и полный список членов пула — чтобы понять, остаётся ли в пуле
// что-то постороннее.
func destroyTargets(ctx context.Context, c *pve.Client, cfg *Config) (targets, all []pve.Member, err error) {
	members, err := c.PoolMembers(ctx, cfg.Pool)
	if err != nil {
		if pve.IsMissing(err) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	expected := cfg.ExpectedVMNames()
	tag := cfg.Tag()
	for _, m := range members {
		if m.Type != "qemu" {
			continue
		}
		if expected[m.Name] || hasTag(m.Tags, tag) {
			targets = append(targets, m)
		}
	}
	return targets, members, nil
}

// DestroyPreview возвращает ВМ, которые будут удалены (для подтверждения в
// TUI), и число посторонних объектов, которые останутся в пуле.
func DestroyPreview(ctx context.Context, c *pve.Client, cfg *Config) (targets []pve.Member, leftover int, err error) {
	targets, all, err := destroyTargets(ctx, c, cfg)
	if err != nil {
		return nil, 0, err
	}
	removing := map[int]bool{}
	for _, m := range targets {
		removing[m.VMID] = true
	}
	for _, m := range all {
		if m.Type == "qemu" && !removing[m.VMID] {
			leftover++
		}
	}
	return targets, leftover, nil
}

// DestroyPlan строит план удаления. Из пула удаляются ТОЛЬКО ВМ, которые
// относятся к этому стенду (см. destroyTargets) — это защищает от
// случайного удаления чужих ВМ, если в пул попало что-то постороннее. Пул
// целиком удаляется, только если после удаления «наших» ВМ в нём ничего не
// осталось.
func DestroyPlan(ctx context.Context, c *pve.Client, cfg *Config) (*Plan, error) {
	targets, all, err := destroyTargets(ctx, c, cfg)
	if err != nil {
		return nil, err
	}
	plan := &Plan{}

	for _, m := range targets {
		m := m
		label := fmt.Sprintf("%s (%d)", m.Name, m.VMID)
		plan.Parallel = append(plan.Parallel, Chain{
			Label: m.Name,
			Steps: []Step{
				{"Остановить " + label, func(ctx context.Context) error {
					st, err := c.VMStatus(ctx, m.Node, m.VMID)
					if err != nil {
						if pve.IsMissing(err) {
							return nil
						}
						return err
					}
					if st != "running" {
						return nil
					}
					upid, err := c.StopVM(ctx, m.Node, m.VMID)
					return waitUPID(ctx, c, m.Node, upid, err)
				}},
				{"Удалить " + label, func(ctx context.Context) error {
					upid, err := c.DeleteVM(ctx, m.Node, m.VMID)
					if pve.IsMissing(err) {
						return nil
					}
					return waitUPID(ctx, c, m.Node, upid, err)
				}},
			},
		})
	}

	for _, u := range cfg.Users {
		u := u
		plan.Post = append(plan.Post, Step{"Удалить пользователя " + u.ID, func(ctx context.Context) error {
			_ = c.SetACL(ctx, "/pool/"+cfg.Pool, u.Role, u.ID, true)
			err := c.DeleteUser(ctx, u.ID)
			if pve.IsMissing(err) {
				return nil
			}
			return err
		}})
	}

	removing := map[int]bool{}
	for _, m := range targets {
		removing[m.VMID] = true
	}
	leftover := 0
	for _, m := range all {
		if m.Type == "qemu" && !removing[m.VMID] {
			leftover++
		}
	}
	if leftover == 0 {
		plan.Post = append(plan.Post, Step{"Удалить пул " + cfg.Pool, func(ctx context.Context) error {
			err := c.DeletePool(ctx, cfg.Pool)
			if pve.IsMissing(err) {
				return nil
			}
			return err
		}})
	} else {
		msg := fmt.Sprintf("Пропустить удаление пула %s (в нём остаётся %d посторонних объектов)", cfg.Pool, leftover)
		plan.Post = append(plan.Post, Step{msg, func(ctx context.Context) error { return nil }})
	}

	if len(cfg.Networks) > 0 {
		changed := new(bool)
		for _, n := range cfg.Networks {
			n := n
			plan.Post = append(plan.Post, Step{"Удалить сеть " + n.Bridge, func(ctx context.Context) error {
				ok, err := c.HasNetwork(ctx, cfg.Node, n.Bridge)
				if err != nil || !ok {
					return err
				}
				*changed = true
				return c.DeleteNetwork(ctx, cfg.Node, n.Bridge)
			}})
		}
		plan.Post = append(plan.Post, Step{"Применить сетевые настройки узла", func(ctx context.Context) error {
			if !*changed {
				return nil
			}
			return c.ApplyNetwork(ctx, cfg.Node)
		}})
	}
	return plan, nil
}

// BaselineSnapshot — имя снапшота «чистое состояние», к которому откатывает
// действие «Сбросить стенд».
const BaselineSnapshot = "stand-clean"

// SnapshotPlan строит план создания базового снапшота для всех ВМ стенда
// (параллельно, по ВМ пула cfg.Pool). Если снапшот с таким именем уже есть,
// он пересоздаётся.
func SnapshotPlan(ctx context.Context, c *pve.Client, cfg *Config) (*Plan, error) {
	members, err := c.PoolMembers(ctx, cfg.Pool)
	if err != nil {
		if pve.IsMissing(err) {
			return &Plan{}, nil
		}
		return nil, err
	}
	plan := &Plan{}
	for _, m := range members {
		if m.Type != "qemu" {
			continue
		}
		m := m
		label := fmt.Sprintf("%s (%d)", m.Name, m.VMID)
		plan.Parallel = append(plan.Parallel, Chain{
			Label: m.Name,
			Steps: []Step{{"Снапшот " + label, func(ctx context.Context) error {
				have, err := c.HasSnapshot(ctx, m.Node, m.VMID, BaselineSnapshot)
				if err != nil {
					return err
				}
				if have {
					upid, err := c.DeleteSnapshot(ctx, m.Node, m.VMID, BaselineSnapshot)
					if err := waitUPID(ctx, c, m.Node, upid, err); err != nil {
						return err
					}
				}
				upid, err := c.CreateSnapshot(ctx, m.Node, m.VMID, BaselineSnapshot, "pvestand: чистое состояние")
				return waitUPID(ctx, c, m.Node, upid, err)
			}}},
		})
	}
	return plan, nil
}

// ResetPlan строит план отката всех ВМ стенда к базовому снапшоту. Если у
// ВМ в конфиге стоит start: true, после отката она снова запускается.
func ResetPlan(ctx context.Context, c *pve.Client, cfg *Config) (*Plan, error) {
	members, err := c.PoolMembers(ctx, cfg.Pool)
	if err != nil {
		if pve.IsMissing(err) {
			return &Plan{}, nil
		}
		return nil, err
	}
	shouldStart := map[string]bool{}
	for _, vm := range cfg.VMs {
		if !vm.Start {
			continue
		}
		for i := 1; i <= vm.Count; i++ {
			shouldStart[vm.InstanceName(i, cfg.suffix)] = true
		}
	}
	plan := &Plan{}
	for _, m := range members {
		if m.Type != "qemu" {
			continue
		}
		m := m
		label := fmt.Sprintf("%s (%d)", m.Name, m.VMID)
		steps := []Step{{"Откатить " + label, func(ctx context.Context) error {
			have, err := c.HasSnapshot(ctx, m.Node, m.VMID, BaselineSnapshot)
			if err != nil {
				return err
			}
			if !have {
				return fmt.Errorf("нет снапшота %q — сначала выполните «Создать снапшот»", BaselineSnapshot)
			}
			upid, err := c.RollbackSnapshot(ctx, m.Node, m.VMID, BaselineSnapshot)
			return waitUPID(ctx, c, m.Node, upid, err)
		}}}
		if shouldStart[m.Name] {
			steps = append(steps, Step{"Запустить " + label, func(ctx context.Context) error {
				return retry(ctx, 3, func() error {
					st, err := c.VMStatus(ctx, m.Node, m.VMID)
					if err != nil {
						return err
					}
					if st == "running" {
						return nil
					}
					upid, err := c.StartVM(ctx, m.Node, m.VMID)
					return waitUPID(ctx, c, m.Node, upid, err)
				})
			}})
		}
		plan.Parallel = append(plan.Parallel, Chain{Label: m.Name, Steps: steps})
	}
	return plan, nil
}
