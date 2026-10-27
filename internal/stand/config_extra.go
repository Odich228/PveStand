package stand

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

func yamlHint(data []byte, err error) string {
	lines := strings.Split(string(data), "\n")
	// The YAML parser can report line 1 for an incorrectly indented sequence.
	expected := -1
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "- name:") {
			n := len(line) - len(strings.TrimLeft(line, " "))
			if expected < 0 {
				expected = n
			} else if n != expected {
				return fmt.Sprintf("\nПроверьте отступ элемента списка в строке %d: %d пробелов, у соседних элементов %d", i+1, n, expected)
			}
		}
	}
	m := regexp.MustCompile(`line (\d+)`).FindStringSubmatch(err.Error())
	if m != nil {
		n, _ := strconv.Atoi(m[1])
		if n > 0 && n <= len(lines) {
			// Only show indentation and key, never values (they may contain passwords).
			line := lines[n-1]
			key, _, ok := strings.Cut(strings.TrimSpace(line), ":")
			if ok {
				return fmt.Sprintf("\nСтрока %d: отступ %d, ключ %q; проверьте также предыдущие строки", n, len(line)-len(strings.TrimLeft(line, " \t")), key)
			}
		}
	}
	return "\nПроверьте отступы: используйте пробелы, а не табуляцию"
}
func (c *Config) resolveSecrets(base string) error { return c.resolveSecretRefs(base, true) }
func (c *Config) resolveSecretRefs(base string, read bool) error {
	resolve := func(users []User, read bool) error {
		for i := range users {
			u := &users[i]
			n := 0
			for _, v := range []string{u.Password, u.PasswordEnv, u.PasswordFile} {
				if v != "" {
					n++
				}
			}
			if n != 1 {
				return fmt.Errorf("пользователь %s: задайте ровно одно из password/password_env/password_file", u.ID)
			}
			if !read {
				continue
			}
			if u.PasswordEnv != "" {
				v, ok := os.LookupEnv(u.PasswordEnv)
				if !ok {
					return fmt.Errorf("не задана переменная %s для %s", u.PasswordEnv, u.ID)
				}
				u.Password = v
			}
			if u.PasswordFile != "" {
				path := u.PasswordFile
				if !filepath.IsAbs(path) {
					path = filepath.Join(base, path)
				}
				v, err := ReadSecret(path)
				if err != nil {
					return fmt.Errorf("пароль %s: %w", u.ID, err)
				}
				u.Password = v
			}
			if len(u.Password) < 8 {
				return fmt.Errorf("пользователь %s: пароль короче 8 символов", u.ID)
			}
		}
		return nil
	}
	topUsed := len(c.Participants) == 0
	for _, p := range c.Participants {
		if (c.ParticipantOnly == "" || c.ParticipantOnly == p.ID) && len(p.Users) == 0 {
			topUsed = true
		}
	}
	if err := resolve(c.Users, read && topUsed); err != nil {
		return err
	}
	for i := range c.Participants {
		if err := resolve(c.Participants[i].Users, read && (c.ParticipantOnly == "" || c.ParticipantOnly == c.Participants[i].ID)); err != nil {
			return err
		}
	}
	return nil
}
func ReadSecret(path string) (string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 {
		return "", fmt.Errorf("файл секрета %s должен быть обычным файлом с правами 0600 или 0400", path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(b), "\r\n"), nil
}
func (c *Config) Secrets() []string {
	var s []string
	for _, u := range c.Users {
		s = append(s, u.Password)
	}
	for _, p := range c.Participants {
		for _, u := range p.Users {
			s = append(s, u.Password)
		}
	}
	return s
}
func validAddress(s string) error {
	host, port, err := net.SplitHostPort(s)
	if err != nil || host == "" {
		return fmt.Errorf("ready_address должен иметь формат host:port")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("неверный порт ready_address")
	}
	return nil
}
func validateDependencies(vms []VM) error {
	byName := map[string]VM{}
	for _, v := range vms {
		if _, ok := byName[v.Name]; ok {
			return fmt.Errorf("группа ВМ %q указана дважды", v.Name)
		}
		byName[v.Name] = v
	}
	colors := map[string]int{}
	var visit func(string) error
	visit = func(name string) error {
		if colors[name] == 1 {
			return fmt.Errorf("цикл depends_on у %s", name)
		}
		if colors[name] == 2 {
			return nil
		}
		colors[name] = 1
		for _, dep := range byName[name].DependsOn {
			v, ok := byName[dep]
			if !ok {
				return fmt.Errorf("%s: зависимость %s не найдена", name, dep)
			}
			if !v.Start {
				return fmt.Errorf("%s: зависимость %s должна иметь start: true", name, dep)
			}
			if err := visit(dep); err != nil {
				return err
			}
		}
		colors[name] = 2
		return nil
	}
	for _, v := range vms {
		if err := visit(v.Name); err != nil {
			return err
		}
	}
	return nil
}
