package stand

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"pvestand/internal/pve"
	"strconv"
	"strings"
	"testing"
)

// This adapter models one cluster, two independent nodes and cluster-wide pools.
// All requests stay in memory; no server sockets or real credentials are used.
type multiTransport struct {
	nodes      map[string]*fakePVE
	pools      map[string]string
	membership map[int]string
	vmNodes    map[int]string
	free       map[string]uint64
	avail      int64
	shared     bool
	next       int
	operations []string
}

func (tr *multiTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	_ = r.ParseForm()
	path := strings.TrimPrefix(r.URL.Path, "/api2/json")
	if r.Method != "GET" {
		tr.operations = append(tr.operations, r.Method+" "+path)
	}
	respond := func(data any, code int) (*http.Response, error) {
		w := httptest.NewRecorder()
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(map[string]any{"data": data})
		return w.Result(), nil
	}
	members := func(pool string) []map[string]any {
		var rows []map[string]any
		for node, f := range tr.nodes {
			for id, vm := range f.vms {
				if pool != "" && tr.membership[id] != pool {
					continue
				}
				rows = append(rows, map[string]any{"type": "qemu", "vmid": id, "node": node, "name": vm["name"], "tags": vm["tags"], "template": vm["template"] == "1"})
			}
		}
		return rows
	}
	switch {
	case path == "/cluster/resources":
		return respond(members(""), 200)
	case path == "/cluster/nextid":
		id := tr.next
		tr.next++
		return respond(strconv.Itoa(id), 200)
	case path == "/pools" && r.Method == "POST":
		tr.pools[r.Form.Get("poolid")] = r.Form.Get("comment")
		return respond(nil, 200)
	case strings.HasPrefix(path, "/pools/"):
		name := strings.TrimPrefix(path, "/pools/")
		comment, ok := tr.pools[name]
		if !ok {
			return respond(nil, 404)
		}
		if r.Method == "DELETE" {
			delete(tr.pools, name)
			return respond(nil, 200)
		}
		return respond(map[string]any{"comment": comment, "members": members(name)}, 200)
	case path == "/storage":
		return respond([]map[string]any{{"storage": "local", "shared": tr.shared}}, 200)
	case strings.HasPrefix(path, "/nodes/"):
		parts := strings.Split(strings.TrimPrefix(path, "/nodes/"), "/")
		node := parts[0]
		f := tr.nodes[node]
		if f == nil {
			return respond(nil, 404)
		}
		tail := strings.Join(parts[1:], "/")
		if tail == "status" {
			return respond(map[string]any{"memory": map[string]uint64{"total": 1 << 38, "free": tr.free[node]}}, 200)
		}
		if tail == "storage/local/status" {
			return respond(map[string]any{"total": 1 << 40, "avail": tr.avail}, 200)
		}
		if strings.HasSuffix(tail, "/clone") {
			newID, _ := strconv.Atoi(r.Form.Get("newid"))
			tr.membership[newID] = r.Form.Get("pool")
			tr.vmNodes[newID] = node
		}
		if strings.HasSuffix(tail, "/snapshot/stand-clean/rollback") {
			return respond("UPID:rollback:", 200)
		}
		r.URL.Path = "/api2/json/nodes/n/" + tail
		if strings.HasPrefix(tail, "qemu/") && r.Method == "DELETE" {
			id, _ := strconv.Atoi(parts[2])
			delete(tr.membership, id)
			delete(tr.vmNodes, id)
		}
		return (fakeTransport{f}).RoundTrip(r)
	default:
		return (fakeTransport{tr.nodes["n"]}).RoundTrip(r)
	}
}
func multiFixture(t *testing.T) (*multiTransport, *pve.Client, *Config) {
	_, _, single := fixture(t)
	cfg, err := loadText(t, minimal+"    memory: 1024\n    nics:\n      - bridge: vmbr101\nnetworks:\n  - bridge: vmbr101\nparticipants:\n  - id: A0\n    bridge_base: 1001\n    users:\n      - id: a0@pve\n        password: Secret123!\n  - id: A1\n    node: second\n    bridge_base: 1101\n    users:\n      - id: a1@pve\n        password: Secret123!\nnodes:\n  second:\n    templates: {1001: 2001}\n")
	if err != nil {
		t.Fatal(err)
	}
	_ = single
	second := newFake()
	second.vms[2001] = second.vms[1001]
	delete(second.vms, 1001)
	tr := &multiTransport{nodes: map[string]*fakePVE{"n": newFake(), "second": second}, pools: map[string]string{}, membership: map[int]string{}, vmNodes: map[int]string{}, free: map[string]uint64{"n": 2 << 30, "second": 2 << 30}, avail: 1 << 39, next: 200}
	c, err := pve.NewWithTransport("http://cluster.test:8006", "token", tr)
	if err != nil {
		t.Fatal(err)
	}
	return tr, c, cfg
}
func TestMultiNodeDeployResumeAndSelectedDestroy(t *testing.T) {
	tr, c, cfg := multiFixture(t)
	if err := Execute(context.Background(), c, cfg, "deploy", 1, nil); err != nil {
		t.Fatal(err)
	}
	if tr.nodes["n"].vms[200]["name"] != "gw-A0" || tr.nodes["second"].vms[201]["name"] != "gw-A1" || len(tr.pools) != 2 {
		t.Fatal(tr.pools, tr.vmNodes)
	}
	if !strings.Contains(tr.nodes["second"].vms[201]["net0"], "bridge=vmbr1101") {
		t.Fatal(tr.nodes["second"].vms[201])
	}
	before := len(tr.operations)
	cfg.Runtime = nil
	if err := Execute(context.Background(), c, cfg, "deploy", 1, nil); err != nil {
		t.Fatal(err)
	}
	for _, op := range tr.operations[before:] {
		if strings.HasSuffix(op, "/clone") {
			t.Fatal("resume created another clone", op)
		}
	}
	if err := cfg.SelectParticipant("A1"); err != nil {
		t.Fatal(err)
	}
	if err := Execute(context.Background(), c, cfg, "snapshot", 1, nil); err != nil {
		t.Fatal(err)
	}
	if err := Execute(context.Background(), c, cfg, "reset", 1, nil); err != nil {
		t.Fatal(err)
	}
	before = len(tr.operations)
	if err := Execute(context.Background(), c, cfg, "destroy", 1, nil); err != nil {
		t.Fatal(err)
	}
	if tr.nodes["n"].vms[200] == nil || tr.nodes["second"].vms[201] != nil || tr.pools["pool-A0"] == "" || tr.pools["pool-A1"] != "" {
		t.Fatal(tr.pools, tr.vmNodes)
	}
	if tr.nodes["n"].networks["vmbr1001"] == nil || tr.nodes["second"].networks["vmbr1101"] != nil || tr.nodes["n"].users["a0@pve"] == nil || tr.nodes["n"].users["a1@pve"] != nil {
		t.Fatal("selected destroy affected wrong resources")
	}
	for _, op := range tr.operations[before:] {
		if strings.Contains(op, "/nodes/n/") {
			t.Fatal("mutated unselected node", op)
		}
	}
}
func TestMultiNodePreflightChecksPerNodeRAMBeforeAnyMutation(t *testing.T) {
	tr, c, cfg := multiFixture(t)
	tr.free["second"] = 512 << 20
	if err := Execute(context.Background(), c, cfg, "deploy", 1, nil); err == nil || !strings.Contains(err.Error(), "second") {
		t.Fatal(err)
	}
	if len(tr.operations) != 0 {
		t.Fatal(tr.operations)
	}
	if err := cfg.SelectParticipant("A0"); err != nil {
		t.Fatal(err)
	}
	if err := Execute(context.Background(), c, cfg, "deploy", 1, nil); err != nil {
		t.Fatal(err)
	}
}
func TestSharedStorageBudgetIsSummedAcrossNodes(t *testing.T) {
	tr, c, cfg := multiFixture(t)
	tr.shared = true
	tr.avail = 300 << 20
	// Each 2 GiB linked disk estimate is 0.2 GiB, their sum exceeds 300 MiB.
	report := Check(context.Background(), c, cfg)
	if report.OK() || !strings.Contains(strings.Join(report.Errors, " "), "суммарного плана") {
		t.Fatal(report.Errors)
	}
	tr.shared = false
	if report = Check(context.Background(), c, cfg); !report.OK() {
		t.Fatal(report.Errors)
	}
}
func TestJournalBlocksParticipantMoveAndRenumber(t *testing.T) {
	for _, field := range []string{"node", "bridges"} {
		t.Run(field, func(t *testing.T) {
			tr, c, cfg := multiFixture(t)
			if err := Execute(context.Background(), c, cfg, "deploy", 1, nil); err != nil {
				t.Fatal(err)
			}
			cfg.SelectParticipant("A0")
			before := len(tr.operations)
			if field == "node" {
				cfg.Participants[0].Node = "second"
			} else {
				cfg.Participants[0].BridgeBase = 1501
			}
			if err := Execute(context.Background(), c, cfg, "deploy", 1, nil); err == nil {
				t.Fatal("layout change accepted")
			}
			if len(tr.operations) != before {
				t.Fatal(tr.operations[before:])
			}
		})
	}
}
func TestForeignBridgeCannotJoinAnotherParticipant(t *testing.T) {
	tr, c, cfg := multiFixture(t)
	tr.nodes["n"].networks["vmbr1001"] = map[string]string{"type": "bridge", "comments": "another owner"}
	if err := Execute(context.Background(), c, cfg, "deploy", 1, nil); err == nil || !strings.Contains(err.Error(), "bridge_base") {
		t.Fatal(err)
	}
	if len(tr.operations) != 0 {
		t.Fatal(tr.operations)
	}
}
func TestSelectedParticipantDoesNotRecoverAnotherPendingClone(t *testing.T) {
	_, c, cfg := multiFixture(t)
	if err := Bind(c, cfg); err != nil {
		t.Fatal(err)
	}
	inst := cfg.Instances()[1]
	inst.put(Resource{Kind: "vm", Name: "gw-A1", VMID: 800, Template: 2001, Operation: "clone", Stage: "uncertain"})
	cfg.SelectParticipant("A0")
	if err := Execute(context.Background(), c, cfg, "deploy", 1, nil); err != nil {
		t.Fatal(err)
	}
	record, ok := inst.record("vm", "gw-A1")
	if !ok || record.Stage != "uncertain" || record.VMID != 800 {
		t.Fatal(fmt.Sprint(record))
	}
}

