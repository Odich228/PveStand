package stand

import (
	"bytes"
	"fmt"
	"gopkg.in/yaml.v3"
	"io"
	"os"
	"path/filepath"
)

type Profile struct {
	Host      string `yaml:"host"`
	Insecure  bool   `yaml:"insecure,omitempty"`
	TokenEnv  string `yaml:"token_env,omitempty"`
	TokenFile string `yaml:"token_file,omitempty"`
	StateDir  string `yaml:"state_dir,omitempty"`
}
type Profiles struct {
	Profiles map[string]Profile `yaml:"profiles"`
}

func LoadProfile(path, name string) (Profile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Profile{}, err
	}
	var all Profiles
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err = dec.Decode(&all); err != nil {
		return Profile{}, err
	}
	var extra any
	if err = dec.Decode(&extra); err != io.EOF {
		return Profile{}, fmt.Errorf("profiles: допускается один документ")
	}
	p, ok := all.Profiles[name]
	if !ok {
		return p, fmt.Errorf("профиль %q не найден", name)
	}
	if p.TokenEnv != "" && p.TokenFile != "" {
		return p, fmt.Errorf("профиль: задайте token_env или token_file")
	}
	if p.TokenFile != "" && !filepath.IsAbs(p.TokenFile) {
		p.TokenFile = filepath.Join(filepath.Dir(path), p.TokenFile)
	}
	if p.StateDir != "" && !filepath.IsAbs(p.StateDir) {
		p.StateDir = filepath.Join(filepath.Dir(path), p.StateDir)
	}
	return p, nil
}
