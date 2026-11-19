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

func run() error {
	path := flag.String("config", "", "节点 YAML 配置文件")
	flag.Parse()
	config, err := model.LoadNodeConfig(*path, "dhmj")
	if err != nil {
		return err
	}
	if config.Nodes["online"] == "" || config.Nodes["settlement"] == "" {
		return errors.New("定海麻将需要配置 online 与 settlement 节点")
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
	}, &mainService{node: cluster.NodeID(config.Listen)})
}
