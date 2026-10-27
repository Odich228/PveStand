//go:build windows

package stand

import "fmt"

func lockFile(path string) (func(), error) {
	return nil, fmt.Errorf("операции изменения поддерживаются в Linux и macOS; используйте WSL")
}
