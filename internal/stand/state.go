package stand

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"pvestand/internal/pve"
)

var DefaultStateDir = ".pvestand"

type Resource struct {
	Kind        string `json:"kind"`
	Template    int    `json:"template,omitempty"`
	RequestedAt int64  `json:"requested_at,omitempty"`
	Scope       string `json:"scope"`
	Name        string `json:"name"`
	Node        string `json:"node"`
	VMID        int    `json:"vmid,omitempty"`
	UPID        string `json:"upid,omitempty"`
	Operation   string `json:"operation,omitempty"`
	Stage       string `json:"stage,omitempty"`
}
type journal struct {
	Version   int                 `json:"version"`
	Owner     string              `json:"owner"`
	Endpoint  string              `json:"endpoint"`
	Resources map[string]Resource `json:"resources"`
}
type Runtime struct {
	mu   sync.Mutex
	path string
	data journal
}

func Bind(c *pve.Client, cfg *Config) error {
	if cfg.Runtime != nil {
		return nil
	}
	h := sha256.Sum256([]byte(c.Endpoint() + "\x00" + cfg.Node + "\x00" + cfg.Pool + "\x00" + cfg.Name))
	r := &Runtime{path: filepath.Join(DefaultStateDir, hex.EncodeToString(h[:12])+".json")}
	if err := r.load(c.Endpoint()); err != nil {
		return err
	}
	cfg.Runtime = r
	c.SetSecrets(cfg.Secrets())
	return nil
}
func (r *Runtime) load(endpoint string) error {
	b, err := os.ReadFile(r.path)
	if os.IsNotExist(err) {
		id := make([]byte, 16)
		if _, err := rand.Read(id); err != nil {
			return err
		}
		r.data = journal{Version: 1, Owner: hex.EncodeToString(id), Endpoint: endpoint, Resources: map[string]Resource{}}
		return nil
	}
	if err != nil {
		return err
	}
	var j journal
	if err = json.Unmarshal(b, &j); err != nil {
		return fmt.Errorf("журнал %s повреждён: %w; восстановите резервную копию", r.path, err)
	}
	if j.Version != 1 || j.Owner == "" || j.Endpoint != endpoint || j.Resources == nil {
		return fmt.Errorf("неподходящий журнал состояния %s", r.path)
	}
	r.data = j
	return nil
}
func (r *Runtime) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(r.path), 0700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(r.data, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(r.path), ".state-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err = f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, r.path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(r.path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
func (r *Runtime) acquire() (func(), error) {
	if err := os.MkdirAll(filepath.Dir(r.path), 0700); err != nil {
		return nil, err
	}
	release, err := lockFile(r.path + ".lock")
	if err != nil {
		return nil, err
	}
	if err = r.load(r.data.Endpoint); err != nil {
		release()
		return nil, err
	}
	r.mu.Lock()
	err = r.saveLocked()
	r.mu.Unlock()
	if err != nil {
		release()
		return nil, err
	}
	return release, nil
}
func (c *Config) scope() string { return c.Node + "/" + c.Pool + "/" + c.Name }
func (c *Config) marker() string {
	h := sha256.Sum256([]byte(c.scope()))
	return "pvestand:" + c.Runtime.data.Owner + ":" + hex.EncodeToString(h[:8])
}
func (c *Config) key(kind, name string) string { return c.scope() + "/" + kind + "/" + name }
func (c *Config) record(kind, name string) (Resource, bool) {
	if c.Runtime == nil {
		return Resource{}, false
	}
	r := c.Runtime
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.data.Resources[c.key(kind, name)]
	return v, ok
}
func (c *Config) put(v Resource) error {
	r := c.Runtime
	r.mu.Lock()
	defer r.mu.Unlock()
	v.Scope = c.scope()
	v.Node = c.Node
	r.data.Resources[c.key(v.Kind, v.Name)] = v
	return r.saveLocked()
}
func (c *Config) forget(kind, name string) error {
	r := c.Runtime
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.data.Resources, c.key(kind, name))
	return r.saveLocked()
}
func (c *Config) resources() []Resource {
	r := c.Runtime
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Resource
	for _, v := range r.data.Resources {
		if v.Scope == c.scope() {
			out = append(out, v)
		}
	}
	return out
}
