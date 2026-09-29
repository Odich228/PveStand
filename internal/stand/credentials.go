package stand

import (
	"fmt"
	"os"
	"strings"
)

// CredentialsTable формирует текстовую таблицу «участник → пул → логин →
// пароль» по списку конфигураций, полученному из Config.Instances().
// Для обычного (без участников) стенда список из одного элемента даст
// таблицу без колонки участника — в остальном она устроена так же.
func CredentialsTable(instances []*Config) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%-16s %-24s %-24s %s\n", "УЧАСТНИК", "ПУЛ", "ЛОГИН", "ПАРОЛЬ")
	for _, inst := range instances {
		name := inst.suffix
		if name == "" {
			name = "-"
		}
		if len(inst.Users) == 0 {
			fmt.Fprintf(&b, "%-16s %-24s %-24s %s\n", name, inst.Pool, "-", "-")
			continue
		}
		for _, u := range inst.Users {
			fmt.Fprintf(&b, "%-16s %-24s %-24s %s\n", name, inst.Pool, u.ID, u.Password)
		}
	}
	return b.String()
}

// WriteCredentialsFile сохраняет таблицу логинов и паролей в файл с правами
// 0600 (файл содержит пароли открытым текстом). Если файл уже существовал с
// более широкими правами, они тоже сужаются до 0600.
func WriteCredentialsFile(path string, instances []*Config) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	// Chmod может не поддерживаться (например, на Windows) — это не фатально.
	_ = f.Chmod(0o600)
	if _, err := f.WriteString(CredentialsTable(instances)); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
