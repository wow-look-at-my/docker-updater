package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[docker-updater] ")

	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("configuration error: %v", err)
	}

	cli, err := newDockerClient()
	if err != nil {
		log.Fatalf("failed to create Docker client: %v", err)
	}
	defer cli.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	info, err := cli.Info(ctx)
	if err != nil {
		log.Fatalf("failed to connect to Docker: %v", err)
	}
	log.Printf("connected to Docker %s (%s)", info.ServerVersion, info.Name)

	var resolveAuth AuthResolver
	if cfg.ConfigPath != "" {
		dockerCfg, err := loadDockerConfig(cfg.ConfigPath)
		if err != nil {
			log.Fatalf("failed to load docker config: %v", err)
		}
		log.Printf("loaded registry credentials for %d registries", len(dockerCfg.Auths))
		resolveAuth = newAuthResolver(dockerCfg)
	} else {
		resolveAuth = newAuthResolver(nil)
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	runLoop(ctx, cli, cfg, sigCh, resolveAuth)
}
