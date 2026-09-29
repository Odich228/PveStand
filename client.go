// Package pve — минимальный клиент REST API Proxmox VE (авторизация по API-токену).
package pve

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client — клиент API Proxmox.
type Client struct {
	base string
	auth string
	http *http.Client
}

// APIError — ошибка, вернувшаяся от API.
type APIError struct {
	Status int
	Msg    string
}

func (e *APIError) Error() string { return e.Msg }

// IsExists — ресурс уже существует.
func IsExists(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "already exist")
}

// IsMissing — ресурс не найден.
func IsMissing(err error) bool {
	if err == nil {
		return false
	}
	var ae *APIError
	if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
		return true
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "does not exist") || strings.Contains(s, "no such")
}

// New создаёт клиент. host: "192.168.1.10", "pve.local:8006" или полный URL.
// token: "root@pam!имя=секрет".
func New(host, token string, insecure bool) (*Client, error) {
	if host == "" || token == "" {
		return nil, errors.New("нужны адрес узла (-host / PVE_HOST) и API-токен (-token / PVE_TOKEN)")
	}
	if !strings.Contains(host, "://") {
		host = "https://" + host
	}
	u, err := url.Parse(host)
	if err != nil {
		return nil, err
	}
	if u.Port() == "" {
		u.Host += ":8006"
	}
	tr := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: insecure}}
	return &Client{
		base: strings.TrimRight(u.String(), "/") + "/api2/json",
		auth: token,
		http: &http.Client{Transport: tr, Timeout: 60 * time.Second},
	}, nil
}

func (c *Client) do(ctx context.Context, method, path string, params url.Values, out any) error {
	target := c.base + path
	var body io.Reader
	if len(params) > 0 {
		if method == http.MethodGet || method == http.MethodDelete {
			target += "?" + params.Encode()
		} else {
			body = strings.NewReader(params.Encode())
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "PVEAPIToken="+c.auth)
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return &APIError{
			Status: resp.StatusCode,
			Msg:    fmt.Sprintf("%s %s: %s %s", method, path, resp.Status, strings.TrimSpace(string(raw))),
		}
	}
	if out == nil {
		return nil
	}
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return err
	}
	if len(env.Data) == 0 || string(env.Data) == "null" {
		return nil
	}
	return json.Unmarshal(env.Data, out)
}

// rawFields разбирает объект JSON в map[string]string (все значения —
// в виде строк, независимо от исходного типа). Удобно для конфигов ВМ и
// сетевых интерфейсов, где Proxmox возвращает разнородные типы полей.
func rawFields(ctx context.Context, c *Client, method, path string) (map[string]string, error) {
	var raw map[string]json.RawMessage
	if err := c.do(ctx, method, path, nil, &raw); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		var s string
		if json.Unmarshal(v, &s) == nil {
			out[k] = s
			continue
		}
		out[k] = string(v)
	}
	return out, nil
}

// ---------- Задачи ----------

