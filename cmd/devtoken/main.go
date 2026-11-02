// cmd/devtoken 签发本地联调用 JWT（HS256，密钥取 AS_AUTH_JWT_SECRET / deploy/.env）：
//
//	devtoken --user 1001 [--tenant 1] [--ttl 24h] [--seed configs/seed/fields.yaml]
//
// 只校验 user 在种子 user_roles 中有绑定；tenant_id claim 默认取该用户唯一的农场租户（只有平台绑定时为 0），
// 绑定跨多个租户时须用 --tenant 指定。不打印密钥。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"

	"agri-steward/internal/clock"
	"agri-steward/internal/config"
	"agri-steward/internal/model"
	"agri-steward/internal/seed"
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
	seedFile := fs.String("seed", "configs/seed/fields.yaml", "种子文件（user_roles 段）")
	user := fs.Uint64("user", 0, "user_id（见种子 user_roles）")
	tenant := fs.Int64("tenant", -1, "tenant_id claim，默认按绑定推断")
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
	data, err := seed.Load(*seedFile)
	if err != nil {
		return err
	}
	tid, err := tokenTenant(data.UserRoles, *user, *tenant)
	if err != nil {
		return fmt.Errorf("%w (%s)", err, *seedFile)
	}
	auth, err := service.NewAuthService(cfg.Auth.JWTSecret, issueOnly{}, clock.System{})
	if err != nil {
		return err
	}
	d := *ttl
	if d == 0 {
		d = cfg.Auth.TokenTTL
	}
	tok, err := auth.Issue(*user, tid, d)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(stdout, tok)
	return err
}

// tokenTenant 按用户的种子绑定决定 tenant_id claim；want < 0 表示未指定。
func tokenTenant(bindings []model.UserRole, user uint64, want int64) (uint64, error) {
	var tenants []uint64
	bound := false
	for _, b := range bindings {
		if b.UserID != user {
			continue
		}
		bound = true
		if b.TenantID != 0 && !slices.Contains(tenants, b.TenantID) {
			tenants = append(tenants, b.TenantID)
		}
	}
	switch {
	case !bound:
		return 0, fmt.Errorf("user %d has no user_roles binding", user)
	case want >= 0:
		if want != 0 && !slices.Contains(tenants, uint64(want)) {
			return 0, fmt.Errorf("user %d has no binding in tenant %d", user, want)
		}
		return uint64(want), nil
	case len(tenants) > 1:
		return 0, fmt.Errorf("user %d is bound in tenants %v, pass --tenant", user, tenants)
	case len(tenants) == 1:
		return tenants[0], nil
	default:
		return 0, nil // 只有平台绑定
	}
}

// issueOnly 是只签发 token 时的 RoleStore 占位：devtoken 不解析 Scope。
type issueOnly struct{}

var errIssueOnly = errors.New("devtoken: scope resolution not supported")

func (issueOnly) ListRoles(context.Context, uint64, uint64, uint64) ([]model.RoleBinding, error) {
	return nil, errIssueOnly
}

func (issueOnly) FarmHasRole(context.Context, uint64, uint64, model.Role) (bool, error) {
	return false, errIssueOnly
}

func (issueOnly) FarmTenants(context.Context, uint64) ([]uint64, error) { return nil, errIssueOnly }
