// cmd/devtoken 签发本地联调用 JWT（HS256，密钥取 AS_AUTH_JWT_SECRET / deploy/.env）：
//
//	devtoken --user 1001 [--ttl 24h]
//
// 只校验 user 存在于 configs/auth.yaml，不打印密钥。
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"agri-steward/internal/clock"
	"agri-steward/internal/config"
	"agri-steward/internal/service"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "devtoken:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("devtoken", flag.ContinueOnError)
	configFile := fs.String("config", "configs/app.yaml", "配置文件")
	envFile := fs.String("env-file", "deploy/.env", "密钥文件（不存在则只读环境变量）")
	user := fs.Uint64("user", 0, "user_id（见 configs/auth.yaml）")
	ttl := fs.Duration("ttl", 0, "有效期，默认取 auth.token_ttl")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *user == 0 {
		return errors.New("--user is required")
	}
	cfg, err := config.Load(*configFile, *envFile)
	if err != nil {
		return err
	}
	if err := cfg.RequireSecrets("auth.jwt_secret"); err != nil {
		return err
	}
	grants, err := config.LoadGrants(cfg.Auth.File)
	if err != nil {
		return err
	}
	if _, ok := grants[*user]; !ok {
		return fmt.Errorf("user %d not in %s", *user, cfg.Auth.File)
	}
	auth, err := service.NewAuthService(cfg.Auth.JWTSecret, grants, clock.System{})
	if err != nil {
		return err
	}
	d := *ttl
	if d == 0 {
		d = cfg.Auth.TokenTTL
	}
	tok, err := auth.Issue(*user, d)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(stdout, tok)
	return err
}
