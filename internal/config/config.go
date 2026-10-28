// Package config 用 Viper 读取 configs/*.yaml 并以环境变量覆盖，产出强类型配置；业务包不 import viper。
// 环境变量前缀 AS_，键中的 "." 换成 "_"（如 mysql.host → AS_MYSQL_HOST）。
// 密码与密钥只从环境变量或 deploy/.env 取，yaml 中出现即报错。
package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/viper"
)

const envPrefix = "AS"

// Config 是进程配置。
type Config struct {
	Env   string `mapstructure:"env"`
	HTTP  HTTP   `mapstructure:"http"`
	MySQL MySQL  `mapstructure:"mysql"`
	Redis Redis  `mapstructure:"redis"`
	MinIO MinIO  `mapstructure:"minio"`
	Auth  Auth   `mapstructure:"auth"`
	Log   Log    `mapstructure:"log"`
}

// HTTP 是 server 监听配置。
type HTTP struct {
	Addr string `mapstructure:"addr"`
}

// MySQL 是业务库与评测库连接配置。
type MySQL struct {
	Host         string `mapstructure:"host"`
	Port         int    `mapstructure:"port"`
	User         string `mapstructure:"user"`
	Password     string `mapstructure:"password"` // secret
	Database     string `mapstructure:"database"`
	EvalDatabase string `mapstructure:"eval_database"`
	MaxOpenConns int    `mapstructure:"max_open_conns"`
}

// Redis 是 Redis 连接配置。
type Redis struct {
	Addr     string `mapstructure:"addr"`
	Password string `mapstructure:"password"` // secret
	DB       int    `mapstructure:"db"`
}

// MinIO 是对象存储配置；换阿里云 OSS 只改 endpoint 与密钥。
type MinIO struct {
	Endpoint   string `mapstructure:"endpoint"`
	AccessKey  string `mapstructure:"access_key"` // secret
	SecretKey  string `mapstructure:"secret_key"` // secret
	UseSSL     bool   `mapstructure:"use_ssl"`
	BucketRaw  string `mapstructure:"bucket_raw"`
	BucketEval string `mapstructure:"bucket_eval"`
}

// Auth 是鉴权配置。
type Auth struct {
	File      string        `mapstructure:"file"`
	TokenTTL  time.Duration `mapstructure:"token_ttl"`
	JWTSecret string        `mapstructure:"jwt_secret"` // secret
}

// Log 是日志配置。
type Log struct {
	Level string `mapstructure:"level"`
}

// secrets 是只允许来自环境变量的键，及其可接受的变量名（先 AS_ 前缀，再 deploy/.env 中的原名）。
var secrets = []struct {
	key  string
	envs []string
}{
	{"mysql.password", []string{"AS_MYSQL_PASSWORD", "MYSQL_ROOT_PASSWORD"}},
	{"redis.password", []string{"AS_REDIS_PASSWORD", "REDIS_PASSWORD"}},
	{"minio.access_key", []string{"AS_MINIO_ACCESS_KEY", "MINIO_USER"}},
	{"minio.secret_key", []string{"AS_MINIO_SECRET_KEY", "MINIO_PASSWORD"}},
	{"auth.jwt_secret", []string{"AS_AUTH_JWT_SECRET", "AUTH_JWT_SECRET"}},
}

// ErrSecretInFile 表示 yaml 中写了密钥。
var ErrSecretInFile = errors.New("config: secret must come from env or deploy/.env, not yaml")

// Load 读取 file（yaml），用环境变量覆盖；envFile（通常 deploy/.env）存在时作为密钥的后备来源，
// 进程环境变量优先。envFile 为空或不存在则跳过。
func Load(file, envFile string) (*Config, error) {
	v := viper.New()
	v.SetConfigFile(file)
	v.SetEnvPrefix(envPrefix)
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()
	if err := v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("config: read %s: %w", file, err)
	}
	dotenv, err := readEnvFile(envFile)
	if err != nil {
		return nil, err
	}
	for _, s := range secrets {
		if v.InConfig(s.key) {
			return nil, fmt.Errorf("%w: %s in %s", ErrSecretInFile, s.key, file)
		}
		v.Set(s.key, lookupSecret(s.envs, dotenv))
	}
	var c Config
	if err := v.Unmarshal(&c); err != nil {
		return nil, fmt.Errorf("config: decode %s: %w", file, err)
	}
	return &c, nil
}

// lookupSecret 依次查进程环境变量与 deploy/.env；都没有返回空串，由使用方校验。
func lookupSecret(names []string, dotenv map[string]string) string {
	for _, n := range names {
		if val, ok := os.LookupEnv(n); ok && val != "" {
			return val
		}
	}
	for _, n := range names {
		if val := dotenv[n]; val != "" {
			return val
		}
	}
	return ""
}

// readEnvFile 用 Viper 的 dotenv 解析读取 deploy/.env；键统一转大写。
func readEnvFile(path string) (map[string]string, error) {
	if path == "" {
		return nil, nil
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("config: stat %s: %w", path, err)
	}
	ev := viper.New()
	ev.SetConfigFile(path)
	ev.SetConfigType("env")
	if err := ev.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	out := make(map[string]string, len(ev.AllKeys()))
	for _, k := range ev.AllKeys() {
		out[strings.ToUpper(k)] = ev.GetString(k)
	}
	return out, nil
}