// WaitTask ждёт завершения задачи по UPID. Пустой UPID — ничего не ждём.
func (c *Client) WaitTask(ctx context.Context, node, upid string, timeout time.Duration) error {
	if upid == "" {
		return nil
	}
	deadline := time.Now().Add(timeout)
	for {
		var st struct {
			Status     string `json:"status"`
			ExitStatus string `json:"exitstatus"`
		}
		if err := c.do(ctx, http.MethodGet, "/nodes/"+node+"/tasks/"+upid+"/status", nil, &st); err != nil {
			return err
		}
		if st.Status == "stopped" {
			if st.ExitStatus == "OK" || strings.HasPrefix(st.ExitStatus, "WARNINGS") {
				return nil
			}
			return fmt.Errorf("задача завершилась с ошибкой: %s", st.ExitStatus)
		}
		if time.Now().After(deadline) {
			return errors.New("таймаут ожидания задачи")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// ---------- Узел ----------

// NodeOnline проверяет, что узел существует и отвечает на API-запросы.
func (c *Client) NodeOnline(ctx context.Context, node string) error {
	return c.do(ctx, http.MethodGet, "/nodes/"+node+"/status", nil, nil)
}

// StorageStatus — доступное/общее место на хранилище узла (в байтах).
type StorageStatus struct {
	Avail int64 `json:"avail"`
	Total int64 `json:"total"`
	Used  int64 `json:"used"`
}

// StorageStatus возвращает состояние хранилища на узле.
func (c *Client) StorageStatus(ctx context.Context, node, storage string) (StorageStatus, error) {
	var st StorageStatus
	err := c.do(ctx, http.MethodGet, fmt.Sprintf("/nodes/%s/storage/%s/status", node, storage), nil, &st)
	return st, err
}

// ---------- ВМ ----------

// NextID возвращает свободный VMID.
func (c *Client) NextID(ctx context.Context) (int, error) {
	var raw json.RawMessage
	if err := c.do(ctx, http.MethodGet, "/cluster/nextid", nil, &raw); err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.Trim(string(raw), `"`))
}

// CloneVM клонирует шаблон, возвращает UPID.
func (c *Client) CloneVM(ctx context.Context, node string, template, newID int, name, pool string, full bool) (string, error) {
	p := url.Values{}
	p.Set("newid", strconv.Itoa(newID))
	p.Set("name", name)
	if pool != "" {
		p.Set("pool", pool)
	}
	if full {
		p.Set("full", "1")
	} else {
		p.Set("full", "0")
	}
	var upid string
	err := c.do(ctx, http.MethodPost, fmt.Sprintf("/nodes/%s/qemu/%d/clone", node, template), p, &upid)
	return upid, err
}

// SetVMConfig меняет параметры ВМ (синхронный вызов).
func (c *Client) SetVMConfig(ctx context.Context, node string, vmid int, p url.Values) error {
	return c.do(ctx, http.MethodPut, fmt.Sprintf("/nodes/%s/qemu/%d/config", node, vmid), p, nil)
}

// VMConfigRaw возвращает конфиг ВМ/шаблона в виде map[поле]значение.
// Используется в preflight-проверках (наличие шаблона, размер дисков).
func (c *Client) VMConfigRaw(ctx context.Context, node string, vmid int) (map[string]string, error) {
	return rawFields(ctx, c, http.MethodGet, fmt.Sprintf("/nodes/%s/qemu/%d/config", node, vmid))
}

// IsTemplate сообщает, помечен ли vmid как шаблон.
func (c *Client) IsTemplate(ctx context.Context, node string, vmid int) (bool, error) {
	cfg, err := c.VMConfigRaw(ctx, node, vmid)
	if err != nil {
		return false, err
	}
	return cfg["template"] == "1", nil
}

// VMStatus возвращает "running", "stopped" и т.п.
func (c *Client) VMStatus(ctx context.Context, node string, vmid int) (string, error) {
	var st struct {
		Status string `json:"status"`
	}
	err := c.do(ctx, http.MethodGet, fmt.Sprintf("/nodes/%s/qemu/%d/status/current", node, vmid), nil, &st)
	return st.Status, err
}

// StartVM запускает ВМ, возвращает UPID.
func (c *Client) StartVM(ctx context.Context, node string, vmid int) (string, error) {
	var upid string
	err := c.do(ctx, http.MethodPost, fmt.Sprintf("/nodes/%s/qemu/%d/status/start", node, vmid), nil, &upid)
	return upid, err
}

// StopVM жёстко останавливает ВМ, возвращает UPID.
func (c *Client) StopVM(ctx context.Context, node string, vmid int) (string, error) {
	var upid string
	err := c.do(ctx, http.MethodPost, fmt.Sprintf("/nodes/%s/qemu/%d/status/stop", node, vmid), nil, &upid)
	return upid, err
}

// DeleteVM удаляет ВМ вместе с дисками, возвращает UPID.
func (c *Client) DeleteVM(ctx context.Context, node string, vmid int) (string, error) {
	p := url.Values{}
	p.Set("purge", "1")
	p.Set("destroy-unreferenced-disks", "1")
	var upid string
	err := c.do(ctx, http.MethodDelete, fmt.Sprintf("/nodes/%s/qemu/%d", node, vmid), p, &upid)
	return upid, err
}

// ---------- Снапшоты ----------

// CreateSnapshot создаёт снапшот ВМ, возвращает UPID.
func (c *Client) CreateSnapshot(ctx context.Context, node string, vmid int, name, desc string) (string, error) {
	p := url.Values{}
	p.Set("snapname", name)
	if desc != "" {
		p.Set("description", desc)
	}
	var upid string
	err := c.do(ctx, http.MethodPost, fmt.Sprintf("/nodes/%s/qemu/%d/snapshot", node, vmid), p, &upid)
	return upid, err
}

// DeleteSnapshot удаляет снапшот, возвращает UPID.
func (c *Client) DeleteSnapshot(ctx context.Context, node string, vmid int, name string) (string, error) {
	var upid string
	err := c.do(ctx, http.MethodDelete, fmt.Sprintf("/nodes/%s/qemu/%d/snapshot/%s", node, vmid, name), nil, &upid)
	return upid, err
}

// RollbackSnapshot откатывает ВМ к снапшоту, возвращает UPID.
func (c *Client) RollbackSnapshot(ctx context.Context, node string, vmid int, name string) (string, error) {
	var upid string
	err := c.do(ctx, http.MethodPost, fmt.Sprintf("/nodes/%s/qemu/%d/snapshot/%s/rollback", node, vmid, name), nil, &upid)
	return upid, err
}

// HasSnapshot проверяет наличие снапшота с указанным именем.
func (c *Client) HasSnapshot(ctx context.Context, node string, vmid int, name string) (bool, error) {
	var list []struct {
		Name string `json:"name"`
	}
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/nodes/%s/qemu/%d/snapshot", node, vmid), nil, &list); err != nil {
		return false, err
	}
	for _, s := range list {
		if s.Name == name {
			return true, nil
		}
	}
	return false, nil
}

// ---------- Guest agent ----------

// AgentIPs возвращает IPv4-адреса ВМ через guest agent (пусто, если агент
// недоступен или ещё не поднялся).
func (c *Client) AgentIPs(ctx context.Context, node string, vmid int) ([]string, error) {
	var res struct {
		Result []struct {
			Name        string `json:"name"`
			IPAddresses []struct {
				IPAddress     string `json:"ip-address"`
				IPAddressType string `json:"ip-address-type"`
			} `json:"ip-addresses"`
		} `json:"result"`
	}
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/nodes/%s/qemu/%d/agent/network-get-interfaces", node, vmid), nil, &res); err != nil {
		return nil, err
	}
	var ips []string
	for _, iface := range res.Result {
		if iface.Name == "lo" {
			continue
		}
		for _, a := range iface.IPAddresses {
			if a.IPAddressType == "ipv4" {
				ips = append(ips, a.IPAddress)
			}
		}
	}
	return ips, nil
}

