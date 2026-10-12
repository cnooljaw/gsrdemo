package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/cnooljaw/gsrdemo/nodes/login/internal/login"
)

func main() {
	path := flag.String("file", "configs/accounts.local.json", "测试账号哈希文件")
	username := flag.String("username", "", "测试用户名；密码从标准输入读取")
	flag.Parse()
	password, err := io.ReadAll(io.LimitReader(os.Stdin, 130))
	if err == nil {
		err = login.CreateAccount(*path, *username, strings.TrimSuffix(strings.TrimSuffix(string(password), "\n"), "\r"))
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("测试账号已创建；重启 login 后生效")
}
