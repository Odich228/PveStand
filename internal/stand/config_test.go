package stand

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const minimal = "name: demo\nnode: n\npool: pool\nvms:\n  - name: gw\n    template: 1001\n    start: true\n"

func loadText(t *testing.T, text string) (*Config, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "stand.yaml")
	if err := os.WriteFile(p, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	return Load(p)
}
func TestConfigValidation(t *testing.T) {
	for _, tc := range []struct{ name, text, fragment string }{
		{"multi", minimal + "---\nname: second\n", "один"},
		{"unknown", minimal + "typo: true\n", "field typo"},
		{"ram", minimal + "    memory: -1\n", "memory"},
		{"vlan", minimal + "    nics:\n      - bridge: vmbr0\n        vlan: 4095\n", "vlan"},
		{"cycle", minimal + "    depends_on: [gw]\n", "цикл"},
		{"missing dependency", minimal + "    depends_on: [unknown]\n", "не найдена"},
		{"readiness", minimal + "    ready: tcp\n    ready_address: invalid\n", "host:port"},
		{"linked storage", minimal + "    storage: local-lvm\n", "full_clone"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadText(t, tc.text)
			if err == nil || !strings.Contains(err.Error(), tc.fragment) {
				t.Fatalf("%v", err)
			}
		})
	}
}
func TestIndentHintDoesNotExposePassword(t *testing.T) {
	text := minimal + " - name: wrong\n    template: 2\nusers:\n  - id: a@pve\n    password: very-secret\n"
	_, err := loadText(t, text)
	if err == nil || !strings.Contains(err.Error(), "строке 8") || strings.Contains(err.Error(), "very-secret") {
		t.Fatal(err)
	}
}
func TestSecretReferencesAndDuplicates(t *testing.T) {
	t.Setenv("TEST_STUDENT_PASS", "Secret123!")
	cfg, err := loadText(t, minimal+"users:\n  - id: a@pve\n    password_env: TEST_STUDENT_PASS\n")
	if err != nil || cfg.Users[0].Password != "Secret123!" {
		t.Fatal(cfg, err)
	}
	_, err = loadText(t, minimal+"users:\n  - id: a@pve\n    password: Secret123!\n    password_env: TEST_STUDENT_PASS\n")
	if err == nil {
		t.Fatal("ambiguous secret accepted")
	}
}
func TestParticipantCopiesRemainIndependent(t *testing.T) {
	cfg, err := loadText(t, minimal+"    nics:\n      - bridge: vmbr101\nnetworks:\n  - bridge: vmbr101\nparticipants:\n  - id: one\n  - id: two\n")
	if err != nil {
		t.Fatal(err)
	}
	instances := cfg.Instances()
	if instances[1].VMs[0].NICs[0].Bridge != "vmbr102" || cfg.VMs[0].NICs[0].Bridge != "vmbr101" {
		t.Fatal(instances)
	}
}
func TestNICOptionsPreserveMACAndRemoveExtraNIC(t *testing.T) {
	v := VM{NICs: []NIC{{Bridge: "vmbr101", Model: "virtio", VLAN: 123}}, ReplaceNICs: true}
	p := desiredVM(v, map[string]string{"net0": "e1000=AA:BB:CC:DD:EE:FF,bridge=vmbr0,firewall=1", "net1": "virtio=XX,bridge=vmbr0", "tags": "existing"}, "stand")
	if p.Get("net0") != "virtio=AA:BB:CC:DD:EE:FF,bridge=vmbr101,tag=123,firewall=1" || p.Get("delete") != "net1" || p.Get("tags") != "existing;stand" {
		t.Fatal(p)
	}
}
func TestSecretFilePermissions(t *testing.T) {
	p := filepath.Join(t.TempDir(), "secret")
	os.WriteFile(p, []byte("password"), 0644)
	if _, err := ReadSecret(p); err == nil {
		t.Fatal("public secret accepted")
	}
	os.Chmod(p, 0600)
	if s, err := ReadSecret(p); err != nil || s != "password" {
		t.Fatal(s, err)
	}
}
func TestDiskEstimateDecimal(t *testing.T) {
	_, size, ok := parseDiskRef("local-lvm:vm-100-disk-0,size=1.5G")
	if !ok || size != 1610612736 {
		t.Fatal(size, ok)
	}
}

func TestParticipantNetworkDoesNotOverlapExternalBridge(t *testing.T) {
	_, err := loadText(t, minimal+"    nics:\n      - bridge: vmbr101\n      - bridge: vmbr102\nnetworks:\n  - bridge: vmbr101\nparticipants:\n  - id: one\n  - id: two\n")
	if err == nil || !strings.Contains(err.Error(), "внешней") {
		t.Fatal(err)
	}
}
func TestProfilesResolveRelativeSecretPaths(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "profiles.yaml")
	os.WriteFile(p, []byte("profiles:\n  lab:\n    host: pve.test\n    token_file: pve.secret\n    state_dir: state\n"), 0600)
	profile, err := LoadProfile(p, "lab")
	if err != nil || profile.TokenFile != filepath.Join(dir, "pve.secret") || profile.StateDir != filepath.Join(dir, "state") {
		t.Fatal(profile, err)
	}
}

func TestTransparentLabNICClearsInheritedFilteringAndLinkDown(t *testing.T) {
	no := false
	vm := VM{NICs: []NIC{{Bridge: "vmbr114", Model: "virtio", Firewall: &no, LinkDown: &no}}}
	raw := map[string]string{"net0": "virtio=AA:BB:CC:DD:EE:FF,bridge=vmbr0,firewall=1,link_down=1,tag=25,trunks=10;20"}
	p := desiredVM(vm, raw, "stand")
	props := parseProperties(p.Get("net0"))
	if props["firewall"] != "0" || props["link_down"] != "0" {
		t.Fatal(p)
	}
	if _, ok := props["tag"]; ok {
		t.Fatal(p)
	}
	if _, ok := props["trunks"]; ok {
		t.Fatal(p)
	}
}
func TestDefaultNICFlagsCompareEqual(t *testing.T) {
	raw := map[string]string{"net0": "virtio=MAC,bridge=vmbr114", "tags": "stand"}
	no := false
	vm := VM{NICs: []NIC{{Bridge: "vmbr114", Model: "virtio", Firewall: &no, LinkDown: &no}}}
	if changes := configChanges(desiredVM(vm, raw, "stand"), raw); len(changes) != 0 {
		t.Fatal(changes)
	}
}
func TestNICFlagFieldsDecode(t *testing.T) {
	cfg, err := loadText(t, minimal+"    nics:\n      - bridge: vmbr101\n        firewall: false\n        link_down: false\n")
	if err != nil {
		t.Fatal(err)
	}
	nic := cfg.VMs[0].NICs[0]
	if nic.Firewall == nil || *nic.Firewall || nic.LinkDown == nil || *nic.LinkDown {
		t.Fatal(nic)
	}
}
