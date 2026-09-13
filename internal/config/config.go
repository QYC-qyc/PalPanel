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
	Backup      Backup `yaml:"backup"`
}

// Backup 备份相关面板配置。
type Backup struct {
	// KeepCount 滚动清理保留条数（指针区分「未配置」与显式 0=不清理）；
	// 未配置时经 KeepCountOrDefault 取默认 20。
	KeepCount *int `yaml:"keep_count"`
}

// KeepCountOrDefault 返回保留条数：未配置时取 def。
func (b Backup) KeepCountOrDefault(def int) int {
	if b.KeepCount != nil {
		return *b.KeepCount
	}
	return def
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
