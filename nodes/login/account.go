package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

// runAccount 是 login account 子命令，密码只从标准输入读取。
func runAccount(args []string) error {
	flags := flag.NewFlagSet("login account", flag.ContinueOnError)
	path := flags.String("file", "configs/accounts.local.yaml", "测试账号哈希文件")
	username := flags.String("username", "", "测试用户名；密码从标准输入读取")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	password, err := io.ReadAll(io.LimitReader(os.Stdin, 130))
	if err != nil {
		return err
	}
	passwordText := strings.TrimSuffix(strings.TrimSuffix(string(password), "\n"), "\r")
	if err := CreateAccount(*path, *username, passwordText); err != nil {
		return err
	}
	fmt.Println("测试账号已创建；重启 login 后生效")
	return nil
}
