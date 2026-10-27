package pve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func testClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	c, e := NewWithTransport("http://pve.test:8006", "test@pve!test=super-secret", roundTripFunc(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		h(w, r)
		return w.Result(), nil
	}))
	if e != nil {
		t.Fatal(e)
	}
	return c
}
func response(w http.ResponseWriter, data any) {
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}
func TestTaskFailureIncludesFullPaginatedRedactedLog(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/status") {
			response(w, map[string]string{"status": "stopped", "exitstatus": "QEMU exited with code 1"})
			return
		}
		if r.URL.Query().Get("start") == "0" {
			var rows []map[string]any
			for i := 0; i < 500; i++ {
				rows = append(rows, map[string]any{"n": i + 1, "t": fmt.Sprint("line ", i)})
			}
			response(w, rows)
		} else {
			response(w, []map[string]any{{"n": 501, "t": "CPU unsupported super-secret student-password"}})
		}
	})
	c.SetSecrets([]string{"student-password"})
	err := c.WaitTask(context.Background(), "SF-4", "UPID:test:", time.Second)
	var te *TaskError
	if !errors.As(err, &te) {
		t.Fatalf("expected task error: %v", err)
	}
	s := err.Error()
	if !strings.Contains(s, "CPU unsupported") || !strings.Contains(s, "line 499") || strings.Contains(s, "super-secret") || strings.Contains(s, "student-password") {
		t.Fatal(s)
	}
}
func TestForbiddenHasActionableMessageAndNoSecrets(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		fmt.Fprint(w, `{"message":"Sys.Audit super-secret"}`)
	})
	err := c.NodeOnline(context.Background(), "SF-4")
	if err == nil || !strings.Contains(err.Error(), "недостаточно прав") || strings.Contains(err.Error(), "super-secret") {
		t.Fatal(err)
	}
}
func TestWaitCancellationKeepsTaskIdentity(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) { response(w, map[string]string{"status": "running"}) })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := c.WaitTask(ctx, "n", "UPID:still-running:", time.Hour)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "UPID:still-running:") {
		t.Fatal(err)
	}
}
func TestParallelIDReservations(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) { response(w, "100") })
	var wg sync.WaitGroup
	ids := make(chan int, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, e := c.NextID(context.Background())
			if e != nil {
				t.Error(e)
			}
			ids <- id
		}()
	}
	wg.Wait()
	close(ids)
	seen := map[int]bool{}
	for id := range ids {
		if seen[id] {
			t.Fatal("duplicate", id)
		}
		seen[id] = true
	}
}
func TestPermissionsZeroMeansGranted(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		response(w, map[string]any{"/nodes/n": map[string]int{"Sys.Audit": 0}})
	})
	p, e := c.Permissions(context.Background(), "/nodes/n")
	if e != nil || !p["Sys.Audit"] {
		t.Fatal(p, e)
	}
}
func TestRetryClassification(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{{&APIError{Status: 403}, false}, {&APIError{Status: 503}, true}, {&TaskError{Status: "bad cpu"}, false}, {context.Canceled, false}, {&APIError{Status: 500, Msg: "can't lock file"}, true}} {
		if Transient(tc.err) != tc.want {
			t.Fatalf("%v", tc.err)
		}
	}
}
func TestRejectCredentialsInURL(t *testing.T) {
	if _, err := New("https://user:secret@host", "token", false); err == nil {
		t.Fatal("credentials accepted")
	}
}
func TestRedirectNeverForwardsToken(t *testing.T) {
	called := false
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Host == "other.test" {
			called = true
			response(w, nil)
			return
		}
		http.Redirect(w, r, "http://other.test/", 302)
	})
	if c.NodeOnline(context.Background(), "n") == nil || called {
		t.Fatal("redirect was followed")
	}
}

func TestTemplateFlagNumericAndBoolean(t *testing.T) {
	for _, value := range []string{"1", "true", "0", "false", "null"} {
		var m Member
		if err := json.Unmarshal([]byte(`{"template":`+value+`}`), &m); err != nil {
			t.Fatal(err)
		}
		want := value == "1" || value == "true"
		if bool(m.Template) != want {
			t.Fatal(value, m.Template)
		}
	}
}

func TestReadRetriesEOFAndRecovers(t *testing.T) {
	calls := 0
	c, err := NewWithTransport("http://pve.test:8006", "token", roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return nil, io.EOF
		}
		w := httptest.NewRecorder()
		response(w, map[string]any{"/vms/1009": map[string]int{"VM.Audit": 1}})
		return w.Result(), nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	perms, err := c.Permissions(context.Background(), "/vms/1009")
	if err != nil || calls != 2 || !perms["VM.Audit"] {
		t.Fatal(calls, perms, err)
	}
}
func TestPersistentReadFailureIsBoundedAndInformative(t *testing.T) {
	calls := 0
	c, _ := NewWithTransport("http://pve.test:8006", "token", roundTripFunc(func(*http.Request) (*http.Response, error) { calls++; return nil, io.EOF }))
	err := c.NodeOnline(context.Background(), "n")
	if !errors.Is(err, io.EOF) || calls != 3 || !strings.Contains(err.Error(), "3 попыток") {
		t.Fatal(calls, err)
	}
}

type brokenBody struct{ closed *bool }

func (b brokenBody) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func (b brokenBody) Close() error             { *b.closed = true; return nil }
func TestTruncatedReadClosesBodyBeforeRetry(t *testing.T) {
	calls := 0
	closed := false
	c, _ := NewWithTransport("http://pve.test:8006", "token", roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return &http.Response{StatusCode: 200, Status: "200 OK", Header: http.Header{}, Body: brokenBody{&closed}, Request: r}, nil
		}
		if !closed {
			t.Error("previous body left open")
		}
		w := httptest.NewRecorder()
		response(w, nil)
		return w.Result(), nil
	}))
	if err := c.NodeOnline(context.Background(), "n"); err != nil || calls != 2 {
		t.Fatal(calls, err)
	}
}
func TestMutationsNeverRetriedAfterEOF(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			calls := 0
			c, _ := NewWithTransport("http://pve.test:8006", "token", roundTripFunc(func(*http.Request) (*http.Response, error) { calls++; return nil, io.EOF }))
			err := c.do(context.Background(), method, "/mutation", nil, nil)
			if calls != 1 || !errors.Is(err, io.EOF) {
				t.Fatal(calls, err)
			}
		})
	}
}
func TestAuthorizationFailuresAreNotRetried(t *testing.T) {
	for _, status := range []int{401, 403} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			calls := 0
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.WriteHeader(status)
				fmt.Fprint(w, `{"message":"denied"}`)
			})
			err := c.NodeOnline(context.Background(), "n")
			var ae *APIError
			if !errors.As(err, &ae) || calls != 1 {
				t.Fatal(calls, err)
			}
		})
	}
}
func TestCancellationDuringReadBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	c, _ := NewWithTransport("http://pve.test:8006", "token", roundTripFunc(func(*http.Request) (*http.Response, error) { calls++; return nil, io.EOF }))
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	err := c.NodeOnline(ctx, "n")
	if calls != 1 || !errors.Is(err, context.Canceled) {
		t.Fatal(calls, err)
	}
}
func TestReadRecoversAfterTemporaryHTTPStatus(t *testing.T) {
	calls := 0
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(503)
			fmt.Fprint(w, `{"message":"busy"}`)
			return
		}
		response(w, nil)
	})
	if err := c.NodeOnline(context.Background(), "n"); err != nil || calls != 2 {
		t.Fatal(calls, err)
	}
}
