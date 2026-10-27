package stand

import (
	"context"
	"fmt"
	"pvestand/internal/pve"
	"strings"
	"time"
)

func pendingClone(r Resource) bool {
	return r.Kind == "vm" && r.Operation == "clone" && r.UPID == "" && (r.Stage == "intent" || r.Stage == "uncertain" || r.Stage == "waiting")
}

// Recovery never repeats POST, allocates a VMID, deletes a VM, or clears a lock.
// Without a UPID, an owned, unlocked clone with every expected disk is evidence
// of completion: QEMU's clone worker removes its lock only after copying drives.
func recoverClone(ctx context.Context, c *pve.Client, cfg *Config, r Resource) error {
	inspectCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var last error
	for {
		raw, err := c.VMConfigRaw(inspectCtx, cfg.Node, r.VMID)
		if err == nil {
			if raw["name"] != "" && raw["name"] != r.Name {
				return fmt.Errorf("VMID %d принадлежит другой ВМ; повтор clone остановлен", r.VMID)
			}
			if raw["description"] != "" && !cfg.ownedVM(raw) {
				return fmt.Errorf("VMID %d: метка владельца не совпадает; повтор clone остановлен", r.VMID)
			}
			if cfg.ownedVM(raw) && raw["name"] == r.Name && raw["lock"] == "" {
				if err := completeClone(inspectCtx, c, cfg, r, raw); err != nil {
					return err
				}
				return nil
			}
			last = fmt.Errorf("VMID %d: клонирование ещё не подтверждено, lock=%q", r.VMID, raw["lock"])
		} else if !pve.IsMissing(err) {
			return recoveryError(r, err)
		} else {
			last = err
		}
		if r.Template > 0 {
			upid, err := c.FindCloneTask(inspectCtx, cfg.Node, r.Template, r.VMID, r.RequestedAt)
			if err != nil {
				return recoveryError(r, err)
			}
			if upid != "" {
				r.UPID = upid
				r.Stage = "waiting"
				if err = cfg.put(r); err != nil {
					return err
				}
				// A discovered legitimate task can take much longer than task discovery.
				if err = resumeTask(ctx, c, cfg, r); err != nil {
					return err
				}
				raw, err = c.VMConfigRaw(ctx, cfg.Node, r.VMID)
				if err != nil {
					return recoveryError(r, err)
				}
				return completeClone(ctx, c, cfg, r, raw)
			}
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-inspectCtx.Done():
			timer.Stop()
			return recoveryError(r, fmt.Errorf("%w; последняя проверка: %v", inspectCtx.Err(), last))
		case <-timer.C:
		}
	}
}
func completeClone(ctx context.Context, c *pve.Client, cfg *Config, r Resource, raw map[string]string) error {
	if raw["name"] != r.Name || !cfg.ownedVM(raw) || raw["template"] == "1" {
		return recoveryError(r, fmt.Errorf("не подтверждены имя, владелец или тип клона"))
	}
	if raw["lock"] != "" {
		return recoveryError(r, fmt.Errorf("на ВМ остаётся lock=%q; он не снимается автоматически", raw["lock"]))
	}
	if r.Template > 0 {
		source, err := c.VMConfigRaw(ctx, cfg.Node, r.Template)
		if err != nil {
			return recoveryError(r, err)
		}
		if source["template"] != "1" {
			return recoveryError(r, fmt.Errorf("не удалось подтвердить исходный шаблон %d", r.Template))
		}
		for key, value := range source {
			if diskKeyRe.MatchString(key) && !strings.HasPrefix(value, "none") && raw[key] == "" {
				return recoveryError(r, fmt.Errorf("не найден диск/привод %s из шаблона %d", key, r.Template))
			}
		}
	}
	r.Stage = "done"
	r.UPID = ""
	return cfg.put(r)
}
func recoveryError(r Resource, err error) error {
	return fmt.Errorf("%s: результат clone для VMID %d пока не подтверждён; новый клон не создавался. Проверьте задачу в Proxmox и соединение, затем повторите запуск: %w", r.Name, r.VMID, err)
}

// retryUnconfirmedClone is invoked only for explicit -retry-clone NAME.
// It never allocates a new VMID. The server's exclusive target configuration
// lock fences a late original request racing this one; ownership is rechecked.
func retryUnconfirmedClone(ctx context.Context, c *pve.Client, cfg *Config, r Resource) error {
	var spec VM
	found := false
	for _, vm := range cfg.VMs {
		for i := 1; i <= vm.Count; i++ {
			if vm.InstanceName(i, cfg.suffix) == r.Name {
				spec = vm
				found = true
			}
		}
	}
	if !found || r.VMID <= 0 || spec.Template != r.Template {
		return fmt.Errorf("%s: невозможно подтвердить параметры исходного clone; повтор не выполнен", r.Name)
	}
	_, err := c.VMConfigRaw(ctx, cfg.Node, r.VMID)
	if err == nil {
		return recoverClone(ctx, c, cfg, r)
	}
	if !pve.IsMissing(err) {
		return recoveryError(r, err)
	}
	upid, err := c.FindCloneTask(ctx, cfg.Node, r.Template, r.VMID, r.RequestedAt)
	if err != nil {
		return recoveryError(r, err)
	}
	if upid != "" {
		return recoverClone(ctx, c, cfg, r)
	}
	// Check again immediately before submission; a foreign occupant is never adopted.
	_, err = c.VMConfigRaw(ctx, cfg.Node, r.VMID)
	if err == nil {
		return recoverClone(ctx, c, cfg, r)
	}
	if !pve.IsMissing(err) {
		return recoveryError(r, err)
	}
	err = task(ctx, c, cfg, r, "clone", func() (string, error) {
		return c.CloneOwned(ctx, cfg.Node, spec.Template, r.VMID, r.Name, cfg.Pool, spec.Storage, cfg.marker(), cfg.FullClone)
	})
	if err == nil {
		return nil
	}
	if pve.IDConflict(err) {
		r.Stage = "uncertain"
		r.UPID = ""
		if e := cfg.put(r); e != nil {
			return e
		}
		return recoverClone(ctx, c, cfg, r)
	}
	if saved, ok := cfg.record("vm", r.Name); ok && pendingClone(saved) {
		return recoverClone(ctx, c, cfg, saved)
	}
	return err
}
