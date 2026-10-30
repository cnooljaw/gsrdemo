package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/logging"
	"github.com/cnooljaw/gsrdemo/common/model"
)

type accessConfig struct {
	Node model.NodeConfig `yaml:",inline"`
	Gate gateOptions      `yaml:"gate"`
}

type gateOptions struct {
	Address               string `yaml:"address"`
	MaxConnections        int    `yaml:"max_connections"`
	MaxFrameBytes         int    `yaml:"max_frame_bytes"`
	MaxOutboundQueueBytes int    `yaml:"max_outbound_queue_bytes"`
}

func loadAccessConfig(path string) (accessConfig, error) {
	var config accessConfig
	if path == "" {
		return config, errors.New("config path is required")
	}
	if err := model.ReadYAMLFile(path, &config); err != nil {
		return config, err
	}
	route := config.Node.Listen
	instance, err := strconv.Atoi(strings.TrimPrefix(route, "access-"))
	if err != nil || instance < 1 || route != fmt.Sprintf("access-%02d", instance) {
		return config, errors.New("listen must be numbered access-01, access-02, ...")
	}
	node, err := model.ResolveNodeConfig(config.Node, path, route)
	if err != nil {
		return config, err
	}
	config.Node = node
	return config, nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	path := flag.String("config", "", "节点 YAML 配置文件")
	flag.Parse()
	config, err := loadAccessConfig(*path)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	nodes := make(map[cluster.NodeID]string, len(config.Node.Nodes))
	for name, address := range config.Node.Nodes {
		nodes[cluster.NodeID(name)] = address
	}
	return gsr.Run(ctx, gsr.Config{
		Logging: logging.Config{Directory: config.Node.LogDirectory},
		Cluster: gsr.ClusterConfig{Nodes: nodes},
	}, NewMain(cluster.NodeID(config.Node.Listen), config.Gate))
}
