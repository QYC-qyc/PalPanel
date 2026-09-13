package main

import (
	"log"

	"palpanel/internal/api"
	"palpanel/internal/audit"
	"palpanel/internal/auth"
	"palpanel/internal/config"
	"palpanel/internal/db"
	"palpanel/internal/instance"
)

// version 由构建注入：-ldflags "-X main.version=..."（见 scripts/build.sh）。
var version = "dev"

func main() {
	cfg, err := config.Load("config.yaml")
	if err != nil {
		log.Fatal(err)
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
	deps := api.Deps{
		Cfg:       cfg,
		DB:        database,
		Auth:      auth.New(database, secret),
		Audit:     audit.New(database),
		Secret:    secret,
		Instances: instance.New(database),
	}
	log.Printf("panel %s listening on %s", version, cfg.Listen)
	if err := api.New(deps).Run(cfg.Listen); err != nil {
		log.Fatal(err)
	}
}
