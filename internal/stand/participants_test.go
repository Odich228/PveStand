package stand

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExplicitBridgesSurviveReorderAndParticipantSelection(t *testing.T) {
	cfg, err := loadText(t, minimal+"    nics:\n      - bridge: vmbr101\n      - bridge: vmbr0\nnetworks:\n  - bridge: vmbr101\nparticipants:\n  - id: A0\n    bridge_base: 1001\n  - id: A1\n    bridge_base: 1101\n")
	if err != nil {
		t.Fatal(err)
	}
	original := cfg.Instances()[1]
	cfg.Participants[0], cfg.Participants[1] = cfg.Participants[1], cfg.Participants[0]
	if err = cfg.SelectParticipant("A1"); err != nil {
		t.Fatal(err)
	}
	inst := cfg.Instances()[0]
	if inst.Pool != "pool-A1" || inst.VMs[0].InstanceName(1, inst.suffix) != "gw-A1" || inst.Networks[0].Bridge != original.Networks[0].Bridge || inst.VMs[0].NICs[1].Bridge != "vmbr0" {
		t.Fatal(inst)
	}
	if err = cfg.SelectParticipant("missing"); err == nil {
		t.Fatal("invalid selector accepted")
	}
}
func TestNodeTemplatesAreResolvedWithoutChangingOriginal(t *testing.T) {
	cfg, err := loadText(t, minimal+"participants:\n  - id: A0\n  - id: A1\n    node: second\nnodes:\n  second:\n    templates: {1001: 2001}\n")
	if err != nil {
		t.Fatal(err)
	}
	inst := cfg.Instances()[1]
	if inst.Node != "second" || inst.VMs[0].Template != 2001 || inst.Instances()[0].VMs[0].Template != 2001 || cfg.VMs[0].Template != 1001 {
		t.Fatal(inst)
	}
}
func TestInvalidParticipantPlacements(t *testing.T) {
	for name, text := range map[string]string{
		"overlap":              minimal + "networks:\n  - bridge: vmbr101\nparticipants:\n  - id: A0\n    bridge_base: 1001\n  - id: A1\n    bridge_base: 1001\n",
		"missing template map": minimal + "participants:\n  - id: A1\n    node: second\n",
		"bad node":             minimal + "participants:\n  - id: A1\n    node: second/evil\n",
		"too high bridge":      minimal + "networks:\n  - bridge: vmbr101\n  - bridge: vmbr102\nparticipants:\n  - id: A1\n    bridge_base: 9999\n",
		"unknown template":     minimal + "nodes:\n  n:\n    templates: {9000: 2001}\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := loadText(t, text); err == nil {
				t.Fatal("invalid placement accepted")
			}
		})
	}
}
func TestLoadStructureDoesNotReadSecrets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	os.WriteFile(path, []byte(minimal+"users:\n  - id: a@pve\n    password_file: missing.secret\n"), 0600)
	if _, err := LoadStructure(path); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("runtime accepted missing secret")
	}
}
func TestL2ManifestContainsOnlyTopologyAndDetectsParallelCables(t *testing.T) {
	cfg, err := loadText(t, "name: lab\nnode: n\npool: lab\nnetworks:\n  - bridge: vmbr101\n  - bridge: vmbr102\nvms:\n  - name: left\n    template: 1001\n    nics:\n      - bridge: vmbr101\n      - bridge: vmbr102\n  - name: right\n    template: 1001\n    nics:\n      - bridge: vmbr101\n      - bridge: vmbr102\nparticipants:\n  - id: A0\n    bridge_base: 1001\n    users:\n      - id: a@pve\n        password: Secret123!\n")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "manifest.json")
	if err = WriteL2Manifest(path, cfg); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "Secret123") || strings.Contains(string(raw), "a@pve") {
		t.Fatal("credentials leaked")
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatal(info.Mode())
	}
	manifest, err := BuildL2Manifest(cfg)
	if err != nil {
		t.Fatal(err)
	}
	stand := manifest.Nodes["n"][0]
	if stand.Masks["vmbr1001"] != 0x4004 || len(stand.Ports) != 2 {
		t.Fatal(stand)
	}
}

func TestSelectedParticipantNeedsOnlyItsOwnSecret(t *testing.T) {
	t.Setenv("ONLY_A0_PASSWORD", "Secret123!")
	path := filepath.Join(t.TempDir(), "config.yaml")
	os.WriteFile(path, []byte(minimal+"participants:\n  - id: A0\n    users:\n      - id: a0@pve\n        password_env: ONLY_A0_PASSWORD\n  - id: A1\n    users:\n      - id: a1@pve\n        password_env: ABSENT_A1_PASSWORD\n"), 0600)
	cfg, err := LoadForParticipant(path, "A0")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Instances()) != 1 || cfg.Instances()[0].Users[0].Password != "Secret123!" {
		t.Fatal(cfg)
	}
	if _, err = Load(path); err == nil {
		t.Fatal("all participants accepted unresolved password")
	}
}

func TestEmptyResolvedPasswordCannotPassRuntimeLoad(t *testing.T) {
	t.Setenv("EMPTY_PARTICIPANT_PASSWORD", "")
	_, err := loadText(t, minimal+"users:\n  - id: a@pve\n    password_env: EMPTY_PARTICIPANT_PASSWORD\n")
	if err == nil {
		t.Fatal("empty password accepted")
	}
}

func TestClusterWideTemplateIDCannotBelongToTwoNodes(t *testing.T) {
	_, err := loadText(t, minimal+"participants:\n  - id: A0\n  - id: A1\n    node: second\nnodes:\n  second:\n    templates: {1001: 1001}\n")
	if err == nil || !strings.Contains(err.Error(), "разных узлов") {
		t.Fatal(err)
	}
}
