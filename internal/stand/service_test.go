package stand

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"pvestand/internal/pve"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakePVE struct {
	mu             sync.Mutex
	pool           string
	vms            map[int]map[string]string
	status         map[int]string
	networks       map[string]map[string]string
	users          map[string]map[string]string
	tasks          map[string]string
	next           int
	operations     []string
	failStart      bool
	conflict       bool
	deny           bool
	foreignNetwork bool
	pending        bool
}

func newFake() *fakePVE {
	return &fakePVE{vms: map[int]map[string]string{1001: {"name": "template", "template": "1", "memory": "1024", "scsi0": "local:base-1001,size=2G", "net0": "virtio=AA:BB:CC:DD:EE:FF,bridge=vmbr0"}}, status: map[int]string{}, networks: map[string]map[string]string{"vmbr0": {"type": "bridge", "bridge_ports": "eth0"}}, users: map[string]map[string]string{}, tasks: map[string]string{}, next: 200}
}

type fakeTransport struct{ f *fakePVE }

func (tr fakeTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	w := httptest.NewRecorder()
	tr.f.serve(w, r)
	return w.Result(), nil
}
func (f *fakePVE) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := strings.TrimPrefix(r.URL.Path, "/api2/json")
	_ = r.ParseForm()
	reply := func(data any) { _ = json.NewEncoder(w).Encode(map[string]any{"data": data}) }
	missing := func() { w.WriteHeader(404); fmt.Fprint(w, `{"message":"does not exist"}`) }
	if r.Method != "GET" {
		f.operations = append(f.operations, r.Method+" "+p)
	}
	if f.deny {
		w.WriteHeader(403)
		fmt.Fprint(w, `{"message":"Sys.Audit denied"}`)
		return
	}
	switch {
	case p == "/version":
		reply(map[string]string{"version": "9.1"})
	case p == "/access/permissions":
		perms := map[string]int{}
		for _, s := range strings.Fields("Sys.Audit Sys.Modify Pool.Allocate Pool.Audit VM.Audit VM.Allocate VM.Clone VM.Config.CPU VM.Config.Memory VM.Config.Network VM.Config.Options VM.PowerMgmt VM.Snapshot VM.Snapshot.Rollback Datastore.Audit Datastore.AllocateSpace User.Modify Permissions.Modify SDN.Use Realm.AllocateUser") {
			perms[s] = 0
		}
		reply(map[string]any{r.URL.Query().Get("path"): perms})
	case p == "/nodes/n/status":
		reply(map[string]any{"memory": map[string]uint64{"total": 1 << 36, "free": 1 << 35}})
	case p == "/nodes/n/storage/local/status":
		reply(map[string]int64{"total": 1 << 40, "avail": 1 << 39})
	case p == "/cluster/resources":
		var rows []map[string]any
		for id, v := range f.vms {
			rows = append(rows, map[string]any{"type": "qemu", "vmid": id, "node": "n", "name": v["name"], "tags": v["tags"], "template": v["template"] == "1"})
		}
		reply(rows)
	case p == "/cluster/nextid":
		reply(fmt.Sprint(f.next))
	case strings.HasPrefix(p, "/nodes/n/tasks/"):
		tail := strings.TrimPrefix(p, "/nodes/n/tasks/")
		upid, part, _ := strings.Cut(tail, "/")
		if part == "log" {
			reply([]map[string]any{{"n": 1, "t": "CPU unsupported: feature vmx"}})
			return
		}
		status := "stopped"
		if f.pending {
			status = "running"
		}
		exit := f.tasks[upid]
		if exit == "" {
			exit = "OK"
		}
		reply(map[string]string{"status": status, "exitstatus": exit})
	case p == "/pools" && r.Method == "POST":
		f.pool = r.Form.Get("comment")
		reply(nil)
	case p == "/pools/pool":
		if f.pool == "" {
			missing()
			return
		}
		if r.Method == "DELETE" {
			f.pool = ""
			reply(nil)
			return
		}
		var rows []map[string]any
		for id, v := range f.vms {
			if id == 1001 {
				continue
			}
			rows = append(rows, map[string]any{"type": "qemu", "vmid": id, "node": "n", "name": v["name"], "tags": v["tags"], "template": v["template"] == "1"})
		}
		reply(map[string]any{"comment": f.pool, "members": rows})
	case p == "/nodes/n/network":
		if r.Method == "PUT" {
			reply("UPID:network:")
			return
		}
		if r.Method == "POST" {
			f.networks[r.Form.Get("iface")] = map[string]string{"type": "bridge", "comments": r.Form.Get("comments")}
			reply(nil)
			return
		}
		reply(nil)
	case strings.HasPrefix(p, "/nodes/n/network/"):
		name := strings.TrimPrefix(p, "/nodes/n/network/")
		n, ok := f.networks[name]
		if !ok {
			missing()
			return
		}
		if r.Method == "DELETE" {
			delete(f.networks, name)
			reply(nil)
			return
		}
		reply(n)
	case p == "/access/users" && r.Method == "POST":
		f.users[r.Form.Get("userid")] = map[string]string{"comment": r.Form.Get("comment")}
		reply(nil)
	case strings.HasPrefix(p, "/access/users/"):
		name := strings.TrimPrefix(p, "/access/users/")
		u, ok := f.users[name]
		if !ok {
			missing()
			return
		}
		if r.Method == "DELETE" {
			delete(f.users, name)
			reply(nil)
			return
		}
		reply(u)
	case p == "/access/acl":
		reply(nil)
	case strings.HasPrefix(p, "/nodes/n/qemu/"):
		parts := strings.Split(strings.TrimPrefix(p, "/nodes/n/qemu/"), "/")
		id, _ := strconv.Atoi(parts[0])
		v, ok := f.vms[id]
		if !ok {
			missing()
			return
		}
		if len(parts) == 1 && r.Method == "DELETE" {
			delete(f.vms, id)
			delete(f.status, id)
			reply("UPID:delete:")
			return
		}
		switch strings.Join(parts[1:], "/") {
		case "clone":
			newID, _ := strconv.Atoi(r.Form.Get("newid"))
			if f.conflict {
				f.conflict = false
				w.WriteHeader(500)
				fmt.Fprintf(w, "VM %d already exists", newID)
				return
			}
			if _, exists := f.vms[newID]; exists {
				w.WriteHeader(409)
				return
			}
			cp := map[string]string{}
			for k, value := range v {
				cp[k] = value
			}
			delete(cp, "template")
			cp["name"] = r.Form.Get("name")
			cp["description"] = r.Form.Get("description")
			f.vms[newID] = cp
			f.status[newID] = "stopped"
			reply("UPID:clone:")
		case "config":
			if r.Method == "PUT" {
				for k, values := range r.Form {
					if k == "delete" {
						for _, key := range strings.Split(values[0], ",") {
							delete(v, key)
						}
					} else {
						v[k] = values[0]
					}
				}
				reply(nil)
			} else {
				reply(v)
			}
		case "status/current":
			reply(map[string]string{"status": f.status[id]})
		case "status/start":
			if f.failStart {
				f.tasks["UPID:start:"] = "QEMU exited with code 1"
			} else {
				f.tasks["UPID:start:"] = "OK"
				f.status[id] = "running"
			}
			reply("UPID:start:")
		case "status/stop":
			f.status[id] = "stopped"
			reply("UPID:stop:")
		case "snapshot":
			if r.Method == "GET" {
				reply([]map[string]string{{"name": BaselineSnapshot}})
			} else {
				reply("UPID:snapshot:")
			}
		case "agent/ping":
			reply(nil)
		default:
			w.WriteHeader(500)
			fmt.Fprint(w, "unhandled VM "+p)
		}
	default:
		w.WriteHeader(500)
		fmt.Fprint(w, "unhandled "+p)
	}
}
func fixture(t *testing.T) (*fakePVE, *pve.Client, *Config) {
	t.Helper()
	dir := t.TempDir()
	old := DefaultStateDir
	DefaultStateDir = filepath.Join(dir, "state")
	t.Cleanup(func() { DefaultStateDir = old })
	f := newFake()
	c, e := pve.NewWithTransport("http://pve.test:8006", "x@pve!x=secret", fakeTransport{f})
	if e != nil {
		t.Fatal(e)
	}
	cfg, e := loadText(t, minimal+"    cores: 2\n    memory: 1024\n    nics:\n      - bridge: vmbr101\nnetworks:\n  - bridge: vmbr101\nusers:\n  - id: student@pve\n    password: Password123!\n")
	if e != nil {
		t.Fatal(e)
	}
	return f, c, cfg
}
func TestDeployResumeAndDestroy(t *testing.T) {
	f, c, cfg := fixture(t)
	f.failStart = true
	err := Execute(context.Background(), c, cfg, "deploy", 2, nil)
	if err == nil || !strings.Contains(err.Error(), "CPU unsupported") {
		t.Fatal(err)
	}
	r, ok := cfg.record("vm", "gw")
	if !ok || r.VMID != 200 || r.UPID != "" {
		t.Fatal(r)
	}
	f.failStart = false
	cfg.Runtime = nil // simulate a new process reading persisted identity and VMID
	if err = Execute(context.Background(), c, cfg, "deploy", 2, nil); err != nil {
		t.Fatal(err)
	}
	clones := 0
	for _, op := range f.operations {
		if strings.HasSuffix(op, "/clone") {
			clones++
		}
	}
	if clones != 1 {
		t.Fatalf("cloned %d times", clones)
	}
	if err = Execute(context.Background(), c, cfg, "destroy", 2, nil); err != nil {
		t.Fatal(err)
	}
	if len(f.vms) != 1 || f.pool != "" || len(f.users) != 0 || len(f.networks) != 1 {
		t.Fatal(f.vms, f.pool, f.users, f.networks)
	}
	if _, err = os.Stat(cfg.Runtime.path); err != nil {
		t.Fatal(err)
	}
}
func TestForeignResourcesNeverAdoptedOrDeleted(t *testing.T) {
	f, c, cfg := fixture(t)
	f.vms[200] = map[string]string{"name": "gw", "description": "someone else"}
	err := Execute(context.Background(), c, cfg, "deploy", 2, nil)
	if err == nil || len(f.operations) != 0 {
		t.Fatal(err, f.operations)
	}
	if err = Execute(context.Background(), c, cfg, "destroy", 2, nil); err != nil {
		t.Fatal(err)
	}
	if f.vms[200] == nil {
		t.Fatal("foreign VM deleted")
	}
}
func TestReusedNetworkSurvivesDestroy(t *testing.T) {
	f, c, cfg := fixture(t)
	f.networks["vmbr101"] = map[string]string{"type": "bridge", "comments": "external"}
	if err := Execute(context.Background(), c, cfg, "deploy", 2, nil); err != nil {
		t.Fatal(err)
	}
	if err := Execute(context.Background(), c, cfg, "destroy", 2, nil); err != nil {
		t.Fatal(err)
	}
	if f.networks["vmbr101"] == nil {
		t.Fatal("reused network removed")
	}
}
func TestMarkerTamperingBlocksDeletion(t *testing.T) {
	f, c, cfg := fixture(t)
	if err := Execute(context.Background(), c, cfg, "deploy", 1, nil); err != nil {
		t.Fatal(err)
	}
	f.vms[200]["description"] = "changed owner"
	before := len(f.operations)
	if err := Execute(context.Background(), c, cfg, "destroy", 1, nil); err == nil {
		t.Fatal("foreign ownership accepted")
	}
	if len(f.operations) != before {
		t.Fatal(f.operations[before:])
	}
}
func TestVMIDConflictRetriesWithoutDeletingConflict(t *testing.T) {
	f, c, cfg := fixture(t)
	f.conflict = true
	if err := Execute(context.Background(), c, cfg, "deploy", 2, nil); err != nil {
		t.Fatal(err)
	}
	r, _ := cfg.record("vm", "gw")
	if r.VMID != 201 {
		t.Fatal(r)
	}
}
func TestPreflightDenialStopsAllMutations(t *testing.T) {
	f, c, cfg := fixture(t)
	f.deny = true
	if err := Execute(context.Background(), c, cfg, "deploy", 1, nil); err == nil || !strings.Contains(err.Error(), "недостаточно прав") {
		t.Fatal(err)
	}
	if len(f.operations) > 0 {
		t.Fatal(f.operations)
	}
}
func TestPendingTaskSurvivesCancellation(t *testing.T) {
	f, c, cfg := fixture(t)
	if err := Bind(c, cfg); err != nil {
		t.Fatal(err)
	}
	release, err := cfg.Runtime.acquire()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	f.pending = true
	r := Resource{Kind: "vm", Name: "gw", VMID: 200, UPID: "UPID:pending:", Operation: "clone"}
	if err = cfg.put(r); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err = resumeTask(ctx, c, cfg, r)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	saved, _ := cfg.record("vm", "gw")
	if saved.UPID != r.UPID {
		t.Fatal(saved)
	}
	f.pending = false
	if err = resumeTask(context.Background(), c, cfg, saved); err != nil {
		t.Fatal(err)
	}
	saved, _ = cfg.record("vm", "gw")
	if saved.UPID != "" {
		t.Fatal(saved)
	}
}
func TestJournalLockAndCorruption(t *testing.T) {
	_, c, cfg := fixture(t)
	if err := Bind(c, cfg); err != nil {
		t.Fatal(err)
	}
	release, err := cfg.Runtime.acquire()
	if err != nil {
		t.Fatal(err)
	}
	if r, e := cfg.Runtime.acquire(); e == nil {
		r()
		t.Fatal("second lock acquired")
	}
	release()
	os.WriteFile(cfg.Runtime.path, []byte("broken"), 0600)
	cfg.Runtime = nil
	if err = Bind(c, cfg); err == nil {
		t.Fatal("corrupt journal accepted")
	}
}
func TestDependencySchedulerAndFailurePropagation(t *testing.T) {
	var mu sync.Mutex
	order := []string{}
	mk := func(s string) Step {
		return Step{Name: s, Run: func(context.Context) error { mu.Lock(); order = append(order, s); mu.Unlock(); return nil }}
	}
	p := &Plan{Parallel: []Chain{{Label: "app", DependsOn: []string{"gw"}, Steps: []Step{mk("app")}}, {Label: "gw", Steps: []Step{mk("gw")}}}}
	if err := Run(context.Background(), p, 1, nil); err != nil {
		t.Fatal(err)
	}
	if strings.Join(order, ",") != "gw,app" {
		t.Fatal(order)
	}
	p.Parallel[1].Steps[0].Run = func(context.Context) error { return errors.New("failed gateway") }
	order = nil
	if err := Run(context.Background(), p, 2, nil); err == nil || len(order) != 0 {
		t.Fatal(err, order)
	}
}
func TestSnapshotKeepsExistingBaseline(t *testing.T) {
	f, c, cfg := fixture(t)
	if err := Execute(context.Background(), c, cfg, "deploy", 1, nil); err != nil {
		t.Fatal(err)
	}
	before := len(f.operations)
	if err := Execute(context.Background(), c, cfg, "snapshot", 1, nil); err != nil {
		t.Fatal(err)
	}
	if len(f.operations) != before {
		t.Fatal(f.operations[before:])
	}
}

