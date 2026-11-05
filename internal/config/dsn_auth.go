package config

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/go-sql-driver/mysql"
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
		// P2-L2b：评测库独立账号（eval_review.enabled 时由启动方要求）
		"eval_review.user":     c.EvalReview.User,
		"eval_review.password": c.EvalReview.Password,
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
