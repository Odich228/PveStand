package pve

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

type TaskSummary struct {
	UPID      string `json:"upid"`
	Node      string `json:"node"`
	Type      string `json:"type"`
	ID        string `json:"id"`
	StartTime int64  `json:"starttime"`
}

// Clone workers are indexed by the SOURCE VMID, not the destination VMID.
// Source: proxmox/qemu-server/src/PVE/API2/Qemu.pm, fork_worker('qmclone', $vmid,...).
// Confirm the destination using the worker's first log line, not a name match.
func (c *Client) FindCloneTask(ctx context.Context, node string, source, target int, since int64) (string, error) {
	matches := map[string]bool{}
	for start := 0; start < 200; start += 50 {
		q := url.Values{"source": {"all"}, "typefilter": {"qmclone"}, "vmid": {strconv.Itoa(source)}, "start": {strconv.Itoa(start)}, "limit": {"50"}}
		if since > 0 {
			q.Set("since", strconv.FormatInt(since-60, 10))
		}
		var tasks []TaskSummary
		if err := c.do(ctx, http.MethodGet, "/nodes/"+node+"/tasks", q, &tasks); err != nil {
			return "", err
		}
		for _, task := range tasks {
			if task.Type != "qmclone" || task.ID != strconv.Itoa(source) || task.Node != node || task.UPID == "" {
				continue
			}
			if since > 0 && task.StartTime < since-60 {
				continue
			}
			var rows []struct {
				T string `json:"t"`
			}
			if err := c.do(ctx, http.MethodGet, "/nodes/"+node+"/tasks/"+url.PathEscape(task.UPID)+"/log", url.Values{"start": {"0"}, "limit": {"20"}}, &rows); err != nil {
				return "", err
			}
			expected := fmt.Sprintf("creating a clone of VM %d with ID %d", source, target)
			for _, row := range rows {
				if strings.TrimSpace(row.T) == expected {
					matches[task.UPID] = true
				}
			}
		}
		if len(tasks) < 50 {
			break
		}
	}
	if len(matches) > 1 {
		return "", fmt.Errorf("для клона VMID %d найдено несколько задач; выберите правильный UPID в Proxmox", target)
	}
	for upid := range matches {
		return upid, nil
	}
	return "", nil
}
