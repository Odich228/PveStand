package stand

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

type L2Endpoint struct {
	VM  string `json:"vm"`
	NIC int    `json:"nic"`
}
type L2Stand struct {
	Participant string                       `json:"participant"`
	Scope       string                       `json:"scope"`
	Ports       map[string]map[string]string `json:"ports"`
	Links       map[string][]L2Endpoint      `json:"links"`
	Masks       map[string]uint64            `json:"masks"`
}
type L2Manifest struct {
	Version int                  `json:"version"`
	Nodes   map[string][]L2Stand `json:"nodes"`
}

func BuildL2Manifest(cfg *Config) (*L2Manifest, error) {
	out := &L2Manifest{Version: 1, Nodes: map[string][]L2Stand{}}
	for _, inst := range cfg.Instances() {
		digest := sha256.Sum256([]byte(inst.scope()))
		stand := L2Stand{Participant: inst.suffix, Scope: hex.EncodeToString(digest[:8]), Ports: map[string]map[string]string{}, Links: map[string][]L2Endpoint{}, Masks: map[string]uint64{}}
		internal := map[string]bool{}
		for _, network := range inst.Networks {
			internal[network.Bridge] = true
			stand.Links[network.Bridge] = []L2Endpoint{}
		}
		for _, vm := range inst.VMs {
			for i := 1; i <= vm.Count; i++ {
				name := vm.InstanceName(i, inst.suffix)
				stand.Ports[name] = map[string]string{}
				for nic, port := range vm.NICs {
					stand.Ports[name][fmt.Sprintf("net%d", nic)] = port.Bridge
					if internal[port.Bridge] {
						if port.VLAN != 0 {
							return nil, fmt.Errorf("%s: для прозрачной карты L2 требуется отсутствие VLAN на %s", name, port.Bridge)
						}
						stand.Links[port.Bridge] = append(stand.Links[port.Bridge], L2Endpoint{VM: name, NIC: nic})
					}
				}
			}
		}
		pairs := map[string]int{}
		bridgePairs := map[string]string{}
		for bridge, endpoints := range stand.Links {
			if len(endpoints) != 2 || endpoints[0].VM == endpoints[1].VM {
				return nil, fmt.Errorf("%s: мост %s должен соединять ровно две разные ВМ", inst.Name, bridge)
			}
			names := []string{endpoints[0].VM, endpoints[1].VM}
			sort.Strings(names)
			pair := names[0] + "/" + names[1]
			pairs[pair]++
			bridgePairs[bridge] = pair
		}
		for bridge, pair := range bridgePairs {
			stand.Masks[bridge] = 0x4000
			if pairs[pair] > 1 {
				stand.Masks[bridge] |= 4
			}
		}
		out.Nodes[inst.Node] = append(out.Nodes[inst.Node], stand)
	}
	return out, nil
}
func WriteL2Manifest(path string, cfg *Config) error {
	data, err := BuildL2Manifest(cfg)
	if err != nil {
		return err
	}
	raw, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".l2-manifest-*")
	if err != nil {
		return err
	}
	temp := file.Name()
	defer os.Remove(temp)
	if _, err = file.Write(append(raw, '\n')); err != nil {
		file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	return os.Rename(temp, path)
}
