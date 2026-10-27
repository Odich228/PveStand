package stand

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"pvestand/internal/pve"
	"strings"
	"testing"
	"time"
)

type cloneEOFTransport struct {
	f                                                             *fakePVE
	accept, locked, temporary, missingDisk, workerFailed, foreign bool
	fired                                                         bool
	cloneCalls, taskQueries                                       int
	saved                                                         map[string]string
}

func (tr *cloneEOFTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	reply := func(data any) (*http.Response, error) {
		w := httptest.NewRecorder()
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
		return w.Result(), nil
	}
	p := strings.TrimPrefix(r.URL.Path, "/api2/json")
	if r.Method == http.MethodPost && strings.HasSuffix(p, "/clone") {
		tr.cloneCalls++
		if !tr.fired {
			tr.fired = true
			if tr.foreign {
				tr.f.mu.Lock()
				tr.f.vms[200] = map[string]string{"name": "stranger", "description": "other owner"}
				tr.f.mu.Unlock()
			}
			if tr.accept {
				if _, err := (fakeTransport{tr.f}).RoundTrip(r); err != nil {
					return nil, err
				}
				tr.f.mu.Lock()
				if tr.missingDisk {
					delete(tr.f.vms[200], "scsi0")
				}
				if tr.locked {
					tr.f.vms[200]["lock"] = "clone"
				}
				if tr.temporary {
					tr.saved = tr.f.vms[200]
					tr.f.vms[200] = map[string]string{"lock": "clone"}
				}
				tr.f.mu.Unlock()
			}
			return nil, io.EOF
		}
	}
	if p == "/nodes/n/tasks" {
		tr.taskQueries++
		if tr.accept && (tr.locked || tr.temporary) {
			return reply([]pve.TaskSummary{{UPID: "UPID:recovered:", Node: "n", Type: "qmclone", ID: "1001", StartTime: time.Now().Unix()}})
		}
		return reply([]pve.TaskSummary{})
	}
	if p == "/nodes/n/tasks/UPID:recovered:/log" {
		rows := []map[string]any{{"n": 1, "t": "creating a clone of VM 1001 with ID 200"}}
		if tr.workerFailed {
			rows = append(rows, map[string]any{"n": 2, "t": "clone failed: disk full"})
		}
		return reply(rows)
	}
	if p == "/nodes/n/tasks/UPID:recovered:/status" {
		tr.f.mu.Lock()
		if tr.temporary {
			tr.f.vms[200] = tr.saved
		}
		delete(tr.f.vms[200], "lock")
		if tr.workerFailed {
			delete(tr.f.vms, 200)
		}
		tr.f.mu.Unlock()
		exit := "OK"
		if tr.workerFailed {
			exit = "clone failed: disk full"
		}
		return reply(map[string]string{"status": "stopped", "exitstatus": exit})
	}
	return (fakeTransport{tr.f}).RoundTrip(r)
}
func recoveryFixture(t *testing.T, tr *cloneEOFTransport) (*pve.Client, *Config) {
	f, _, cfg := fixture(t)
	tr.f = f
	c, err := pve.NewWithTransport("http://pve.test:8006", "token", tr)
	if err != nil {
		t.Fatal(err)
	}
	return c, cfg
}
func TestAcceptedCloneLostResponseContinuesWithoutSecondPOST(t *testing.T) {
	tr := &cloneEOFTransport{accept: true}
	c, cfg := recoveryFixture(t, tr)
	if err := Execute(context.Background(), c, cfg, "deploy", 1, nil); err != nil {
		t.Fatal(err)
	}
	if tr.cloneCalls != 1 || tr.f.status[200] != "running" {
		t.Fatal(tr.cloneCalls, tr.f.status)
	}
	r, _ := cfg.record("vm", "gw")
	if r.VMID != 200 || r.RequestedAt == 0 || r.Template != 1001 {
		t.Fatal(r)
	}
}
func TestLockedCloneRecoversTaskWithoutRepeatingPOST(t *testing.T) {
	tr := &cloneEOFTransport{accept: true, locked: true}
	c, cfg := recoveryFixture(t, tr)
	if err := Execute(context.Background(), c, cfg, "deploy", 1, nil); err != nil {
		t.Fatal(err)
	}
	if tr.cloneCalls != 1 || tr.taskQueries == 0 || tr.f.status[200] != "running" {
		t.Fatal(tr.cloneCalls, tr.taskQueries, tr.f.status)
	}
}
func TestTemporaryCloneConfigCanRecoverBeforeOwnerWritten(t *testing.T) {
	tr := &cloneEOFTransport{accept: true, temporary: true}
	c, cfg := recoveryFixture(t, tr)
	if err := Execute(context.Background(), c, cfg, "deploy", 1, nil); err != nil {
		t.Fatal(err)
	}
	if tr.cloneCalls != 1 || tr.f.status[200] != "running" {
		t.Fatal(tr.cloneCalls, tr.f.status)
	}
}
func TestUnacceptedCloneIsNotBlindlyRepeated(t *testing.T) {
	tr := &cloneEOFTransport{}
	c, cfg := recoveryFixture(t, tr)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := Execute(ctx, c, cfg, "deploy", 1, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	r, _ := cfg.record("vm", "gw")
	if r.Stage != "uncertain" || r.VMID != 200 || tr.cloneCalls != 1 {
		t.Fatal(r, tr.cloneCalls)
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel2()
	err = Execute(ctx2, c, cfg, "deploy", 1, nil)
	if !errors.Is(err, context.DeadlineExceeded) || tr.cloneCalls != 1 {
		t.Fatal(err, tr.cloneCalls)
	}
}
func TestIncompleteCloneCannotStartAfterLostResponse(t *testing.T) {
	tr := &cloneEOFTransport{accept: true, missingDisk: true}
	c, cfg := recoveryFixture(t, tr)
	err := Execute(context.Background(), c, cfg, "deploy", 1, nil)
	if err == nil || !strings.Contains(err.Error(), "scsi0") {
		t.Fatal(err)
	}
	if tr.f.status[200] == "running" || tr.cloneCalls != 1 {
		t.Fatal(tr.f.status, tr.cloneCalls)
	}
}
func TestForeignVMAtReservedIDStopsRecovery(t *testing.T) {
	tr := &cloneEOFTransport{foreign: true}
	c, cfg := recoveryFixture(t, tr)
	err := Execute(context.Background(), c, cfg, "deploy", 1, nil)
	if err == nil || !strings.Contains(err.Error(), "другой ВМ") {
		t.Fatal(err)
	}
	if tr.cloneCalls != 1 {
		t.Fatal(tr.cloneCalls)
	}
	for _, op := range tr.f.operations {
		if strings.Contains(op, "/status/start") || strings.HasPrefix(op, "DELETE ") {
			t.Fatal(op)
		}
	}
}
func TestRecoveredWorkerFailureShowsActualTaskError(t *testing.T) {
	tr := &cloneEOFTransport{accept: true, locked: true, workerFailed: true}
	c, cfg := recoveryFixture(t, tr)
	err := Execute(context.Background(), c, cfg, "deploy", 1, nil)
	var te *pve.TaskError
	if !errors.As(err, &te) || !strings.Contains(err.Error(), "disk full") {
		t.Fatal(err)
	}
	if tr.cloneCalls != 1 {
		t.Fatal(tr.cloneCalls)
	}
}
func TestLegacyIntentIsRecoveredBeforePreflightAndMutations(t *testing.T) {
	f, c, cfg := fixture(t)
	if err := Bind(c, cfg); err != nil {
		t.Fatal(err)
	}
	f.vms[200] = map[string]string{"name": "gw", "description": cfg.marker(), "scsi0": "local:vm-200-disk-0", "memory": "1024"}
	f.status[200] = "stopped"
	if err := cfg.put(Resource{Kind: "vm", Name: "gw", VMID: 200, Operation: "clone", Stage: "intent"}); err != nil {
		t.Fatal(err)
	}
	report := Check(context.Background(), c, cfg)
	if !report.OK() {
		t.Fatal(report.Errors)
	}
	if !strings.Contains(strings.Join(report.Warnings, " "), "ответ clone потерян") {
		t.Fatal(report.Warnings)
	}
	if err := Execute(context.Background(), c, cfg, "deploy", 1, nil); err != nil {
		t.Fatal(err)
	}
	for _, op := range f.operations {
		if strings.HasSuffix(op, "/clone") {
			t.Fatal("clone resubmitted", op)
		}
	}
	r, _ := cfg.record("vm", "gw")
	if r.VMID != 200 || r.Template != 1001 {
		t.Fatal(fmt.Sprint(r))
	}
}

func TestExplicitRetryUsesOriginalVMIDAfterConfirmedAbsence(t *testing.T) {
	tr := &cloneEOFTransport{}
	c, cfg := recoveryFixture(t, tr)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := Execute(ctx, c, cfg, "deploy", 1, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	tr.accept = true
	tr.f.next = 999
	cfg.RetryClone = "gw"
	if err := Execute(context.Background(), c, cfg, "deploy", 1, nil); err != nil {
		t.Fatal(err)
	}
	r, _ := cfg.record("vm", "gw")
	if r.VMID != 200 || tr.f.vms[999] != nil || tr.f.status[200] != "running" || tr.cloneCalls != 2 {
		t.Fatal(r, tr.cloneCalls, tr.f.status)
	}
}
func TestExplicitRetryWithoutPendingRecordDoesNotMutateServer(t *testing.T) {
	f, c, cfg := fixture(t)
	cfg.RetryClone = "unknown"
	if err := Execute(context.Background(), c, cfg, "deploy", 1, nil); err == nil {
		t.Fatal("unexpected retry")
	}
	if len(f.operations) != 0 {
		t.Fatal(f.operations)
	}
}
func TestExplicitRetryWillNotReplaceForeignOccupant(t *testing.T) {
	f, c, cfg := fixture(t)
	if err := Bind(c, cfg); err != nil {
		t.Fatal(err)
	}
	if err := cfg.put(Resource{Kind: "vm", Name: "gw", VMID: 200, Template: 1001, Operation: "clone", Stage: "uncertain"}); err != nil {
		t.Fatal(err)
	}
	f.vms[200] = map[string]string{"name": "stranger", "description": "other owner"}
	cfg.RetryClone = "gw"
	if err := Execute(context.Background(), c, cfg, "deploy", 1, nil); err == nil {
		t.Fatal("foreign VM accepted")
	}
	if len(f.operations) != 0 {
		t.Fatal(f.operations)
	}
}
