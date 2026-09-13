# PalPanel

面向社区的幻兽帕鲁（Palworld）专用服务器开源管理面板。

> ⚠️ 当前处于早期开发阶段（V1-M2 进程管理与安装能力已完成，均为后端 API；前端在后续里程碑）。

## 功能规划

- **服管基础盘**：SteamCMD 安装/更新、启停守护、崩溃自愈、备份恢复、监控
- **配置中心**：120+ 世界参数 schema 驱动可视化编辑、预设模板
- **运行期管理**：在线玩家、踢/封/公告（官方 REST API + RCON 双通道）
- **多实例**：一台面板管理同主机多个游戏服实例，RBAC + 按实例分权
- **存档管理**：解析浏览（独立子进程隔离，只读）

完整设计见 [docs/superpowers/specs](docs/superpowers/specs/)。

## 快速开始（当前阶段：后端 API）

```bash
# 源码运行（Go 1.26+）
go run .                 # 默认监听 :8080，数据目录 ./data

# 首次初始化
curl -X POST http://127.0.0.1:8080/api/v1/setup \
  -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"your-password"}'

# Docker
docker build -t palpanel .
docker run -d -p 8080:8080 -v palpanel-data:/app/data palpanel
```

## 实例管理 API（M2）

全部挂在 `/api/v1/instances/:id` 下，需 Bearer token 与对应权限码（详见 docs/superpowers/specs）：

| 端点 | 方法 | 权限 | 说明 |
|---|---|---|---|
| `/start` `/stop` `/restart` | POST | instance.start / instance.stop / 两者 | 启动/优雅停机（REST→RCON→强杀链）/重启 |
| `/install` | POST | instance.install | 异步 job 安装/校验（SteamCMD），进行中重复请求 409 |
| `/update-check` | GET | instance.upgrade | `{local, remote, up_to_date}` |
| `/update` | POST | instance.upgrade | 运行中先优雅停服 → 更新 → 保持停止 |
| `/status` | GET | instance.read | `{status, pid, started_at, restarts, metrics?}` |
| `/logs?tail=200` | GET | instance.read | console.log 末 N 行（上限 1000） |
| `/players` | GET | player.read | 在线玩家（官方 REST API） |
| `/players/kick` `/players/ban` | POST | player.kick / player.ban | `{"uid":"1"}` |
| `/announce` `/save` | POST | player.announce / player.save | 广播 / 手动存档 |

实时事件（状态变更/日志流/任务进度）经 `GET /api/ws?token=<JWT>` 推送。

## 构建

```bash
make test    # go vet + test -race（CI 同款）
make build   # 本机二进制
make cross   # windows/amd64 + linux/amd64 + linux/arm64
```

发布：推送 `v*` tag 自动构建 GitHub Release（三平台二进制 + sha256）并推送 Docker 镜像到阿里云 ACR。

## 技术栈

Go 1.26 · gin · SQLite（modernc，纯 Go 无 CGO）· JWT · bcrypt · AES-GCM

## License

Apache-2.0（见 [LICENSE](LICENSE)）

## 文档

设计文档、开发约定与依赖许可核查维护于仓库本地 `docs/` 目录（不随公开仓库分发）。
