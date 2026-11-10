package main

import (
	"context"
	"database/sql"
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

const (
	defaultWriteWorkers     = 12
	defaultReadWorkers      = 2
	defaultMySQLConnections = 10
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

type settlementConfig struct {
	Node             model.NodeConfig `yaml:",inline"`
	MySQLDSN         string           `yaml:"mysql_dsn"`
	WriteWorkers     int              `yaml:"write_workers"`
	ReadWorkers      int              `yaml:"read_workers"`
	MySQLConnections int              `yaml:"mysql_connections"`
	MailboxCapacity  int              `yaml:"mailbox_capacity"`
}

func loadSettlementConfig(path string) (settlementConfig, error) {
	var config settlementConfig
	if path == "" {
		return config, errors.New("config path is required")
	}
	if err := model.ReadYAMLFile(path, &config); err != nil {
		return config, err
	}
	if config.Node.Listen != "settlement" && !strings.HasPrefix(config.Node.Listen, "settlement-") {
		return config, errors.New("listen must name a settlement node")
	}
	node, err := model.ResolveNodeConfig(config.Node, path, config.Node.Listen)
	if err != nil {
		return config, err
	}
	config.Node = node
	if strings.TrimSpace(config.MySQLDSN) == "" {
		return config, errors.New("mysql_dsn is required; use a private local YAML config")
	}
	if config.MailboxCapacity == 0 {
		config.MailboxCapacity = 64
	}
	if config.MailboxCapacity < 1 || config.MailboxCapacity > 256 {
		return config, errors.New("mailbox_capacity must be 1..256")
	}
	if config.WriteWorkers == 0 {
		config.WriteWorkers = defaultWriteWorkers
	}
	if config.ReadWorkers == 0 {
		config.ReadWorkers = defaultReadWorkers
	}
	if config.MySQLConnections == 0 {
		config.MySQLConnections = defaultMySQLConnections
	}
	if config.WriteWorkers < 1 || config.WriteWorkers > 64 || config.ReadWorkers < 1 || config.ReadWorkers > 64 || config.MySQLConnections < 2 || config.MySQLConnections > 128 {
		return config, errors.New("write_workers/read_workers must be 1..64; mysql_connections must be 2..128")
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

	db, err := openSettlementDatabase(ctx, config.MySQLDSN)
	if err != nil {
		return fmt.Errorf("open settlement database: %w", err)
	}
	db.SetMaxOpenConns(config.MySQLConnections)
	db.SetMaxIdleConns(config.MySQLConnections)
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
		Runtime: gsr.RuntimeConfig{MailboxCapacity: config.MailboxCapacity},
		Cluster: gsr.ClusterConfig{Nodes: nodes},
	}, NewMain(cluster.NodeID(config.Node.Listen), db, config.WriteWorkers, config.ReadWorkers))
}

// openSettlementDatabase 建立宿主拥有的 MySQL 池；运行入口按配置设置并行上限。
func openSettlementDatabase(ctx context.Context, dsn string) (*sql.DB, error) {
	db, err := mysqlclient.Open(ctx, dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(2)
	return db, nil
}
