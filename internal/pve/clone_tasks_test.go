package pve

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestCloneTaskSearchUsesSourceIDAndMatchesTargetInLog(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/tasks") {
			if r.URL.Query().Get("vmid") != "1007" || r.URL.Query().Get("source") != "all" || r.URL.Query().Get("typefilter") != "qmclone" {
				t.Error(r.URL)
			}
			response(w, []TaskSummary{{UPID: "UPID:right:", Node: "SF-4", Type: "qmclone", ID: "1007", StartTime: time.Now().Unix()}, {UPID: "UPID:wrong:", Node: "SF-4", Type: "qmclone", ID: "1007", StartTime: time.Now().Unix()}})
			return
		}
		line := "creating a clone of VM 1007 with ID 112"
		if strings.Contains(r.URL.Path, "UPID:right:") {
			line = "creating a clone of VM 1007 with ID 111"
		}
		response(w, []map[string]string{{"t": line}})
	})
	upid, err := c.FindCloneTask(context.Background(), "SF-4", 1007, 111, time.Now().Unix())
	if err != nil || upid != "UPID:right:" {
		t.Fatal(upid, err)
	}
}
func TestCloneTaskSearchRejectsMultipleMatches(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/tasks") {
			response(w, []TaskSummary{{UPID: "a", Node: "n", Type: "qmclone", ID: "1007"}, {UPID: "b", Node: "n", Type: "qmclone", ID: "1007"}})
			return
		}
		response(w, []map[string]string{{"t": fmt.Sprintf("creating a clone of VM %d with ID %d", 1007, 111)}})
	})
	if _, err := c.FindCloneTask(context.Background(), "n", 1007, 111, 0); err == nil {
		t.Fatal("ambiguous tasks accepted")
	}
}
