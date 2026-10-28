package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/logging"
	"github.com/cnooljaw/gsr/mysqlclient"
	"github.com/cnooljaw/gsrdemo/common/model"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

type settlementConfig struct {
	Node     model.NodeConfig `yaml:",inline"`
	MySQLDSN string           `yaml:"mysql_dsn"`
}

func loadSettlementConfig(path string) (settlementConfig, error) {
	var config settlementConfig
	if path == "" {
		return config, errors.New("config path is required")
	}
	if err := model.ReadYAMLFile(path, &config); err != nil {
		return config, err
	}
	node, err := model.ResolveNodeConfig(config.Node, path, "settlement")
	if err != nil {
		return config, err
	}
	config.Node = node
	if strings.TrimSpace(config.MySQLDSN) == "" {
		return config, errors.New("mysql_dsn is required; use a private local YAML config")
	}
	return config, nil
}

func run() (result error) {
	path := flag.String("config", "", "节点 YAML 配置文件")
	initializeUser := flag.Uint64("init-wallet", 0, "显式初始化钱包并退出，必须提供 -silver")
	silver := flag.Int64("silver", -1, "初始化银子最小单位；重复执行不会重复发放")
	flag.Parse()
	config, err := loadSettlementConfig(*path)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	db, err := mysqlclient.Open(ctx, config.MySQLDSN)
	if err != nil {
		return fmt.Errorf("open settlement database: %w", err)
	}
	defer func() { result = errors.Join(result, db.Close()) }()
	if *initializeUser != 0 || *silver != -1 {
		if *initializeUser == 0 || *silver < 0 {
			return errors.New("-init-wallet and nonnegative -silver are required together")
		}
		wait, stop := context.WithTimeout(ctx, settlementSQLTimeout)
		defer stop()
		receipt, err := (settlementStore{db: db}).initialize(wait, *initializeUser, *silver)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(receipt)
	}
	nodes := make(map[cluster.NodeID]string, len(config.Node.Nodes))
	for name, address := range config.Node.Nodes {
		nodes[cluster.NodeID(name)] = address
	}
	return gsr.Run(ctx, gsr.Config{
		Logging: logging.Config{Directory: config.Node.LogDirectory},
		Cluster: gsr.ClusterConfig{Nodes: nodes},
	}, NewMain(cluster.NodeID(config.Node.Listen), db))
}
