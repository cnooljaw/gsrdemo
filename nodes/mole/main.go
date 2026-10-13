package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/logging"
	"github.com/cnooljaw/gsrdemo/common/model"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	path := flag.String("config", "", "节点 JSON 配置文件")
	flag.Parse()
	cfg, err := model.LoadNodeConfig(*path, "mole")
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	nodes := make(map[cluster.NodeID]string, len(cfg.Nodes))
	for name, address := range cfg.Nodes {
		nodes[cluster.NodeID(name)] = address
	}
	return gsr.Run(ctx, gsr.Config{
		Logging: logging.Config{Directory: cfg.LogDirectory},
		Cluster: cluster.ClusterConfig{Nodes: nodes},
	}, NewMain(cfg, os.Stdout))
}
