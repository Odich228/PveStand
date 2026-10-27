package stand

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"pvestand/internal/pve"
	"sort"
	"strconv"
	"strings"
	"time"
)

func desiredVM(vm VM, current map[string]string, tag string) url.Values {
	p := url.Values{}
	for k, n := range map[string]int{"cores": vm.Cores, "memory": vm.Memory, "sockets": vm.Sockets} {
		if n > 0 {
			p.Set(k, strconv.Itoa(n))
		}
	}
	if vm.CPU != "" {
		p.Set("cpu", vm.CPU)
	}
	if len(vm.BootOrder) > 0 {
		p.Set("boot", "order="+strings.Join(vm.BootOrder, ";"))
	}
	for i, nic := range vm.NICs {
		key := fmt.Sprintf("net%d", i)
		model := nic.Model
		// Preserve the generated MAC and template options while overriding declared fields.
		old := parseProperties(current[key])
		if nic.Firewall != nil {
			old["firewall"] = "0"
			if *nic.Firewall {
				old["firewall"] = "1"
			}
		}
		if nic.LinkDown != nil {
			old["link_down"] = "0"
			if *nic.LinkDown {
				old["link_down"] = "1"
			}
		}
		oldModel, mac, _ := strings.Cut(strings.Split(current[key], ",")[0], "=")
		if mac != "" {
			model += "=" + mac
		}
		value := model + ",bridge=" + nic.Bridge
		if nic.VLAN > 0 {
			value += fmt.Sprintf(",tag=%d", nic.VLAN)
		}
		var keys []string
		for k := range old {
			if k != oldModel && k != "bridge" && k != "tag" && k != "trunks" && k != "virtio" && k != "e1000" && k != "rtl8139" && k != "vmxnet3" && k != "e1000e" && k != "ne2k_pci" {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			value += "," + k + "=" + old[k]
		}
		p.Set(key, value)
	}
	if vm.ReplaceNICs {
		var keys []string
		for k := range current {
			if strings.HasPrefix(k, "net") {
				if i, err := strconv.Atoi(strings.TrimPrefix(k, "net")); err == nil && i >= len(vm.NICs) {
					keys = append(keys, k)
				}
			}
		}
		sort.Strings(keys)
		if len(keys) > 0 {
			p.Set("delete", strings.Join(keys, ","))
		}
	}
	tags := current["tags"]
	if !hasTag(tags, tag) {
		if tags != "" {
			tags += ";"
		}
		tags += tag
	}
	p.Set("tags", tags)
	return p
}
func parseProperties(s string) map[string]string {
	out := map[string]string{}
	for _, p := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(p, "=")
		if ok {
			out[k] = v
		} else if p != "" {
			out[p] = ""
		}
	}
	return out
}
func configChanges(p url.Values, current map[string]string) []string {
	var changes []string
	for k, v := range p {
		if k == "delete" {
			for _, key := range strings.Split(v[0], ",") {
				changes = append(changes, key+": удалить")
			}
			continue
		}
		equal := current[k] == v[0]
		if strings.HasPrefix(k, "net") {
			a, b := parseProperties(current[k]), parseProperties(v[0])
			for _, flag := range []string{"firewall", "link_down"} {
				if a[flag] == "" {
					a[flag] = "0"
				}
				if b[flag] == "" {
					b[flag] = "0"
				}
			}
			equal = len(a) == len(b)
			for key, value := range b {
				if a[key] != value {
					equal = false
				}
			}
		}
		if !equal {
			changes = append(changes, fmt.Sprintf("%s: %s → %s", k, current[k], v[0]))
		}
	}
	sort.Strings(changes)
	return changes
}
func waitReady(ctx context.Context, c *pve.Client, cfg *Config, vm VM, id int) error {
	if vm.Ready == "" {
		return nil
	}
	seconds := vm.ReadyTimeout
	if seconds == 0 {
		seconds = 120
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(seconds)*time.Second)
	defer cancel()
	var last error
	for {
		if vm.Ready == "agent" {
			last = c.AgentReady(ctx, cfg.Node, id)
		} else {
			d := net.Dialer{Timeout: 3 * time.Second}
			var conn net.Conn
			conn, last = d.DialContext(ctx, "tcp", vm.ReadyAddress)
			if conn != nil {
				conn.Close()
			}
		}
		if last == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("VMID %d: ожидание готовности %s: %w (последняя проверка: %v)", id, vm.Ready, ctx.Err(), last)
		case <-time.After(2 * time.Second):
		}
	}
}
