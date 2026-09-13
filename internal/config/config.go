package config

import (
	"errors"
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Listen      string `yaml:"listen"`
	DataDir     string `yaml:"data_dir"`
	SteamCmdDir string `yaml:"steamcmd_dir"` // 空 → <data_dir>/steamcmd（main 装配时回填）
}

func Load(path string) (Config, error) {
	cfg := Config{Listen: ":8080", DataDir: "data"}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return cfg, err
	}
	if cfg.Listen == "" {
		cfg.Listen = ":8080"
	}
	if cfg.DataDir == "" {
		cfg.DataDir = "data"
	}
	return cfg, nil
}
