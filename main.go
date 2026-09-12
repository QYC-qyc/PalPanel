package main

import (
	"log"

	"palpanel/internal/api"
	"palpanel/internal/config"
)

func main() {
	cfg, err := config.Load("config.yaml")
	if err != nil {
		log.Fatal(err)
	}
	if err := api.Run(cfg); err != nil {
		log.Fatal(err)
	}
}
