// Package stand — описание стенда (YAML) и логика его развёртывания/удаления.
package stand

import (
	"bytes"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config — описание стенда.
type Config struct {
	Name      string    `yaml:"name"`
	Node      string    `yaml:"node"`       // имя узла PVE
	Pool      string    `yaml:"pool"`       // выделенный пул под стенд
	FullClone bool      `yaml:"full_clone"` // false = связанные клоны (быстрее)
	Networks  []Network `yaml:"networks"`
	VMs       []VM      `yaml:"vms"`
	Users     []User    `yaml:"users"`

	// Participants — если задано, стенд разворачивается отдельной копией на
	// каждого участника/команду: свой пул (с суффиксом id), свои ВМ (тоже с
	// суффиксом в имени), свои бриджи (со сдвинутым номером) и свои
	// пользователи. См. Config.Instances().
	Participants []Participant `yaml:"participants"`

	// suffix — служебное поле, не читается из YAML. Проставляется в
	// Instances() для конкретной копии стенда и добавляется к именам ВМ.
	suffix string `yaml:"-"`
}

// Participant — одна копия стенда (участник или команда).
type Participant struct {
	ID    string `yaml:"id"`    // используется как суффикс пула/ВМ, только латиница/цифры/дефис
	Users []User `yaml:"users"` // если не заданы — используются пользователи верхнего уровня
}

// Network — изолированный Linux-бридж на узле.
type Network struct {
	Bridge  string `yaml:"bridge"` // vmbr101
	Comment string `yaml:"comment"`
}

// VM — группа одинаковых машин, клонируемых из шаблона.
type VM struct {
	Name     string `yaml:"name"`
	Template int    `yaml:"template"` // VMID шаблона
	Count    int    `yaml:"count"`    // сколько копий (по умолчанию 1)
	Cores    int    `yaml:"cores"`
	Memory   int    `yaml:"memory"` // МБ
	NICs     []NIC  `yaml:"nics"`
	Start    bool   `yaml:"start"`
}

// NIC — сетевой адаптер ВМ.
type NIC struct {
	Bridge string `yaml:"bridge"`
	Model  string `yaml:"model"` // по умолчанию virtio
}

// User — пользователь, получающий роль на пул стенда.
type User struct {
	ID       string `yaml:"id"` // student1@pve
	Password string `yaml:"password"`
	Role     string `yaml:"role"` // по умолчанию PVEVMUser
}

// InstanceName — имя конкретной копии ВМ. suffix (если не пуст) добавляется
// в конце — так помечаются копии стенда для разных участников.
func (v VM) InstanceName(i int, suffix string) string {
	name := v.Name
	if v.Count != 1 {
		name = fmt.Sprintf("%s-%d", v.Name, i)
	}
	if suffix != "" {
		name = name + "-" + suffix
	}
	return name
}

const (
	// maxVMName — предел длины имени ВМ в Proxmox (DNS-имя).
	maxVMName = 63
	// DefaultRole — роль пользователя на пул, если в YAML не указана другая.
	DefaultRole = "PVEVMUser"
)

var (
	bridgeRe = regexp.MustCompile(`^vmbr([0-9]{1,4})$`)
	nameRe   = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?$`)
	// idRe — допустимые символы для имени стенда и пула (пул в API Proxmox
	// принимает только [A-Za-z0-9_-]; имя стенда идёт в тег и комментарий).
	idRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)
	// nicBridgeRe / nicModelRe защищают строку netN ("model,bridge=X") от
	// подстановки лишних опций через запятую.
	nicBridgeRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
	nicModelRe  = regexp.MustCompile(`^[a-z0-9]+$`)
	tagRe       = regexp.MustCompile(`[^a-z0-9_-]+`)
)

// Load читает и проверяет YAML-файл стенда.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true) // опечатки в ключах = ошибка
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	for i := range c.VMs {
		if c.VMs[i].Count == 0 {
			c.VMs[i].Count = 1
		}
		for j := range c.VMs[i].NICs {
			if c.VMs[i].NICs[j].Model == "" {
				c.VMs[i].NICs[j].Model = "virtio"
			}
		}
	}
	for i := range c.Users {
		if c.Users[i].Role == "" {
			c.Users[i].Role = DefaultRole
		}
	}
	for i := range c.Participants {
		for j := range c.Participants[i].Users {
			if c.Participants[i].Users[j].Role == "" {
				c.Participants[i].Users[j].Role = DefaultRole
			}
		}
	}
}

// Validate проверяет конфигурацию до обращения к Proxmox.
func (c *Config) Validate() error {
	if c.Name == "" || c.Node == "" || c.Pool == "" {
		return fmt.Errorf("поля name, node и pool обязательны")
	}
	if !idRe.MatchString(c.Name) {
		return fmt.Errorf("name %q: допустимы только латиница, цифры, «-» и «_» (из имени строится тег стенда, по которому ВМ находятся при удалении)", c.Name)
	}
	if !idRe.MatchString(c.Pool) {
		return fmt.Errorf("pool %q: Proxmox принимает только латиницу, цифры, «-» и «_»", c.Pool)
	}
	seen := map[string]bool{}
	for _, n := range c.Networks {
		if !bridgeRe.MatchString(n.Bridge) {
			return fmt.Errorf("сеть %q: имя бриджа должно быть вида vmbr101", n.Bridge)
		}
		if seen[n.Bridge] {
			return fmt.Errorf("сеть %q указана дважды", n.Bridge)
		}
		seen[n.Bridge] = true
	}
	if len(c.VMs) == 0 {
		return fmt.Errorf("в стенде нет ни одной ВМ")
	}
	names := map[string]bool{}
	for _, v := range c.VMs {
		if !nameRe.MatchString(v.Name) {
			return fmt.Errorf("ВМ %q: имя должно состоять из латиницы, цифр и дефисов", v.Name)
		}
		if v.Template <= 0 {
			return fmt.Errorf("ВМ %q: не указан template (VMID шаблона)", v.Name)
		}
		if v.Count < 1 {
			return fmt.Errorf("ВМ %q: count должен быть >= 1", v.Name)
		}
		for i := 1; i <= v.Count; i++ {
			n := v.InstanceName(i, "")
			if names[n] {
				return fmt.Errorf("имя ВМ %q повторяется", n)
			}
			names[n] = true
		}
		for _, nic := range v.NICs {
			if nic.Bridge == "" {
				return fmt.Errorf("ВМ %q: у сетевого адаптера не указан bridge", v.Name)
			}
			if !nicBridgeRe.MatchString(nic.Bridge) {
				return fmt.Errorf("ВМ %q: недопустимое имя bridge %q у сетевого адаптера", v.Name, nic.Bridge)
			}
			if !nicModelRe.MatchString(nic.Model) {
				return fmt.Errorf("ВМ %q: недопустимая модель сетевого адаптера %q", v.Name, nic.Model)
			}
		}
	}
	if err := validateUsers(c.Users); err != nil {
		return err
	}
	pids := map[string]bool{}
	for _, p := range c.Participants {
		if !nameRe.MatchString(p.ID) {
			return fmt.Errorf("участник %q: id должен состоять из латиницы, цифр и дефисов", p.ID)
		}
		if pids[p.ID] {
			return fmt.Errorf("участник %q указан дважды", p.ID)
		}
		pids[p.ID] = true
		if err := validateUsers(p.Users); err != nil {
			return fmt.Errorf("участник %q: %w", p.ID, err)
		}
	}

	// Проверки, зависящие от суффиксов участников: считаем по реальным копиям.
	owners := map[string]string{} // user id -> участник
	for _, inst := range c.Instances() {
		who := inst.suffix
		for _, n := range inst.Networks {
			if !bridgeRe.MatchString(n.Bridge) {
				return fmt.Errorf("участник %q: для бриджа получился недопустимый номер %q — уменьшите номера бриджей или число участников (максимум vmbr9999)", who, n.Bridge)
			}
		}
		for _, v := range inst.VMs {
			for i := 1; i <= v.Count; i++ {
				n := v.InstanceName(i, inst.suffix)
				if len(n) > maxVMName {
					return fmt.Errorf("имя ВМ %q (%d симв.) длиннее %d символов — сократите name ВМ или id участника", n, len(n), maxVMName)
				}
			}
		}
		if inst.isParticipant() {
			for _, u := range inst.Users {
				if prev, dup := owners[u.ID]; dup {
					return fmt.Errorf("пользователь %q достался и участнику %q, и участнику %q: у каждого участника должны быть свои users (иначе удаление одной копии стенда сломает доступ другой)", u.ID, prev, who)
				}
				owners[u.ID] = who
			}
		}
	}
	return nil
}

func validateUsers(users []User) error {
	seen := map[string]bool{}
	for _, u := range users {
		if err := validateUser(u); err != nil {
			return err
		}
		if seen[u.ID] {
			return fmt.Errorf("пользователь %q указан дважды", u.ID)
		}
		seen[u.ID] = true
	}
	return nil
}

func validateUser(u User) error {
	if len(u.ID) < 3 || !strings.Contains(u.ID, "@") {
		return fmt.Errorf("пользователь %q: нужен формат имя@realm, например student1@pve", u.ID)
	}
	if len(u.Password) < 8 {
		return fmt.Errorf("пользователь %q: пароль короче 8 символов", u.ID)
	}
	return nil
}

// HasParticipants сообщает, задан ли режим нескольких копий стенда.
func (c *Config) HasParticipants() bool { return len(c.Participants) > 0 }

// ParticipantID — id участника, которому принадлежит эта копия стенда
// (пусто для обычного стенда). Заполняется в Instances().
func (c *Config) ParticipantID() string { return c.suffix }

func (c *Config) isParticipant() bool { return c.suffix != "" }

// bridgeNum достаёт номер из имени бриджа vmbrNNN.
func bridgeNum(b string) (int, bool) {
	m := bridgeRe.FindStringSubmatch(b)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	return n, err == nil
}

// bridgeStride — на сколько сдвигаются номера бриджей от одного участника
// к следующему: размах номеров в networks (max-min+1). Диапазоны разных
// участников не пересекаются: при vmbr101 и vmbr102 у второго участника
// будут vmbr103 и vmbr104, у третьего — vmbr105 и vmbr106.
func (c *Config) bridgeStride() int {
	lo, hi, ok := 0, 0, false
	for _, n := range c.Networks {
		v, good := bridgeNum(n.Bridge)
		if !good {
			continue
		}
		if !ok || v < lo {
			lo = v
		}
		if !ok || v > hi {
			hi = v
		}
		ok = true
	}
	if !ok {
		return 0
	}
	return hi - lo + 1
}

// mapBridge возвращает имя бриджа для участника с порядковым номером idx.
// У первого участника имена остаются как в YAML.
func mapBridge(name string, idx, stride int) string {
	if idx == 0 || stride == 0 {
		return name
	}
	v, ok := bridgeNum(name)
	if !ok {
		return name
	}
	return fmt.Sprintf("vmbr%d", v+idx*stride)
}

// Instances возвращает список конфигураций для развёртывания/удаления:
// либо один элемент — сам cfg (обычный режим), либо по одному на каждого
// участника из Participants. В копии участника:
//   - пул и имя получают суффикс "-id", имена ВМ — тоже;
//   - бриджи из networks получают собственные номера (см. bridgeStride), а
//     ссылки на них в nics ВМ переписываются — команды не делят L2-сегмент,
//     и удаление одной копии не трогает сети остальных. Бриджи, которых нет в
//     networks (например, существующий vmbr0), не меняются;
//   - пользователи берутся из участника, если заданы, иначе — из верхнего
//     уровня конфига (Validate не даст двум участникам получить одних и тех
//     же пользователей).
//
// Копии независимы: срезы Networks и VMs не разделяются с исходным конфигом.
func (c *Config) Instances() []*Config {
	if len(c.Participants) == 0 {
		return []*Config{c}
	}
	stride := c.bridgeStride()
	out := make([]*Config, 0, len(c.Participants))
	for idx, p := range c.Participants {
		cp := *c
		cp.Pool = c.Pool + "-" + p.ID
		cp.Name = c.Name + "-" + p.ID
		cp.suffix = p.ID
		cp.Participants = nil

		bmap := map[string]string{}
		cp.Networks = make([]Network, len(c.Networks))
		for j, n := range c.Networks {
			nb := mapBridge(n.Bridge, idx, stride)
			bmap[n.Bridge] = nb
			n.Bridge = nb
			n.Comment = strings.TrimSpace(n.Comment + " " + p.ID)
			cp.Networks[j] = n
		}
		cp.VMs = make([]VM, len(c.VMs))
		for j, v := range c.VMs {
			nics := make([]NIC, len(v.NICs))
			for k, nic := range v.NICs {
				if nb, ok := bmap[nic.Bridge]; ok {
					nic.Bridge = nb
				}
				nics[k] = nic
			}
			v.NICs = nics
			cp.VMs[j] = v
		}
		if len(p.Users) > 0 {
			cp.Users = p.Users
		}
		out = append(out, &cp)
	}
	return out
}

// ExpectedVMNames — имена всех ВМ, которые должны быть в этом стенде
// согласно конфигу. Используется при безопасном удалении, чтобы не
// затронуть ВМ, которых в описании стенда нет.
func (c *Config) ExpectedVMNames() map[string]bool {
	out := map[string]bool{}
	for _, v := range c.VMs {
		for i := 1; i <= v.Count; i++ {
			out[v.InstanceName(i, c.suffix)] = true
		}
	}
	return out
}

// Tag — метка, которой помечаются ВМ этого стенда (проставляется в
// SetVMConfig при развёртывании). Используется при безопасном удалении как
// второй, независимый от имени признак «это наша ВМ».
func (c *Config) Tag() string {
	return "pvestand-" + tagRe.ReplaceAllString(strings.ToLower(c.Name), "-")
}
