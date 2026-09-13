package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"

	"palpanel/internal/api"
	"palpanel/internal/audit"
	"palpanel/internal/auth"
	"palpanel/internal/backup"
	"palpanel/internal/config"
	"palpanel/internal/db"
	"palpanel/internal/event"
	"palpanel/internal/gateway"
	"palpanel/internal/instance"
	"palpanel/internal/installer"
	"palpanel/internal/job"
	"palpanel/internal/scheduler"
	"palpanel/internal/steamcmd"
	"palpanel/internal/supervisor"
)

// version 由构建注入：-ldflags "-X main.version=..."（见 scripts/build.sh）。
var version = "dev"

// httpDownload 是生产 steamcmd.Downloader：流式下载到临时文件后改名，
// 避免半截包被当作可用分发包。（不单测：安装链路测试注入假 DL。）
func httpDownload(ctx context.Context, url, dest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("下载 %s: HTTP %d", url, resp.StatusCode)
	}
	tmp := dest + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dest)
}

// restProber 把 api.Deps.RESTFor 适配为 supervisor.Prober（约 15 行，放 main 层避免
// gateway 依赖 instance 包）：REST 可用走官方 /info 探活；未启用 REST 的实例退化为本机
// 游戏端口 TCP 探活，保证也能进入 running。
type restProber struct {
	restFor func(instance.Instance) (*gateway.Client, error)
}

func (p restProber) Probe(ctx context.Context, inst instance.Instance) error {
	if client, err := p.restFor(inst); err == nil {
		return client.Probe(ctx)
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", inst.GamePort))
	if err != nil {
		return err
	}
	return conn.Close()
}

// autostart 在面板组件就绪后启动带 autostart 标记的实例（失败仅记录，不阻断面板启动）。
// 语义：面板异常退出残留的 "running" 状态视为孤儿进程现场，跳过以免与残留进程
// 端口冲突，需人工确认处理后再手动启动。
func autostart(store *instance.Store, start func(int64) error) {
	list, err := store.List()
	if err != nil {
		log.Printf("autostart: 读取实例失败: %v", err)
		return
	}
	for _, in := range list {
		if !in.Autostart || in.Status == supervisor.StateRunning {
			continue
		}
		if err := start(in.ID); err != nil {
			log.Printf("autostart: 实例 %d(%s) 启动失败: %v", in.ID, in.Name, err)
		}
	}
}

func main() {
	cfg, err := config.Load("config.yaml")
	if err != nil {
		log.Fatal(err)
	}
	if cfg.SteamCmdDir == "" {
		cfg.SteamCmdDir = filepath.Join(cfg.DataDir, "steamcmd")
	}
	database, err := db.Open(cfg.DataDir)
	if err != nil {
		log.Fatal(err)
	}
	if err := db.Migrate(database); err != nil {
		log.Fatal(err)
	}
	if err := auth.Seed(database); err != nil {
		log.Fatal(err)
	}
	secret, err := auth.LoadOrCreateSecret(cfg.DataDir)
	if err != nil {
		log.Fatal(err)
	}

	hub := event.NewHub()
	jobs := job.NewManager(hub)
	instStore := instance.New(database)
	sup := supervisor.New(database, instStore, hub)
	sup.LogRoot = cfg.DataDir
	sup.StartFn = supervisor.NewOSStarter // 生产启动器（os/exec + 树杀适配）

	backupStore := backup.NewStore(database)
	// 备份停服手段：Sup.Stop 的包装——实例未运行时容忍（返回 nil）。
	supStop := func(id int64) error {
		err := sup.Stop(id)
		if errors.Is(err, supervisor.ErrNotRunning) {
			return nil
		}
		return err
	}

	deps := api.Deps{
		Cfg:       cfg,
		DB:        database,
		Auth:      auth.New(database, secret),
		Audit:     audit.New(database),
		Secret:    secret,
		Instances: instStore,
		Hub:       hub,
		Jobs:      jobs,
		Sup:       sup,
		Backups:   backupStore,
		BackupSvc: backup.NewService(backupStore, supStop, cfg.Backup.KeepCountOrDefault(20)),
		RESTFor:   api.DefaultRESTFor(secret),
		RCONFor:   api.DefaultRCONFor(secret),
		Installer: installer.New(steamcmd.NewRunner(cfg.SteamCmdDir), httpDownload),
	}
	// 探活器须在首次 Start 之前注入
	sup.SetProber(restProber{restFor: deps.RESTFor})

	// 定时任务调度器：执行体经 api.Deps.RunScheduled 分派
	// （backup/broadcast/restart/update），根 context 驱动，随进程退出结束。
	sched := scheduler.New(database, deps.RunScheduled)
	deps.Sched = sched
	go sched.Start(context.Background())

	// autostart：组件全部就绪后拉起标记实例（Sup.Start 异步返回，不阻断监听）
	autostart(instStore, deps.StartInstance)

	log.Printf("panel %s listening on %s", version, cfg.Listen)
	if err := api.New(deps).Run(cfg.Listen); err != nil {
		log.Fatal(err)
	}
}
