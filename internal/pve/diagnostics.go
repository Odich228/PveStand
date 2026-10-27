package pve

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"syscall"
)

type TaskError struct{ Node, UPID, Status, Log string }

func (e *TaskError) Error() string {
	return fmt.Sprintf("узел %s, UPID %s: %s\n%s", e.Node, e.UPID, e.Status, e.Log)
}
func (c *Client) Endpoint() string            { return c.base }
func (c *Client) SetSecrets(secrets []string) { c.secrets = append([]string(nil), secrets...) }
func (c *Client) Redact(s string) string {
	secrets := append([]string{c.auth}, c.secrets...)
	if _, secret, ok := strings.Cut(c.auth, "="); ok {
		secrets = append(secrets, secret)
	}
	for _, secret := range secrets {
		if secret != "" {
			s = strings.ReplaceAll(s, secret, "[скрыто]")
			s = strings.ReplaceAll(s, url.QueryEscape(secret), "[скрыто]")
		}
	}
	return s
}
func Transient(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var task *TaskError
	if errors.As(err, &task) {
		return false
	}
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Status == 429 || ae.Status == 502 || ae.Status == 503 || ae.Status == 504 || (ae.Status == 500 && strings.Contains(strings.ToLower(ae.Msg), "lock"))
	}
	var ne net.Error
	return errors.As(err, &ne)
}
func IDConflict(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && (ae.Status == 409 || (strings.Contains(strings.ToLower(ae.Msg), "already exists") && strings.Contains(strings.ToLower(ae.Msg), "vm")))
}
func (c *Client) TaskLog(ctx context.Context, node, upid string) (string, error) {
	var lines []string
	for start := 0; ; start += 500 {
		var rows []struct {
			N int    `json:"n"`
			T string `json:"t"`
		}
		q := url.Values{"start": {fmt.Sprint(start)}, "limit": {"500"}}
		if err := c.do(ctx, http.MethodGet, "/nodes/"+node+"/tasks/"+url.PathEscape(upid)+"/log", q, &rows); err != nil {
			return "", err
		}
		sort.Slice(rows, func(i, j int) bool { return rows[i].N < rows[j].N })
		for _, r := range rows {
			lines = append(lines, r.T)
		}
		if len(rows) < 500 {
			break
		}
		if start >= 99500 {
			lines = append(lines, "[журнал ограничен 100000 строками]")
			break
		}
	}
	return strings.Join(lines, "\n"), nil
}
func (c *Client) NodeStatus(ctx context.Context, node string) (map[string]string, error) {
	return rawFields(ctx, c, http.MethodGet, "/nodes/"+node+"/status")
}
func (c *Client) NodeMemory(ctx context.Context, node string) (uint64, uint64, error) {
	var st struct {
		Memory struct{ Total, Free uint64 } `json:"memory"`
	}
	err := c.do(ctx, http.MethodGet, "/nodes/"+node+"/status", nil, &st)
	return st.Memory.Total, st.Memory.Free, err
}
func (c *Client) Permissions(ctx context.Context, path string) (map[string]bool, error) {
	var result map[string]map[string]int
	err := c.do(ctx, http.MethodGet, "/access/permissions", url.Values{"path": {path}}, &result)
	out := map[string]bool{}
	for privilege := range result[path] {
		out[privilege] = true
	}
	return out, err
}
func (c *Client) PoolInfo(ctx context.Context, pool string) (map[string]string, error) {
	return rawFields(ctx, c, http.MethodGet, "/pools/"+pool)
}
func (c *Client) UserInfo(ctx context.Context, id string) (map[string]string, error) {
	return rawFields(ctx, c, http.MethodGet, "/access/users/"+url.PathEscape(id))
}
func (c *Client) CreateOwnedUser(ctx context.Context, id, password, comment string) error {
	return c.do(ctx, http.MethodPost, "/access/users", url.Values{"userid": {id}, "password": {password}, "comment": {comment}}, nil)
}
func (c *Client) CloneOwned(ctx context.Context, node string, template, id int, name, pool, storage, description string, full bool) (string, error) {
	p := url.Values{"newid": {fmt.Sprint(id)}, "name": {name}, "pool": {pool}, "description": {description}, "full": {"0"}}
	if full {
		p.Set("full", "1")
	}
	if storage != "" {
		p.Set("storage", storage)
	}
	var upid string
	err := c.do(ctx, http.MethodPost, fmt.Sprintf("/nodes/%s/qemu/%d/clone", node, template), p, &upid)
	return upid, err
}
func (c *Client) AgentReady(ctx context.Context, node string, id int) error {
	return c.do(ctx, http.MethodPost, fmt.Sprintf("/nodes/%s/qemu/%d/agent/ping", node, id), nil, nil)
}
func (c *Client) ClusterVMs(ctx context.Context) ([]Member, error) {
	var v []Member
	err := c.do(ctx, http.MethodGet, "/cluster/resources", url.Values{"type": {"vm"}}, &v)
	return v, err
}

// NewWithTransport supports isolated API tests and custom certificate transports.
func NewWithTransport(host, token string, transport http.RoundTripper) (*Client, error) {
	c, err := New(host, token, false)
	if err != nil {
		return nil, err
	}
	if transport == nil {
		return nil, fmt.Errorf("transport обязателен")
	}
	c.http.Transport = transport
	return c, nil
}

func (c *Client) MajorVersion(ctx context.Context) (int, error) {
	var v struct {
		Version string `json:"version"`
	}
	if err := c.do(ctx, http.MethodGet, "/version", nil, &v); err != nil {
		return 0, err
	}
	var major int
	if _, err := fmt.Sscanf(v.Version, "%d", &major); err != nil {
		return 0, err
	}
	return major, nil
}

func (c *Client) BeginNetworkApply(ctx context.Context, node string) (string, error) {
	var upid string
	err := c.do(ctx, http.MethodPut, "/nodes/"+node+"/network", nil, &upid)
	return upid, err
}

// APIBool handles both numeric (0/1) and JSON boolean representations used by PVE.
type APIBool bool

func (b *APIBool) UnmarshalJSON(data []byte) error {
	switch strings.TrimSpace(string(data)) {
	case "1", "true":
		*b = true
	case "0", "false", "null":
		*b = false
	default:
		return fmt.Errorf("некорректное логическое значение API %q", data)
	}
	return nil
}

// Retry only failed reads and explicitly temporary HTTP responses, never authorization failures.
func retryableRead(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Status == 429 || ae.Status == 502 || ae.Status == 503 || ae.Status == 504
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNABORTED) || errors.Is(err, syscall.EPIPE) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// StorageShared uses the index API (Datastore.Audit), not the privileged config-read endpoint.
func (c *Client) StorageShared(ctx context.Context, storage string) (bool, error) {
	var rows []struct {
		Storage string  `json:"storage"`
		Shared  APIBool `json:"shared"`
	}
	if err := c.do(ctx, http.MethodGet, "/storage", nil, &rows); err != nil {
		return false, err
	}
	for _, row := range rows {
		if row.Storage == storage {
			return bool(row.Shared), nil
		}
	}
	return false, fmt.Errorf("хранилище %s отсутствует в доступном списке /storage", storage)
}
