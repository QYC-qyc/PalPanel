// Package palsettings 实现 PalWorld 配置中心引擎：
// fields.json 为字段 schema 单一事实源，并提供 PalWorldSettings.ini 的解析、
// 生成与校验能力（含 WorldOption.sav 粗检测提示）。
package palsettings

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sync"
)

//go:embed fields.json
var fieldsJSON []byte

type rawField struct {
	Key     string   `json:"key"`
	Type    string   `json:"type"`
	Default string   `json:"default"`
	Min     *float64 `json:"min"`
	Max     *float64 `json:"max"`
	Options []string `json:"options"`
	Group   string   `json:"group"`
	Help    string   `json:"help"`
}

var (
	fieldsOnce  sync.Once
	fieldsCache []Field
	keyIndex    map[string]Field
	fieldsErr   error
)

func loadFields() {
	fieldsOnce.Do(func() {
		var raws []rawField
		if err := json.Unmarshal(fieldsJSON, &raws); err != nil {
			fieldsErr = fmt.Errorf("fields.json 解析失败: %w", err)
			return
		}
		fieldsCache = make([]Field, 0, len(raws))
		keyIndex = make(map[string]Field, len(raws))
		for _, r := range raws {
			f := Field(r)
			fieldsCache = append(fieldsCache, f)
			keyIndex[r.Key] = f
		}
	})
}
