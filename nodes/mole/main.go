package main

import (
	"context"
	"errors"
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

func loadMoleConfig(path string) (model.NodeConfig, error) {
	config, err := model.LoadNodeConfig(path, "mole")
	if err != nil {
		return config, err
	}
	if config.Nodes["settlement"] == "" || config.Nodes["online"] == "" {
		return config, errors.New("online and settlement routes are required")
	}
	return config, nil
}

func run() error {
	path := flag.String("config", "", "节点 YAML 配置文件")
	flag.Parse()
	config, err := loadMoleConfig(*path)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	nodes := make(map[cluster.NodeID]string, len(config.Nodes))
	for name, address := range config.Nodes {
		nodes[cluster.NodeID(name)] = address
	}
	return gsr.Run(ctx, gsr.Config{
		Logging: logging.Config{Directory: config.LogDirectory},
		Cluster: gsr.ClusterConfig{Nodes: nodes},
	}, NewMain(cluster.NodeID(config.Listen)))
}
