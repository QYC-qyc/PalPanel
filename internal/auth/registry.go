package auth

// 权限码：领域.动作。实例级权限判定时需要实例 grant；全局权限不需要。
const (
	PUserManage      = "user.manage"
	PRoleManage      = "role.manage"
	PAuditRead       = "audit.read"
	PInstanceManage  = "instance.manage" // 全局：创建/删除/看全部实例
	PInstanceRead    = "instance.read"
	PInstanceUpdate  = "instance.update"
	PInstanceStart   = "instance.start"
	PInstanceStop    = "instance.stop"
	PInstanceInstall = "instance.install"
	PInstanceUpgrade = "instance.upgrade"
	PBackupRead      = "backup.read"
	PBackupCreate    = "backup.create"
	PBackupRestore   = "backup.restore"
	PConfigRead      = "config.read"
	PConfigWrite     = "config.write"
	PPlayerRead      = "player.read"
	PPlayerAnnounce  = "player.announce"
	PPlayerSave      = "player.save"
	PPlayerKick      = "player.kick"
	PPlayerBan       = "player.ban"
	PScheduleManage  = "schedule.manage"
)

var AllPermissions = []string{
	PUserManage, PRoleManage, PAuditRead, PInstanceManage,
	PInstanceRead, PInstanceUpdate, PInstanceStart, PInstanceStop,
	PInstanceInstall, PInstanceUpgrade,
	PBackupRead, PBackupCreate, PBackupRestore,
	PConfigRead, PConfigWrite,
	PPlayerRead, PPlayerAnnounce, PPlayerSave, PPlayerKick, PPlayerBan,
	PScheduleManage,
}

// AllPermissionsWithout 返回排除 exclude 之后的全部权限码（角色编辑用）。
func AllPermissionsWithout(exclude ...string) []string {
	var out []string
	for _, p := range AllPermissions {
		skip := false
		for _, e := range exclude {
			if p == e {
				skip = true
				break
			}
		}
		if !skip {
			out = append(out, p)
		}
	}
	return out
}

var builtinRoles = []struct {
	Name, Desc string
	Perms      []string
}{
	{"admin", "超级管理员", AllPermissions},
	{"operator", "运维", []string{
		PInstanceRead, PInstanceUpdate, PInstanceStart, PInstanceStop,
		PInstanceInstall, PInstanceUpgrade,
		PBackupRead, PBackupCreate, PBackupRestore,
		PConfigRead, PConfigWrite, PPlayerRead, PPlayerKick, PPlayerBan,
		PPlayerAnnounce, PPlayerSave,
		PScheduleManage, PAuditRead,
	}},
	{"viewer", "只读", []string{
		PInstanceRead, PBackupRead, PConfigRead, PPlayerRead, PAuditRead,
	}},
}
