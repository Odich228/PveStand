package stand

import (
	"fmt"
	"sort"
)

// NodeSettings maps topology template IDs to cluster-wide IDs local to this node.
type NodeSettings struct {
	Templates map[int]int `yaml:"templates"`
	Storage   string      `yaml:"storage"`
}

func (c *Config) SelectParticipant(id string) error {
	if id == "all" {
		id = ""
	}
	if id != "" {
		found := false
		for _, p := range c.Participants {
			if p.ID == id {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("участник %q не найден в participants", id)
		}
	}
	c.ParticipantOnly = id
	return nil
}
func (c *Config) Instances() []*Config {
	all := c.allInstances()
	if c.ParticipantOnly == "" {
		return all
	}
	var out []*Config
	for _, inst := range all {
		if inst.suffix == c.ParticipantOnly {
			out = append(out, inst)
		}
	}
	return out
}
func (c *Config) bridgeMin() int {
	min := 10000
	for _, n := range c.Networks {
		value, ok := bridgeNum(n.Bridge)
		if ok && value < min {
			min = value
		}
	}
	if min == 10000 {
		return 0
	}
	return min
}
func (c *Config) validatePlacements() error {
	usedTemplates := map[int]bool{}
	for _, v := range c.VMs {
		usedTemplates[v.Template] = true
	}
	for node, settings := range c.Nodes {
		if !idRe.MatchString(node) {
			return fmt.Errorf("nodes: недопустимый узел %q", node)
		}
		if settings.Storage != "" && (!idRe.MatchString(settings.Storage) || !c.FullClone) {
			return fmt.Errorf("nodes.%s.storage требует full_clone: true и корректное имя хранилища", node)
		}
		for source, target := range settings.Templates {
			if !usedTemplates[source] || target <= 0 {
				return fmt.Errorf("nodes.%s.templates: неизвестный template %d или неверный целевой VMID %d", node, source, target)
			}
		}
	}
	for _, p := range c.Participants {
		if p.Node != "" && !idRe.MatchString(p.Node) {
			return fmt.Errorf("участник %s: недопустимый node", p.ID)
		}
		if p.BridgeBase < 0 || p.BridgeBase > 9999 || (p.BridgeBase > 0 && len(c.Networks) == 0) {
			return fmt.Errorf("участник %s: bridge_base должен быть 1..9999 и требует networks", p.ID)
		}
		if p.Node != "" && p.Node != c.Node {
			for template := range usedTemplates {
				if _, ok := c.Nodes[p.Node].Templates[template]; !ok {
					return fmt.Errorf("участник %s: задайте nodes.%s.templates для исходного шаблона %d на другом узле", p.ID, p.Node, template)
				}
			}
		}
	}
	templateNodes := map[int]string{}
	networks := map[string]string{}
	external := map[string]bool{}
	own := map[string]bool{}
	for _, n := range c.Networks {
		own[n.Bridge] = true
	}
	for _, v := range c.VMs {
		for _, nic := range v.NICs {
			if !own[nic.Bridge] {
				external[nic.Bridge] = true
			}
		}
	}
	for _, inst := range c.allInstances() {
		for _, vm := range inst.VMs {
			if prev, ok := templateNodes[vm.Template]; ok && prev != inst.Node {
				return fmt.Errorf("template VMID %d указан для разных узлов %s и %s; нужны локальные шаблоны с разными VMID", vm.Template, prev, inst.Node)
			}
			templateNodes[vm.Template] = inst.Node
		}
		for _, n := range inst.Networks {
			if external[n.Bridge] {
				return fmt.Errorf("участник %s: внутренняя сеть %s пересекается с внешней", inst.suffix, n.Bridge)
			}
			key := inst.Node + "/" + n.Bridge
			if prev, ok := networks[key]; ok {
				return fmt.Errorf("мост %s на %s пересекается у участников %s и %s", n.Bridge, inst.Node, prev, inst.suffix)
			}
			networks[key] = inst.suffix
		}
	}
	return nil
}

// InstanceSummary is deterministic and never contains credentials.
func (c *Config) InstanceSummary() []string {
	var lines []string
	for _, inst := range c.Instances() {
		count, memory := 0, 0
		bridges := []string{}
		for _, v := range inst.VMs {
			count += v.Count
			memory += v.Count * v.Memory
		}
		for _, n := range inst.Networks {
			bridges = append(bridges, n.Bridge)
		}
		sort.Strings(bridges)
		who := inst.suffix
		if who == "" {
			who = inst.Name
		}
		lines = append(lines, fmt.Sprintf("[%s] узел %s, пул %s: %d ВМ, задано %.1f ГиБ RAM, мосты %v", who, inst.Node, inst.Pool, count, float64(memory)/1024, bridges))
	}
	return lines
}
