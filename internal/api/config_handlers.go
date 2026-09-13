// 配置中心端点：
//   - GET /instances/{id}/config（config.read）：schema 全量字段 + 当前 ini 值
//     （含未管理键原样返回）+ WorldOption 粗检测提示（ini 不存在时提示将生成新文件）。
//   - PUT /instances/{id}/config（config.write）：逐键校验（未知键 400、非法值 400、
//     空串值视为未设置跳过——M3-T7 裁决注记）→ LoadGameINI → Serialize → SaveGameINI
//     → 运行中实例尽力 RCON 热应用（失败静默）→ 审计（detail=变更键列表）。
package api

import (
	"net/http"
	"os"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"

	"palpanel/internal/auth"
	"palpanel/internal/instance"
	"palpanel/internal/palsettings"
	"palpanel/internal/supervisor"
)

func (d Deps) registerConfig(v1 *gin.RouterGroup) {
	g := v1.Group("/instances", d.authRequired())
	g.GET("/:id/config", d.requirePerm(auth.PConfigRead), d.handleConfigGet)
	g.PUT("/:id/config", d.requirePerm(auth.PConfigWrite), d.handleConfigPut)
}

// configFieldDTO 是 schema 字段的对外形态（小写 json 键，前后端契约）。
type configFieldDTO struct {
	Key     string   `json:"key"`
	Type    string   `json:"type"`
	Default string   `json:"default"`
	Min     *float64 `json:"min"`
	Max     *float64 `json:"max"`
	Options []string `json:"options"`
	Group   string   `json:"group"`
	Help    string   `json:"help"`
}

// noINIHint ini 不存在（新实例首次）时的提示文案。
const noINIHint = "该实例还没有配置文件，保存后将生成新配置文件"

func (d Deps) handleConfigGet(c *gin.Context) {
	in, ok := d.loadInstance(c)
	if !ok {
		return
	}
	values, err := palsettings.LoadGameINI(in.GameDir)
	if err != nil {
		fail(c, http.StatusInternalServerError, "读取配置文件失败")
		return
	}
	fields := []configFieldDTO{}
	for _, f := range palsettings.Fields() {
		fields = append(fields, configFieldDTO{Key: f.Key, Type: f.Type, Default: f.Default,
			Min: f.Min, Max: f.Max, Options: f.Options, Group: f.Group, Help: f.Help})
	}
	// WorldOption 粗检测：ini 存在且 WorldOption.sav 更新时提示覆盖风险
	hint := ""
	if info, err := os.Stat(palsettings.GameINIPath(in.GameDir)); err != nil {
		if os.IsNotExist(err) {
			hint = noINIHint
		}
	} else {
		hint = palsettings.WorldOptionHint(in.GameDir, info.ModTime())
	}
	c.JSON(http.StatusOK, gin.H{"fields": fields, "values": values, "world_option_hint": hint})
}

func (d Deps) handleConfigPut(c *gin.Context) {
	in, ok := d.loadInstance(c)
	if !ok {
		return
	}
	var req struct {
		Values map[string]string `json:"values"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Values == nil {
		fail(c, http.StatusBadRequest, "参数不合法：values 必填")
		return
	}
	// 逐键校验：未知键 400；空串视为未设置跳过；非法值 400 带键名
	changes := make(map[string]string, len(req.Values))
	for k, v := range req.Values {
		f, known := palsettings.FieldByKey(k)
		if !known {
			fail(c, http.StatusBadRequest, "未知配置项: "+k)
			return
		}
		if v == "" {
			continue // 空 override=未设置（M3-T7 裁决注记）
		}
		if err := palsettings.Validate(f, v); err != nil {
			fail(c, http.StatusBadRequest, err.Error())
			return
		}
		changes[k] = v
	}
	if len(changes) == 0 {
		fail(c, http.StatusBadRequest, "没有需要保存的配置")
		return
	}
	existing, err := palsettings.LoadGameINI(in.GameDir)
	if err != nil {
		fail(c, http.StatusInternalServerError, "读取配置文件失败")
		return
	}
	if err := palsettings.SaveGameINI(in.GameDir, palsettings.Serialize(changes, existing)); err != nil {
		fail(c, http.StatusInternalServerError, "保存配置文件失败")
		return
	}
	d.rconHotApply(in, changes)
	keys := sortedKeys(changes)
	d.audit(c, in.ID, "config.write", "变更: "+strings.Join(keys, ", "))
	c.JSON(http.StatusOK, gin.H{"ok": true, "hint": "重启实例后完全生效"})
}

// rconHotApply 运行中实例经 RCON 逐键热应用 OptionSettings（尽力而为语义）：
// 守护器未运行、RCON 未启用/拨号失败、单键 Exec 失败一律静默跳过，不阻断保存；
// RCON OptionSettings 不支持全部键，完全生效仍需重启（响应 hint 已提示）。
func (d Deps) rconHotApply(in instance.Instance, changes map[string]string) {
	if d.Sup == nil {
		return
	}
	st, found := d.Sup.Status(in.ID)
	if !found || (st.State != supervisor.StateRunning && st.State != supervisor.StateStarting) {
		return
	}
	conn, err := d.rconClient(in)
	if err != nil {
		return
	}
	defer conn.Close()
	for _, k := range sortedKeys(changes) {
		_, _ = conn.Exec("OptionSettings " + k + " " + changes[k])
	}
}

// sortedKeys 稳定输出键序（审计 detail 与 RCON 命令顺序确定）。
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