// ---------- Пулы ----------

// Member — участник пула.
type Member struct {
	Type   string `json:"type"`
	VMID   int    `json:"vmid"`
	Node   string `json:"node"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Tags   string `json:"tags"`
}

// CreatePool создаёт пул.
func (c *Client) CreatePool(ctx context.Context, pool, comment string) error {
	p := url.Values{}
	p.Set("poolid", pool)
	if comment != "" {
		p.Set("comment", comment)
	}
	return c.do(ctx, http.MethodPost, "/pools", p, nil)
}

// PoolMembers возвращает содержимое пула (поддержаны старый и новый формат API).
func (c *Client) PoolMembers(ctx context.Context, pool string) ([]Member, error) {
	var raw json.RawMessage
	err := c.do(ctx, http.MethodGet, "/pools/"+pool, nil, &raw)
	if err != nil {
		q := url.Values{}
		q.Set("poolid", pool)
		raw = nil
		if err2 := c.do(ctx, http.MethodGet, "/pools", q, &raw); err2 != nil {
			return nil, err
		}
	}
	var obj struct {
		Members []Member `json:"members"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		return obj.Members, nil
	}
	var arr []struct {
		Members []Member `json:"members"`
	}
	if json.Unmarshal(raw, &arr) == nil {
		var all []Member
		for _, a := range arr {
			all = append(all, a.Members...)
		}
		return all, nil
	}
	return nil, nil
}

