package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
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

type loginConfig struct {
	Node     model.NodeConfig `yaml:",inline"`
	MySQLDSN string           `yaml:"mysql_dsn"`
}

func loadLoginConfig(path string) (loginConfig, error) {
	var config loginConfig
	if path == "" {
		return config, errors.New("config path is required")
	}
	if err := model.ReadYAMLFile(path, &config); err != nil {
		return config, err
	}
	node, err := model.ResolveNodeConfig(config.Node, path, "login")
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
	username := flag.String("create-user", "", "创建示例账号并退出，密码从标准输入读取")
	flag.Parse()
	config, err := loadLoginConfig(*path)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	db, err := mysqlclient.Open(ctx, config.MySQLDSN)
	if err != nil {
		return fmt.Errorf("open login database: %w", err)
	}
	defer func() { result = errors.Join(result, db.Close()) }()
	if *username != "" {
		password, err := readPassword(os.Stdin)
		if err != nil {
			return err
		}
		id, err := createAccount(ctx, db, *username, password)
		if err != nil {
			return fmt.Errorf("create account: %w", err)
		}
		_, err = fmt.Printf("UserID=%d\n", id)
		return err
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

func readPassword(input io.Reader) (string, error) {
	line, err := bufio.NewReader(io.LimitReader(input, 130)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	password := strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
	if len(password) < 1 || len(password) > 128 {
		return "", errors.New("password must contain 1..128 bytes")
	}
	return password, nil
}
