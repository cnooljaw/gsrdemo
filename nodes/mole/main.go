package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/logging"
	"github.com/cnooljaw/gsrdemo/common/model"
	"github.com/redis/go-redis/v9"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

type moleConfig struct {
	Node     model.NodeConfig `yaml:",inline"`
	RedisURL string           `yaml:"redis_url"`
	RedisKey string           `yaml:"redis_key"`
}

func loadMoleConfig(path string) (moleConfig, error) {
	var config moleConfig
	if path == "" {
		return config, errors.New("config path is required")
	}
	if err := model.ReadYAMLFile(path, &config); err != nil {
		return config, err
	}
	node, err := model.ResolveNodeConfig(config.Node, path, "mole")
	if err != nil {
		return config, err
	}
	config.Node = node
	if node.Nodes["settlement"] == "" || node.Nodes["online"] == "" {
		return config, errors.New("online and settlement routes are required")
	}
	if strings.TrimSpace(config.RedisURL) == "" || strings.TrimSpace(config.RedisKey) == "" {
		return config, errors.New("redis_url and redis_key are required")
	}
	if _, err := redis.ParseURL(config.RedisURL); err != nil {
		return config, errors.New("invalid redis_url")
	}
	return config, nil
}

func newMoleRedis(rawURL string) (*redis.Client, error) {
	options, err := redis.ParseURL(rawURL)
	if err != nil {
		return nil, errors.New("invalid redis_url")
	}
	options.Protocol, options.DisableIdentity, options.ContextTimeoutEnabled = 2, true, true
	options.MaxRetries = -1
	options.DialTimeout, options.ReadTimeout, options.WriteTimeout = time.Second, time.Second, time.Second
	return redis.NewClient(options), nil
}

func run() (result error) {
	path := flag.String("config", "", "节点 YAML 配置文件")
	initialize := flag.Bool("init-accounting", false, "显式创建零统计检查点并退出，已有状态不覆盖")
	flag.Parse()
	config, err := loadMoleConfig(*path)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	client, err := newMoleRedis(config.RedisURL)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, client.Close()) }()
	checkpoint := moleCheckpoint{client: client, key: config.RedisKey}
	if *initialize {
		wait, stop := context.WithTimeout(ctx, moleCallTimeout)
		defer stop()
		return checkpoint.initializeLanes(wait)
	}

	nodes := make(map[cluster.NodeID]string, len(config.Node.Nodes))
	for name, address := range config.Node.Nodes {
		nodes[cluster.NodeID(name)] = address
	}
	runContext, fail := context.WithCancelCause(ctx)
	defer fail(nil)
	err = gsr.Run(runContext, gsr.Config{
		Logging: logging.Config{Directory: config.Node.LogDirectory},
		Cluster: gsr.ClusterConfig{Nodes: nodes},
	}, NewMain(cluster.NodeID(config.Node.Listen), checkpoint, fail))
	if cause := context.Cause(runContext); cause != nil && !errors.Is(cause, context.Canceled) {
		return errors.Join(err, cause)
	}
	return err
}