func TestReadinessAgentAndTimeout(t *testing.T) {
	_, c, cfg := fixture(t)
	if err := waitReady(context.Background(), c, cfg, VM{Ready: "agent"}, 1001); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitReady(ctx, c, cfg, VM{Ready: "tcp", ReadyAddress: "127.0.0.1:1"}, 1001); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
func TestOnlyIncludesAllDependencyCopies(t *testing.T) {
	cfg := &Config{Only: "app", VMs: []VM{{Name: "app", Count: 1, DependsOn: []string{"gw"}}, {Name: "gw", Count: 2}, {Name: "unrelated", Count: 1}}}
	s := selectedVMs(cfg)
	if len(s) != 3 || !s["gw-1"] || !s["gw-2"] || !s["app"] || s["unrelated"] {
		t.Fatal(s)
	}
}

func TestTemplateNameDoesNotConflictWithClone(t *testing.T) {
	f, c, cfg := fixture(t)
	f.vms[1001]["name"] = "gw"
	report := Check(context.Background(), c, cfg)
	if !report.OK() {
		t.Fatal(report.Errors)
	}
	lines, err := LivePlan(context.Background(), c, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range lines {
		if strings.Contains(line, "КОНФЛИКТ") {
			t.Fatal(line)
		}
	}
	if err = Execute(context.Background(), c, cfg, "deploy", 1, nil); err != nil {
		t.Fatal(err)
	}
	if f.vms[1001]["template"] != "1" || f.vms[200]["name"] != "gw" {
		t.Fatal("template changed or clone missing", f.vms)
	}
	if err = Execute(context.Background(), c, cfg, "destroy", 1, nil); err != nil {
		t.Fatal(err)
	}
	if f.vms[1001] == nil {
		t.Fatal("template deleted")
	}
}
func TestForeignVMStillConflictsBesideSameNamedTemplate(t *testing.T) {
	f, c, cfg := fixture(t)
	f.vms[1001]["name"] = "gw"
	f.vms[101] = map[string]string{"name": "gw"}
	report := Check(context.Background(), c, cfg)
	if report.OK() {
		t.Fatal("foreign VM accepted")
	}
	joined := strings.Join(report.Errors, "\n")
	if !strings.Contains(joined, "VMID 101 ") || strings.Contains(joined, "VMID 1001 ") {
		t.Fatal(joined)
	}
	if len(f.operations) > 0 {
		t.Fatal(f.operations)
	}
}

type onceEOFTransport struct {
	base   http.RoundTripper
	failed bool
	calls  int
}

func (tr *onceEOFTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if strings.HasSuffix(r.URL.Path, "/access/permissions") && r.URL.Query().Get("path") == "/vms/1001" {
		tr.calls++
		if !tr.failed {
			tr.failed = true
			return nil, io.EOF
		}
	}
	return tr.base.RoundTrip(r)
}
func TestPreflightRecoversFromOnePermissionReadEOF(t *testing.T) {
	f, _, cfg := fixture(t)
	tr := &onceEOFTransport{base: fakeTransport{f}}
	c, err := pve.NewWithTransport("http://pve.test:8006", "token", tr)
	if err != nil {
		t.Fatal(err)
	}
	report := Check(context.Background(), c, cfg)
	if !report.OK() || tr.calls != 2 {
		t.Fatal(report.Errors, tr.calls)
	}
	if len(f.operations) != 0 {
		t.Fatal("preflight mutated resources", f.operations)
	}
}
