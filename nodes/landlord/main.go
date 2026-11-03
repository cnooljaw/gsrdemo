package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/logging"
	"github.com/cnooljaw/gsrdemo/common/model"
	"github.com/redis/go-redis/v9"
)

type landlordConfig struct {
	SettlementNode string           `yaml:"settlement_node"`
	Node           model.NodeConfig `yaml:",inline"`
	RedisURL       string           `yaml:"redis_url"`
	RedisKey       string           `yaml:"redis_key"`
}

func loadLandlordConfig(path string) (landlordConfig, error) {
	var config landlordConfig
	if err := model.ReadYAMLFile(path, &config); err != nil {
		return config, err
	}
	node, err := model.ResolveNodeConfig(config.Node, path, "landlord")
	if err != nil {
		return config, err
	}
	config.Node = node
	if config.SettlementNode == "" {
		config.SettlementNode = "settlement"
	}
	if node.Nodes[config.SettlementNode] == "" {
		return config, errors.New("settlement_node must exist in nodes_file")
	}
	if node.Nodes["online"] == "" || config.RedisKey == "" {
		return config, errors.New("online, settlement and redis_key are required")
	}
	if _, err := redis.ParseURL(config.RedisURL); err != nil {
		return config, errors.New("invalid redis_url")
	}
	return config, nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() (result error) {
	path := flag.String("config", "", "节点 YAML 配置文件")
	flag.Parse()
	config, err := loadLandlordConfig(*path)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	options, err := redis.ParseURL(config.RedisURL)
	if err != nil {
		return errors.New("invalid redis_url")
	}
	options.Protocol, options.DisableIdentity, options.ContextTimeoutEnabled = 2, true, true
	options.MaxRetries = -1
	options.DialTimeout, options.ReadTimeout, options.WriteTimeout = time.Second, time.Second, time.Second
	client := redis.NewClient(options)
	defer func() { result = errors.Join(result, client.Close()) }()
	journal := landlordJournal{client: client, key: config.RedisKey}
	wait, stop := context.WithTimeout(ctx, callTimeout)
	pending, err := journal.load(wait)
	stop()
	if err != nil {
		return err
	}
	nodes := map[cluster.NodeID]string{}
	for name, address := range config.Node.Nodes {
		nodes[cluster.NodeID(name)] = address
	}
	return gsr.Run(ctx, gsr.Config{Logging: logging.Config{Directory: config.Node.LogDirectory}, Cluster: gsr.ClusterConfig{Nodes: nodes}},
		&mainService{node: cluster.NodeID(config.Node.Listen), settlementNode: cluster.NodeID(config.SettlementNode), journal: journal, pending: pending})
}
