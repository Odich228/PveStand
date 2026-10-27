package stand

import (
	"context"
	"fmt"
	"pvestand/internal/pve"
	"sort"
	"strings"
)

// Execute is the sole entry point for mutations, shared by TUI and CLI.
func Execute(ctx context.Context, c *pve.Client, cfg *Config, action string, parallel int, emit func(Event)) error {
	if parallel < 1 {
		return fmt.Errorf("parallel должен быть >= 1")
	}
	if err := cfg.SelectParticipant(cfg.ParticipantOnly); err != nil {
		return err
	}
	if action == "deploy" {
		for _, inst := range cfg.Instances() {
			for _, u := range inst.Users {
				if len(u.Password) < 8 {
					return fmt.Errorf("пользователь %s: перед deploy прочитайте пароль через Load/LoadForParticipant", u.ID)
				}
			}
		}
	}
	if err := Bind(c, cfg); err != nil {
		return err
	}
	release, err := cfg.Runtime.acquire()
	if err != nil {
		return err
	}
	defer release()
	if err := cfg.checkLayouts(); err != nil {
		return err
	}
	if action == "deploy" {
		r := Check(ctx, c, cfg)
		for _, line := range append(r.Info, r.Warnings...) {
			if emit != nil {
				emit(Event{Msg: line})
			}
		}
		if !r.OK() {
			return fmt.Errorf("проверка перед развёртыванием:\n%s", strings.Join(r.Errors, "\n"))
		}
	}
	// Recover lost clone acknowledgements before changing other resources.
	// Re-submission is permitted only by the explicit CLI -retry-clone option.
	retryMatched := cfg.RetryClone == ""
	for _, inst := range cfg.Instances() {
		for _, r := range inst.resources() {
			if r.Kind == "vm" && r.Operation == "clone" && r.UPID != "" {
				if cfg.RetryClone == r.Name {
					retryMatched = true
				}
				if emit != nil {
					emit(Event{Msg: fmt.Sprintf("Возобновить clone: %s, VMID %d, UPID %s", r.Name, r.VMID, r.UPID), Resource: r.Name, VMID: r.VMID})
				}
				if err := resumeTask(ctx, c, inst, r); err != nil {
					return err
				}
				raw, err := c.VMConfigRaw(ctx, inst.Node, r.VMID)
				if err != nil {
					return recoveryError(r, err)
				}
				if err := completeClone(ctx, c, inst, r, raw); err != nil {
					return err
				}
				continue
			}
			if pendingClone(r) {
				if r.Template == 0 {
					for _, vm := range inst.VMs {
						for i := 1; i <= vm.Count; i++ {
							if vm.InstanceName(i, inst.suffix) == r.Name {
								r.Template = vm.Template
							}
						}
					}
				}
				if emit != nil {
					emit(Event{Msg: fmt.Sprintf("Уточнить результат clone: %s, VMID %d", r.Name, r.VMID), Resource: r.Name, VMID: r.VMID})
				}
				if cfg.RetryClone == r.Name {
					retryMatched = true
					if emit != nil {
						emit(Event{Msg: fmt.Sprintf("Явный повтор clone %s: только сохранённый VMID %d", r.Name, r.VMID), Resource: r.Name, VMID: r.VMID})
					}
					if err := retryUnconfirmedClone(ctx, c, inst, r); err != nil {
						return err
					}
				} else if err := recoverClone(ctx, c, inst, r); err != nil {
					return err
				}
			}
		}
	}
	if !retryMatched {
		return fmt.Errorf("для %s нет неподтверждённого clone в журнале; повтор не выполнялся", cfg.RetryClone)
	}

	if action == "deploy" {
		if err := cfg.saveLayouts(); err != nil {
			return err
		}
	}
	// Resume known server tasks before issuing any new mutation, including deletion.
	for _, inst := range cfg.Instances() {
		for _, r := range inst.resources() {
			if r.UPID != "" {
				if emit != nil {
					emit(Event{Msg: "Возобновить ожидание " + r.UPID, Resource: r.Name, VMID: r.VMID})
				}
				if err := resumeTask(ctx, c, inst, r); err != nil {
					return err
				}
			}
		}
	}
	found := cfg.Only == ""
	for _, inst := range cfg.Instances() {
		if cfg.Only != "" {
			if action != "deploy" {
				return fmt.Errorf("-only поддерживается для deploy")
			}
			if len(selectedVMs(inst)) == 0 {
				continue
			}
			found = true
		}
		var p *Plan
		switch action {
		case "deploy":
			p = DeploySteps(c, inst)
		case "destroy":
			p, err = DestroyPlan(ctx, c, inst)
		case "snapshot":
			p, err = SnapshotPlan(ctx, c, inst)
		case "reset":
			p, err = ResetPlan(ctx, c, inst)
		default:
			return fmt.Errorf("неизвестное действие %q", action)
		}
		if err != nil {
			return err
		}
		prefix := ""
		if cfg.HasParticipants() {
			prefix = "[" + inst.ParticipantID() + "] "
		}
		send := func(e Event) {
			e.Msg = c.Redact(prefix + e.Msg)
			if emit != nil {
				emit(e)
			}
		}
		if err = Run(ctx, p, parallel, send); err != nil {
			return err
		}
	}
	if !found {
		return fmt.Errorf("ВМ %q не найдена", cfg.Only)
	}
	if action == "destroy" {
		return cfg.releaseEmptyLayouts()
	}
	return nil
}
func LivePlan(ctx context.Context, c *pve.Client, cfg *Config) ([]string, error) {
	if err := Bind(c, cfg); err != nil {
		return nil, err
	}
	if err := cfg.checkLayouts(); err != nil {
		return nil, err
	}
	out := cfg.InstanceSummary()
	for _, inst := range cfg.Instances() {
		info, err := c.PoolInfo(ctx, inst.Pool)
		switch {
		case pve.IsMissing(err):
			out = append(out, "СОЗДАТЬ пул "+inst.Pool)
		case err != nil:
			return nil, err
		case info["comment"] != inst.marker():
			out = append(out, "КОНФЛИКТ пул "+inst.Pool)
		default:
			out = append(out, "БЕЗ ИЗМЕНЕНИЙ пул "+inst.Pool)
		}
		for _, n := range inst.Networks {
			_, ok, err := c.NetworkIface(ctx, inst.Node, n.Bridge)
			if err != nil {
				return nil, err
			}
			status := "СОЗДАТЬ"
			if ok {
				status = "ИСПОЛЬЗОВАТЬ"
			}
			out = append(out, status+" сеть "+n.Bridge)
		}
		members, err := c.ClusterVMs(ctx)
		if err != nil {
			return nil, err
		}
		selected := selectedVMs(inst)
		for _, v := range inst.VMs {
			for i := 1; i <= v.Count; i++ {
				name := v.InstanceName(i, inst.suffix)
				if !selected[name] {
					continue
				}
				r, ok := inst.record("vm", name)
				if !ok {
					conflict := false
					for _, m := range members {
						if m.Name == name && !bool(m.Template) {
							conflict = true
						}
					}
					status := "СОЗДАТЬ"
					if conflict {
						status = "КОНФЛИКТ"
					}
					out = append(out, fmt.Sprintf("%s ВМ %s из %d", status, name, v.Template))
					continue
				}
				raw, err := checkVM(ctx, c, inst, r)
				if pve.IsMissing(err) {
					out = append(out, "СОЗДАТЬ заново "+name)
					continue
				}
				if err != nil {
					out = append(out, "КОНФЛИКТ "+name+": "+err.Error())
					continue
				}
				changes := configChanges(desiredVM(v, raw, inst.Tag()), raw)
				if len(changes) == 0 {
					out = append(out, fmt.Sprintf("БЕЗ ИЗМЕНЕНИЙ ВМ %s (%d)", name, r.VMID))
				} else {
					out = append(out, fmt.Sprintf("ИЗМЕНИТЬ ВМ %s (%d): %s", name, r.VMID, strings.Join(changes, ", ")))
				}
				if v.Start {
					st, err := c.VMStatus(ctx, inst.Node, r.VMID)
					if err != nil {
						return nil, err
					}
					if st != "running" {
						out = append(out, "ЗАПУСТИТЬ "+name)
					}
				}
			}
		}
		for _, u := range inst.Users {
			raw, err := c.UserInfo(ctx, u.ID)
			switch {
			case pve.IsMissing(err):
				out = append(out, "СОЗДАТЬ пользователя "+u.ID)
			case err != nil:
				return nil, err
			case raw["comment"] != inst.marker():
				out = append(out, "КОНФЛИКТ пользователя "+u.ID)
			default:
				out = append(out, "ПРОВЕРИТЬ роль "+u.Role+" пользователя "+u.ID)
			}
		}
	}
	return out, nil
}

// OwnedResourcePreview lists non-VM records as candidates; execution verifies remote markers again.
func OwnedResourcePreview(cfg *Config) []string {
	var out []string
	for _, r := range cfg.resources() {
		if r.Kind == "vm" || r.Kind == "task" {
			continue
		}
		out = append(out, fmt.Sprintf("Проверить и удалить принадлежащий журналу ресурс: %s %s", r.Kind, r.Name))
	}
	sort.Strings(out)
	return out
}
