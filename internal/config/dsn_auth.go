package config

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/spf13/viper"

	"agri-steward/internal/model"
)

// DSN 返回连接 db 的 DSN：parseTime + loc=UTC（存储一律 UTC）；multiStatements 只给迁移用。
func (m MySQL) DSN(db string, multiStatements bool) string {
	c := mysql.NewConfig()
	c.User = m.User
	c.Passwd = m.Password
	c.Net = "tcp"
	c.Addr = net.JoinHostPort(m.Host, strconv.Itoa(m.Port))
	c.DBName = db
	c.ParseTime = true
	c.Loc = time.UTC
	c.MultiStatements = multiStatements
	// FormatDSN 对默认的 UTC 不输出 loc，这里显式写出，便于排查
	c.Params = map[string]string{"charset": "utf8mb4", "loc": "UTC"}
	return c.FormatDSN()
}

// RequireSecrets 校验运行所需密钥已从环境变量取到（不回显值）。
func (c *Config) RequireSecrets(keys ...string) error {
	vals := map[string]string{
		"mysql.password":   c.MySQL.Password,
		"redis.password":   c.Redis.Password,
		"minio.access_key": c.MinIO.AccessKey,
		"minio.secret_key": c.MinIO.SecretKey,
		"auth.jwt_secret":  c.Auth.JWTSecret,
	}
	var missing []error
	for _, k := range keys {
		val, known := vals[k]
		if !known {
			return fmt.Errorf("config: unknown secret key %q", k)
		}
		if val == "" {
			missing = append(missing, fmt.Errorf("config: secret %s not set (env or deploy/.env)", k))
		}
	}
	return errors.Join(missing...)
}

// authFile 是 configs/auth.yaml 的原始结构：user_id → 授权列表。
type authFile struct {
	Users map[string][]struct {
		TenantID uint64   `mapstructure:"tenant_id"`
		FarmID   uint64   `mapstructure:"farm_id"`
		Roles    []string `mapstructure:"roles"`
	} `mapstructure:"users"`
}

var knownRoles = map[model.Role]bool{
	model.RoleFarmManager: true, model.RoleAgronomist: true, model.RoleOperator: true, model.RolePlatformAdmin: true,
}

// LoadGrants 经 Viper 读取 configs/auth.yaml，校验 user_id 与角色取值。
func LoadGrants(path string) (map[uint64][]model.Grant, error) {
	v := viper.New()
	v.SetConfigFile(path)
	if err := v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	var f authFile
	if err := v.Unmarshal(&f); err != nil {
		return nil, fmt.Errorf("config: decode %s: %w", path, err)
	}
	out := make(map[uint64][]model.Grant, len(f.Users))
	for key, items := range f.Users {
		uid, err := strconv.ParseUint(key, 10, 64)
		if err != nil || uid == 0 {
			return nil, fmt.Errorf("config: %s: bad user_id %q", path, key)
		}
		for _, it := range items {
			g := model.Grant{TenantID: it.TenantID, FarmID: it.FarmID}
			for _, r := range it.Roles {
				if !knownRoles[model.Role(r)] {
					return nil, fmt.Errorf("config: %s: user %d: unknown role %q", path, uid, r)
				}
				g.Roles = append(g.Roles, model.Role(r))
			}
			if len(g.Roles) == 0 {
				return nil, fmt.Errorf("config: %s: user %d: empty roles", path, uid)
			}
			out[uid] = append(out[uid], g)
		}
	}
	return out, nil
}
