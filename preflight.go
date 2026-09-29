package stand

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"pvestand/internal/pve"
)

// PreflightReport — результат проверок перед развёртыванием. Errors —
// блокирующие проблемы (нельзя разворачивать, пока не исправлено),
// Warnings — то, на что стоит обратить внимание, но что не мешает продолжить.
type PreflightReport struct {
	Errors   []string
	Warnings []string
}

// OK сообщает, можно ли продолжать развёртывание (нет ошибок).
func (r *PreflightReport) OK() bool { return len(r.Errors) == 0 }

// Preflight проверяет доступность узла, наличие и корректность шаблонов,
// свободность бриджей и — приблизительно — место на хранилищах, до начала
// развёртывания. copies — сколько копий стенда будет развёрнуто
// (количество участников); для обычного стенда передайте 1.
func Preflight(ctx context.Context, c *pve.Client, cfg *Config, copies int) *PreflightReport {
	r := &PreflightReport{}
	if copies < 1 {
		copies = 1
	}

	if err := c.NodeOnline(ctx, cfg.Node); err != nil {
		r.Errors = append(r.Errors, fmt.Sprintf("узел %q недоступен: %v", cfg.Node, err))
		return r // без узла остальные проверки бессмысленны
	}

	// Сгруппируем ВМ по используемому шаблону, чтобы не опрашивать один и
	// тот же шаблон повторно и правильно посчитать нужное место на диске.
	templateInstances := map[int]int{}
	templateNames := map[int][]string{}
	for _, vm := range cfg.VMs {
		templateInstances[vm.Template] += vm.Count
		templateNames[vm.Template] = append(templateNames[vm.Template], vm.Name)
	}

	need := map[string]int64{} // storage -> примерно нужно байт
	for vmid, count := range templateInstances {
		names := strings.Join(templateNames[vmid], ", ")
		cfgMap, err := c.VMConfigRaw(ctx, cfg.Node, vmid)
		if err != nil {
			if pve.IsMissing(err) {
				r.Errors = append(r.Errors, fmt.Sprintf("шаблон %d (для %s) не найден на узле %s", vmid, names, cfg.Node))
			} else {
				r.Errors = append(r.Errors, fmt.Sprintf("шаблон %d (для %s): %v", vmid, names, err))
			}
			continue
		}
		if cfgMap["template"] != "1" {
			r.Warnings = append(r.Warnings, fmt.Sprintf("VMID %d (для %s) не отмечен как шаблон в Proxmox — клонирование может занять больше времени и места, чем ожидается", vmid, names))
		}
		for k, v := range cfgMap {
			if !diskKeyRe.MatchString(k) {
				continue
			}
			storage, size, ok := parseDiskRef(v)
			if !ok {
				continue
			}
			factor := int64(count) * int64(copies)
			if cfg.FullClone {
				need[storage] += size * factor
			} else {
				// Связанные клоны почти не занимают места на старте —
				// грубая оценка ~10% от размера диска шаблона на копию.
				need[storage] += size / 10 * factor
			}
		}
	}

	// Бриджи: если бридж уже существует, убеждаемся, что это действительно
	// бридж и что он не подключён к «боевому» физическому интерфейсу —
	// иначе развёртывание рискует задеть существующую сеть узла.
	for _, n := range cfg.Networks {
		fields, ok, err := c.NetworkIface(ctx, cfg.Node, n.Bridge)
		if err != nil {
			r.Warnings = append(r.Warnings, fmt.Sprintf("не удалось проверить бридж %s: %v", n.Bridge, err))
			continue
		}
		if !ok {
			continue // будет создан — свободен
		}
		if fields["type"] != "bridge" {
			r.Errors = append(r.Errors, fmt.Sprintf("интерфейс %s уже существует и не является бриджем (type=%s) — конфликт имени", n.Bridge, fields["type"]))
			continue
		}
		if ports := fields["bridge_ports"]; ports != "" && ports != "none" {
			r.Warnings = append(r.Warnings, fmt.Sprintf("бридж %s уже существует и подключён к %s — убедитесь, что это не боевая сеть узла", n.Bridge, ports))
		}
	}

	// Место на хранилищах.
	for storage, bytes := range need {
		st, err := c.StorageStatus(ctx, cfg.Node, storage)
		if err != nil {
			r.Warnings = append(r.Warnings, fmt.Sprintf("не удалось проверить место на хранилище %s: %v", storage, err))
			continue
		}
		if st.Avail > 0 && bytes > st.Avail {
			r.Errors = append(r.Errors, fmt.Sprintf("хранилище %s: нужно примерно %s, доступно %s", storage, humanBytes(bytes), humanBytes(st.Avail)))
		}
	}

	return r
}

var (
	diskKeyRe = regexp.MustCompile(`^(scsi|virtio|ide|sata)\d+$`)
	sizeRe    = regexp.MustCompile(`size=(\d+)([KMGT])?`)
)

// parseDiskRef разбирает значение вида "local-lvm:vm-9000-disk-0,size=32G".
func parseDiskRef(v string) (storage string, sizeBytes int64, ok bool) {
	parts := strings.SplitN(v, ":", 2)
	if len(parts) != 2 {
		return "", 0, false
	}
	storage = parts[0]
	m := sizeRe.FindStringSubmatch(v)
	if m == nil {
		return storage, 0, true
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return storage, 0, true
	}
	mult := int64(1)
	switch m[2] {
	case "K":
		mult = 1 << 10
	case "M":
		mult = 1 << 20
	case "G":
		mult = 1 << 30
	case "T":
		mult = 1 << 40
	}
	return storage, n * mult, true
}

func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d Б", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}
