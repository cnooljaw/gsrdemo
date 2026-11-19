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

type dhmjConfig struct {
	Node     model.NodeConfig `yaml:",inline"`
	Room     roomRules        `yaml:"room"`
	RedisURL string           `yaml:"redis_url"`
	RedisKey string           `yaml:"redis_key"`
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
	config := dhmjConfig{Room: defaultRoomRules, RedisURL: "redis://127.0.0.1:6379/0", RedisKey: "gsrdemo:dhmj:pending"}
	if err := model.ReadYAMLFile(*path, &config); err != nil {
		return err
	}
	node, err := model.ResolveNodeConfig(config.Node, *path, "dhmj")
	if err != nil {
		return err
	}
	if node.Nodes["online"] == "" || node.Nodes["settlement"] == "" {
		return errors.New("定海麻将需要配置 online 与 settlement 节点")
	}
	if !config.Room.valid() || config.RedisKey == "" {
		return errors.New("定海麻将房间分值、手续费或 Redis key 无效")
	}
	options, err := redis.ParseURL(config.RedisURL)
	if err != nil {
		return errors.New("定海麻将 redis_url 无效")
	}
	options.Protocol, options.DisableIdentity, options.ContextTimeoutEnabled = 2, true, true
	options.MaxRetries = -1
	options.DialTimeout, options.ReadTimeout, options.WriteTimeout = time.Second, time.Second, time.Second
	client := redis.NewClient(options)
	defer func() { result = errors.Join(result, client.Close()) }()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	journal := settlementJournal{client: client, key: config.RedisKey}
	wait, stop := context.WithTimeout(ctx, callTimeout)
	pending, err := journal.load(wait)
	stop()
	if err != nil {
		return fmt.Errorf("读取定海麻将待结算请求：%w", err)
	}
	nodes := make(map[cluster.NodeID]string, len(node.Nodes))
	for name, address := range node.Nodes {
		nodes[cluster.NodeID(name)] = address
	}
	return gsr.Run(ctx, gsr.Config{
		Logging: logging.Config{Directory: node.LogDirectory},
		Cluster: gsr.ClusterConfig{Nodes: nodes},
	}, &mainService{node: cluster.NodeID(node.Listen), rules: config.Room, journal: journal, pending: pending})
}
