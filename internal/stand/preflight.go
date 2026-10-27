package stand

import (
	"context"
	"fmt"
	"pvestand/internal/pve"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type diskBudget struct {
	Node, Storage string
	Need, Avail   int64
}
type PreflightReport struct {
	Errors, Warnings, Info []string
	disks                  []diskBudget
}

func (r *PreflightReport) OK() bool { return len(r.Errors) == 0 }

// Kept for callers of the original API. Every participant is checked individually.
func Preflight(ctx context.Context, c *pve.Client, cfg *Config, copies int) *PreflightReport {
	return Check(ctx, c, cfg)
}
func Check(ctx context.Context, c *pve.Client, cfg *Config) *PreflightReport {
	r := &PreflightReport{}
	if err := cfg.SelectParticipant(cfg.ParticipantOnly); err != nil {
		r.Errors = append(r.Errors, err.Error())
		return r
	}
	if err := Bind(c, cfg); err != nil {
		r.Errors = append(r.Errors, err.Error())
		return r
	}
	if err := cfg.checkLayouts(); err != nil {
		r.Errors = append(r.Errors, err.Error())
		return r
	}
	groups := map[string][]*Config{}
	for _, inst := range cfg.Instances() {
		groups[inst.Node] = append(groups[inst.Node], inst)
	}
	nodes := []string{}
	for node := range groups {
		nodes = append(nodes, node)
	}
	sort.Strings(nodes)
	r.Info = append(r.Info, cfg.InstanceSummary()...)
	for _, node := range nodes {
		part := checkNode(ctx, c, cfg, groups[node], node)
		r.disks = append(r.disks, part.disks...)
		for _, line := range part.Info {
			r.Info = append(r.Info, "["+node+"] "+line)
		}
		for _, line := range part.Errors {
			r.Errors = append(r.Errors, "["+node+"] "+line)
		}
		for _, line := range part.Warnings {
			r.Warnings = append(r.Warnings, "["+node+"] "+line)
		}
	}
	checkSharedStorage(ctx, c, r)
	if len(cfg.Instances()) > 1 {
		r.Warnings = append(r.Warnings, "Внутренние мосты участников разделены; внешние мосты (например vmbr0) остаются общими. Для WAN нужны согласованные адреса и правила доступа.")
	}
	return r
}

func checkNode(ctx context.Context, c *pve.Client, cfg *Config, instances []*Config, node string) *PreflightReport {
	r := &PreflightReport{}
	if err := Bind(c, cfg); err != nil {
		r.Errors = append(r.Errors, err.Error())
		return r
	}
	major, versionErr := c.MajorVersion(ctx)
	if versionErr != nil {
		r.Warnings = append(r.Warnings, "не удалось определить версию Proxmox: "+versionErr.Error())
	}
	total, free, err := c.NodeMemory(ctx, node)
	if err != nil {
		r.Errors = append(r.Errors, fmt.Sprintf("не удалось прочитать состояние узла %s: %v", node, err))
		return r
	}
	all, err := c.ClusterVMs(ctx)
	if err != nil {
		r.Errors = append(r.Errors, "список ВМ: "+err.Error())
		return r
	}
	templates := map[int]map[string]string{}
	var ram uint64
	need := map[string]int64{}
	checkedPaths := map[string]map[string]bool{}
	require := func(path string, privs ...string) {
		perms, ok := checkedPaths[path]
		if !ok {
			var e error
			perms, e = c.Permissions(ctx, path)
			if e != nil {
				r.Errors = append(r.Errors, fmt.Sprintf("проверка прав %s: %v", path, e))
				return
			}
			checkedPaths[path] = perms
		}
		for _, priv := range privs {
			if !perms[priv] {
				r.Errors = append(r.Errors, fmt.Sprintf("не хватает права %s на %s (проверьте ACL пользователя и API-токена)", priv, path))
			}
		}
	}
	requireEither := func(paths []string, privs ...string) {
		for _, priv := range privs {
			granted := false
			for _, path := range paths {
				perms, ok := checkedPaths[path]
				if !ok {
					var e error
					perms, e = c.Permissions(ctx, path)
					if e != nil {
						r.Errors = append(r.Errors, fmt.Sprintf("проверка прав %s: %v", path, e))
						continue
					}
					checkedPaths[path] = perms
				}
				if perms[priv] {
					granted = true
				}
			}
			if !granted {
				r.Errors = append(r.Errors, fmt.Sprintf("не хватает %s на одном из путей %s", priv, strings.Join(paths, " или ")))
			}
		}
	}
	require("/nodes/"+node, "Sys.Audit")
	for _, inst := range instances {
		selected := selectedVMs(inst)
		if inst.Only != "" && len(selected) == 0 {
			continue
		}

		pool, pe := c.PoolInfo(ctx, inst.Pool)
		poolMissing := pve.IsMissing(pe)
		if pe != nil && !poolMissing {
			r.Errors = append(r.Errors, fmt.Sprintf("пул %s: %v", inst.Pool, pe))
		} else if pe == nil && pool["comment"] != inst.marker() {
			r.Errors = append(r.Errors, fmt.Sprintf("конфликт: пул %s создан вне этого журнала; задайте новый pool", inst.Pool))
		}
		if poolMissing {
			require("/pool/"+inst.Pool, "Pool.Allocate")
		}
		for _, n := range inst.Networks {
			fields, ok, e := c.NetworkIface(ctx, inst.Node, n.Bridge)
			if e != nil {
				r.Errors = append(r.Errors, fmt.Sprintf("сеть %s: %v", n.Bridge, e))
				continue
			}
			if !ok {
				require("/nodes/"+inst.Node, "Sys.Modify")
				continue
			}
			if inst.isParticipant() && !ownedComment(fields["comments"], inst.marker()) {
				r.Errors = append(r.Errors, fmt.Sprintf("участник %s: мост %s существует вне этого стенда; выберите другой bridge_base", inst.suffix, n.Bridge))
			}
			if fields["type"] != "bridge" {
				r.Errors = append(r.Errors, fmt.Sprintf("%s не является bridge", n.Bridge))
			}
			if ownedComment(fields["comments"], inst.marker()) {
				require("/nodes/"+inst.Node, "Sys.Modify")
			}
			if ports := fields["bridge_ports"]; ports != "" && ports != "none" {
				r.Warnings = append(r.Warnings, fmt.Sprintf("сеть %s подключена к физическим портам %s; будет использована без изменения", n.Bridge, ports))
			}
		}
		for _, u := range inst.Users {
			raw, e := c.UserInfo(ctx, u.ID)
			if e == nil && raw["comment"] != inst.marker() {
				r.Errors = append(r.Errors, "конфликт существующего пользователя "+u.ID)
			} else if e != nil && !pve.IsMissing(e) {
				r.Errors = append(r.Errors, fmt.Sprintf("пользователь %s: %v", u.ID, e))
			}
			require("/access/groups", "User.Modify")
			perms, e := c.Permissions(ctx, "/pool/"+inst.Pool)
			if e != nil {
				r.Errors = append(r.Errors, e.Error())
			} else if !perms["Permissions.Modify"] && !perms["Pool.Allocate"] {
				r.Errors = append(r.Errors, "для выдачи роли на пул "+inst.Pool+" нужно Permissions.Modify или Pool.Allocate с правами делегировать эту роль")
			}
			if major >= 9 {
				_, realm, _ := strings.Cut(u.ID, "@")
				require("/access/realm/"+realm, "Realm.AllocateUser")
			}
		}
		for _, vm := range inst.VMs {
			raw, ok := templates[vm.Template]
			if !ok {
				var e error
				raw, e = c.VMConfigRaw(ctx, inst.Node, vm.Template)
				if e != nil {
					r.Errors = append(r.Errors, fmt.Sprintf("шаблон %d для %s: %v", vm.Template, vm.Name, e))
					continue
				}
				templates[vm.Template] = raw
				if raw["template"] != "1" {
					r.Errors = append(r.Errors, fmt.Sprintf("VMID %d не отмечен как шаблон", vm.Template))
				}
				if raw["lock"] != "" {
					r.Errors = append(r.Errors, fmt.Sprintf("шаблон %d заблокирован: %s", vm.Template, raw["lock"]))
				}
				for key := range raw {
					if strings.HasPrefix(key, "hostpci") || strings.HasPrefix(key, "usb") || key == "args" || key == "hugepages" {
						r.Warnings = append(r.Warnings, fmt.Sprintf("шаблон %d наследует %s: проверьте доступность устройства/ресурса на %s", vm.Template, key, inst.Node))
					}
				}
				cpu := vm.CPU
				if cpu == "" {
					cpu = raw["cpu"]
				}
				if cpu != "" {
					r.Info = append(r.Info, fmt.Sprintf("шаблон %d: CPU %s; совместимость флагов проверяется при запуске QEMU", vm.Template, cpu))
				}
			}
			if vm.Ready == "agent" && raw["agent"] != "1" && !strings.Contains(raw["agent"], "enabled=1") {
				r.Errors = append(r.Errors, fmt.Sprintf("%s: ready: agent требует включённого qemu-guest-agent в шаблоне %d", vm.Name, vm.Template))
			}
			for i := 1; i <= vm.Count; i++ {
				name := vm.InstanceName(i, inst.suffix)
				if !selected[name] {
					continue
				}
				existing := false
				effective := raw
				running := false
				if rec, found := inst.record("vm", name); found {
					if rec.Operation == "clone" && (pendingClone(rec) || rec.UPID != "") {
						existing = true // Recovery validates ownership before any other mutation.
						_, probeErr := c.VMConfigRaw(ctx, inst.Node, rec.VMID)
						if pve.IsMissing(probeErr) {
							existing = false
						} else if probeErr != nil {
							r.Errors = append(r.Errors, probeErr.Error())
						}
						r.Warnings = append(r.Warnings, fmt.Sprintf("%s: VMID %d, ответ clone потерян; перед изменениями программа уточнит результат по ВМ и задаче", name, rec.VMID))
					} else {
						current, e := checkVM(ctx, c, inst, rec)
						if e == nil {
							existing = true
							effective = current
							status, e := c.VMStatus(ctx, inst.Node, rec.VMID)
							if e != nil {
								r.Errors = append(r.Errors, e.Error())
							}
							running = status == "running"
							if current["lock"] != "" && rec.UPID == "" {
								r.Errors = append(r.Errors, fmt.Sprintf("VMID %d заблокирована: %s", rec.VMID, current["lock"]))
							}
						} else if !pve.IsMissing(e) {
							r.Errors = append(r.Errors, e.Error())
						}
					}
				}
				if !existing {
					for _, m := range all {
						if m.Name == name && !bool(m.Template) {
							r.Errors = append(r.Errors, fmt.Sprintf("конфликт имени %s: VMID %d не принадлежит журналу", name, m.VMID))
						}
					}
					require(fmt.Sprintf("/vms/%d", vm.Template), "VM.Audit", "VM.Clone")
					requireEither([]string{"/vms", "/pool/" + inst.Pool}, "VM.Allocate")
					for key, value := range raw {
						if !diskKeyRe.MatchString(key) {
							continue
						}
						storage, size, ok := parseDiskRef(value)
						if !ok {
							continue
						}
						if vm.Storage != "" {
							storage = vm.Storage
						}
						if !inst.FullClone {
							size /= 10
						}
						need[storage] += size
						require("/storage/"+storage, "Datastore.Audit", "Datastore.AllocateSpace")
					}
				}
				targetPath := "/vms"
				if rec, found := inst.record("vm", name); found && existing {
					targetPath = fmt.Sprintf("/vms/%d", rec.VMID)
				}
				privileges := []string{"VM.Config.Network", "VM.Config.Options"}
				if vm.Cores > 0 || vm.Sockets > 0 || vm.CPU != "" {
					privileges = append(privileges, "VM.Config.CPU")
				}
				if vm.Memory > 0 {
					privileges = append(privileges, "VM.Config.Memory")
				}
				if vm.Start {
					privileges = append(privileges, "VM.PowerMgmt")
				}
				if targetPath == "/vms" {
					requireEither([]string{targetPath, "/pool/" + inst.Pool}, privileges...)
				} else {
					require(targetPath, privileges...)
				}
				if vm.Start && !running {
					mb := vm.Memory
					if mb == 0 {
						mb, _ = strconv.Atoi(effective["memory"])
					}
					ram += uint64(mb) * 1024 * 1024
				}
				known := map[string]bool{}
				for _, n := range inst.Networks {
					known[n.Bridge] = true
				}
				for _, nic := range vm.NICs {
					path := "/sdn/zones/localnetwork/" + nic.Bridge
					if nic.VLAN > 0 {
						path += fmt.Sprintf("/%d", nic.VLAN)
					}
					require(path, "SDN.Use")
					if known[nic.Bridge] {
						continue
					}
					_, ok, e := c.NetworkIface(ctx, inst.Node, nic.Bridge)
					if e != nil || !ok {
						r.Errors = append(r.Errors, fmt.Sprintf("ВМ %s: внешняя сеть %s недоступна: %v", name, nic.Bridge, e))
					}
				}
				if !vm.ReplaceNICs {
					for key := range raw {
						if strings.HasPrefix(key, "net") {
							index, e := strconv.Atoi(strings.TrimPrefix(key, "net"))
							if e == nil && index >= len(vm.NICs) {
								r.Warnings = append(r.Warnings, fmt.Sprintf("%s наследует %s шаблона; replace_nics: true удалит лишние интерфейсы", name, key))
							}
						}
					}
				}
			}
		}
	}
	r.Info = append(r.Info, fmt.Sprintf("RAM для ещё не запущенных ВМ: %s; свободно: %s из %s", humanBytes(int64(ram)), humanBytes(int64(free)), humanBytes(int64(total))))
	if ram > free {
		r.Errors = append(r.Errors, "недостаточно свободной RAM для всех запланированных запусков")
	}
	for storage, bytes := range need {
		st, e := c.StorageStatus(ctx, node, storage)
		if e != nil {
			r.Errors = append(r.Errors, fmt.Sprintf("хранилище %s: %v", storage, e))
			continue
		}
		if (st.Active != nil && *st.Active == 0) || (st.Enabled != nil && *st.Enabled == 0) {
			r.Errors = append(r.Errors, "хранилище "+storage+" не активно или выключено")
		}
		r.disks = append(r.disks, diskBudget{Node: node, Storage: storage, Need: bytes, Avail: st.Avail})
		if st.Total == 0 {
			r.Errors = append(r.Errors, "хранилище "+storage+" не сообщает доступный объём")
		}
		if bytes > st.Avail {
			r.Errors = append(r.Errors, fmt.Sprintf("хранилище %s: нужно примерно %s, свободно %s", storage, humanBytes(bytes), humanBytes(st.Avail)))
		}
		r.Info = append(r.Info, fmt.Sprintf("%s: оценка новых дисков %s, свободно %s", storage, humanBytes(bytes), humanBytes(st.Avail)))
	}
	r.Warnings = append(r.Warnings, "Оценка дисков связанных клонов приблизительная (10%); расход может расти после запуска. CPU, ISO и passthrough окончательно проверяет QEMU.")
	r.Errors = unique(r.Errors)
	r.Warnings = unique(r.Warnings)
	return r
}
func unique(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

var diskKeyRe = regexp.MustCompile(`^(scsi|virtio|ide|sata)\d+$`)
var sizeRe = regexp.MustCompile(`(?:^|,)size=(\d+(?:\.\d+)?)([KMGT]?)`)

func parseDiskRef(v string) (string, int64, bool) {
	storage, _, ok := strings.Cut(v, ":")
	if !ok {
		return "", 0, false
	}
	m := sizeRe.FindStringSubmatch(v)
	if m == nil {
		return storage, 0, true
	}
	size, _ := strconv.ParseFloat(m[1], 64)
	switch m[2] {
	case "K":
		size *= 1 << 10
	case "M":
		size *= 1 << 20
	case "G":
		size *= 1 << 30
	case "T":
		size *= 1 << 40
	}
	return storage, int64(size), true
}
func humanBytes(b int64) string {
	if b < 1024 {
		return fmt.Sprintf("%d Б", b)
	}
	x := float64(b)
	units := []string{"Б", "КиБ", "МиБ", "ГиБ", "ТиБ", "ПиБ"}
	i := 0
	for x >= 1024 && i < len(units)-1 {
		x /= 1024
		i++
	}
	return fmt.Sprintf("%.1f %s", x, units[i])
}

// The same shared storage must not be counted as independently free on each node.
func checkSharedStorage(ctx context.Context, c *pve.Client, r *PreflightReport) {
	groups := map[string][]diskBudget{}
	for _, budget := range r.disks {
		groups[budget.Storage] = append(groups[budget.Storage], budget)
	}
	names := []string{}
	for name := range groups {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		budgets := groups[name]
		if len(budgets) < 2 {
			continue
		}
		shared, err := c.StorageShared(ctx, name)
		if err != nil {
			r.Errors = append(r.Errors, fmt.Sprintf("не удалось проверить общий доступ хранилища %s: %v", name, err))
			continue
		}
		if !shared {
			continue
		}
		var required int64
		available := budgets[0].Avail
		for _, budget := range budgets {
			required += budget.Need
			if budget.Avail < available {
				available = budget.Avail
			}
		}
		r.Info = append(r.Info, fmt.Sprintf("Общее хранилище %s: все узлы вместе требуют примерно %s, свободно %s", name, humanBytes(required), humanBytes(available)))
		if required > available {
			r.Errors = append(r.Errors, fmt.Sprintf("общее хранилище %s: недостаточно места для суммарного плана всех узлов", name))
		}
	}
}