// DeletePool удаляет пул (пробует оба варианта API).
func (c *Client) DeletePool(ctx context.Context, pool string) error {
	err := c.do(ctx, http.MethodDelete, "/pools/"+pool, nil, nil)
	if err == nil {
		return nil
	}
	q := url.Values{}
	q.Set("poolid", pool)
	if err2 := c.do(ctx, http.MethodDelete, "/pools", q, nil); err2 == nil {
		return nil
	}
	return err
}

// ---------- Сеть узла ----------

// HasNetwork проверяет, есть ли на узле интерфейс с таким именем.
func (c *Client) HasNetwork(ctx context.Context, node, iface string) (bool, error) {
	var list []struct {
		Iface string `json:"iface"`
	}
	if err := c.do(ctx, http.MethodGet, "/nodes/"+node+"/network", nil, &list); err != nil {
		return false, err
	}
	for _, n := range list {
		if n.Iface == iface {
			return true, nil
		}
	}
	return false, nil
}

// NetworkIface возвращает поля существующего сетевого интерфейса узла;
// ok=false, если интерфейса с таким именем нет (это не ошибка).
func (c *Client) NetworkIface(ctx context.Context, node, iface string) (map[string]string, bool, error) {
	fields, err := rawFields(ctx, c, http.MethodGet, "/nodes/"+node+"/network/"+iface)
	if err != nil {
		if IsMissing(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return fields, true, nil
}

// CreateBridge создаёт Linux-бридж без портов (изолированная сеть).
// Изменение попадает в pending-конфиг; применить нужно через ApplyNetwork.
func (c *Client) CreateBridge(ctx context.Context, node, iface, comment string) error {
	p := url.Values{}
	p.Set("type", "bridge")
	p.Set("iface", iface)
	p.Set("autostart", "1")
	if comment != "" {
		p.Set("comments", comment)
	}
	return c.do(ctx, http.MethodPost, "/nodes/"+node+"/network", p, nil)
}

// DeleteNetwork удаляет интерфейс из pending-конфига.
func (c *Client) DeleteNetwork(ctx context.Context, node, iface string) error {
	return c.do(ctx, http.MethodDelete, "/nodes/"+node+"/network/"+iface, nil, nil)
}

// ApplyNetwork применяет сетевую конфигурацию узла (нужен ifupdown2).
func (c *Client) ApplyNetwork(ctx context.Context, node string) error {
	var upid string
	if err := c.do(ctx, http.MethodPut, "/nodes/"+node+"/network", nil, &upid); err != nil {
		return err
	}
	return c.WaitTask(ctx, node, upid, 5*time.Minute)
}

// ---------- Пользователи и права ----------

// CreateUser создаёт пользователя (например, student1@pve).
func (c *Client) CreateUser(ctx context.Context, id, password string) error {
	p := url.Values{}
	p.Set("userid", id)
	p.Set("password", password)
	return c.do(ctx, http.MethodPost, "/access/users", p, nil)
}

// DeleteUser удаляет пользователя.
func (c *Client) DeleteUser(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/access/users/"+id, nil, nil)
}

// HasUser проверяет, существует ли пользователь (это не ошибка, если нет).
func (c *Client) HasUser(ctx context.Context, id string) (bool, error) {
	err := c.do(ctx, http.MethodGet, "/access/users/"+id, nil, nil)
	if err != nil {
		if IsMissing(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// SetACL выдаёт (remove=false) или снимает (remove=true) роль пользователю на пути.
func (c *Client) SetACL(ctx context.Context, path, role, user string, remove bool) error {
	p := url.Values{}
	p.Set("path", path)
	p.Set("roles", role)
	p.Set("users", user)
	p.Set("propagate", "1")
	if remove {
		p.Set("delete", "1")
	}
	return c.do(ctx, http.MethodPut, "/access/acl", p, nil)
}
