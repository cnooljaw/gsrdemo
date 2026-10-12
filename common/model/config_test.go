package model

import (
	"strings"
	"testing"
)

// TestDecodeNodeConfigRejectsUnsafeOrAmbiguousInput 保护节点配置的完整校验和单对象边界。
func TestDecodeNodeConfigRejectsUnsafeOrAmbiguousInput(t *testing.T) {
	cases := []string{
		`{"instance":"login-01","listen":"login","log_directory":"logs/login-01","nodes":{"login":"127.0.0.1:19002"},"unknown":1}`,
		`{"instance":"login-01","listen":"login","log_directory":"logs/login-01","nodes":{"login":"0.0.0.0:19002"}}`,
		`{"instance":"login-01","listen":"login","log_directory":"logs/login-01","nodes":{"login":"127.0.0.1:19002"}} {}`,
		`{"instance":"login-01","listen":"online","log_directory":"logs/login-01","nodes":{"online":"127.0.0.1:19002"}}`,
	}
	for _, input := range cases {
		if _, err := DecodeNodeConfig(strings.NewReader(input), "login"); err == nil {
			t.Fatalf("invalid input accepted: %s", input)
		}
	}
}

// TestDecodeNodeConfigAcceptsLocalNode 保护合法本地配置原值，不在加载时暗改路由。
func TestDecodeNodeConfigAcceptsLocalNode(t *testing.T) {
	input := `{"instance":"login-01","listen":"login","log_directory":"logs/login-01","nodes":{"login":"127.0.0.1:19002"}}`
	cfg, err := DecodeNodeConfig(strings.NewReader(input), "login")
	if err != nil || cfg.Nodes["login"] != "127.0.0.1:19002" {
		t.Fatalf("config = %+v, error = %v", cfg, err)
	}
}
