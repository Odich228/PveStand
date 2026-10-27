package stand

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
)

type Layout struct {
	Node    string   `json:"node"`
	Bridges []string `json:"bridges"`
}

func layoutOf(inst *Config) Layout {
	l := Layout{Node: inst.Node, Bridges: []string{}}
	for _, n := range inst.Networks {
		l.Bridges = append(l.Bridges, n.Bridge)
	}
	sort.Strings(l.Bridges)
	return l
}
func layoutKey(inst *Config) string { return inst.Pool + "/" + inst.Name }
func (c *Config) checkLayouts() error {
	if c.Runtime == nil {
		return nil
	}
	r := c.Runtime
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, inst := range c.Instances() {
		if !inst.isParticipant() {
			continue
		}
		haveResources := false
		expected := layoutOf(inst)
		expectedBridges := map[string]bool{}
		for _, bridge := range expected.Bridges {
			expectedBridges[bridge] = true
		}
		for _, record := range r.data.Resources {
			_, logical, ok := strings.Cut(record.Scope, "/")
			if !ok || logical != layoutKey(inst) {
				continue
			}
			haveResources = true
			if record.Node != inst.Node {
				return fmt.Errorf("стенд %s уже имеет ресурсы на %s; смена node на %s не является миграцией, верните прежнюю конфигурацию", inst.suffix, record.Node, inst.Node)
			}
			if record.Kind == "network" && !expectedBridges[record.Name] {
				return fmt.Errorf("стенд %s: изменились закреплённые мосты; сохраните прежний bridge_base до удаления стенда", inst.suffix)
			}
		}
		if stored, ok := r.data.Layouts[layoutKey(inst)]; ok && haveResources && !reflect.DeepEqual(stored, expected) {
			return fmt.Errorf("стенд %s: node или диапазон мостов отличается от журнала; используйте исходную конфигурацию", inst.suffix)
		}
	}
	return nil
}
func (c *Config) saveLayouts() error {
	r := c.Runtime
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.data.Layouts == nil {
		r.data.Layouts = map[string]Layout{}
	}
	for _, inst := range c.Instances() {
		if inst.isParticipant() {
			r.data.Layouts[layoutKey(inst)] = layoutOf(inst)
		}
	}
	return r.saveLocked()
}
func (c *Config) releaseEmptyLayouts() error {
	r := c.Runtime
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, inst := range c.Instances() {
		empty := true
		for _, record := range r.data.Resources {
			if record.Scope == inst.scope() {
				empty = false
				break
			}
		}
		if empty {
			delete(r.data.Layouts, layoutKey(inst))
		}
	}
	return r.saveLocked()
}