func TestTenStandsDeploy150VMsWithoutDuplicates(t *testing.T) {
	tr, c, cfg := multiFixture(t)
	cfg.Participants = nil
	cfg.Nodes = nil
	cfg.VMs[0].Count = 15
	for i := 0; i < 10; i++ {
		cfg.Participants = append(cfg.Participants, Participant{ID: fmt.Sprintf("A%d", i), BridgeBase: 1001 + i*100, Users: []User{{ID: fmt.Sprintf("a%d@pve", i), Password: "Secret123!", Role: "PVEVMUser"}}})
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	tr.free["n"] = 224 << 30
	if err := Execute(context.Background(), c, cfg, "deploy", 1, nil); err != nil {
		t.Fatal(err)
	}
	if len(tr.membership) != 150 || len(tr.pools) != 10 || len(tr.nodes["n"].users) != 10 {
		t.Fatal(len(tr.membership), len(tr.pools), len(tr.nodes["n"].users))
	}
	before := len(tr.operations)
	cfg.Runtime = nil
	if err := Execute(context.Background(), c, cfg, "deploy", 1, nil); err != nil {
		t.Fatal(err)
	}
	for _, op := range tr.operations[before:] {
		if strings.HasSuffix(op, "/clone") {
			t.Fatal("duplicate clone", op)
		}
	}
}
