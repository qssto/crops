package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	base := writeFile(t, dir, "app.yaml", "env: dev\nmysql: {host: db.local, port: 3306, user: root, database: agri_steward}\n")
	withSecret := writeFile(t, dir, "bad.yaml", "mysql: {password: leaked}\n")
	dotenv := writeFile(t, dir, ".env", "MYSQL_ROOT_PASSWORD=from-dotenv\nAUTH_JWT_SECRET=dotenv-secret\n")

	tests := []struct {
		name     string
		file     string
		envFile  string
		env      map[string]string
		wantErr  error
		wantHost string
		wantPass string
	}{
		{name: "yaml only", file: base, wantHost: "db.local"},
		{name: "env overrides yaml", file: base, env: map[string]string{"AS_MYSQL_HOST": "10.0.0.1"}, wantHost: "10.0.0.1"},
		{name: "secret from dotenv", file: base, envFile: dotenv, wantHost: "db.local", wantPass: "from-dotenv"},
		{name: "process env beats dotenv", file: base, envFile: dotenv,
			env: map[string]string{"AS_MYSQL_PASSWORD": "from-env"}, wantHost: "db.local", wantPass: "from-env"},
		{name: "missing dotenv is fine", file: base, envFile: filepath.Join(dir, "nope.env"), wantHost: "db.local"},
		{name: "secret in yaml rejected", file: withSecret, wantErr: ErrSecretInFile},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, s := range secrets { // 隔离宿主环境中的真实密钥
				for _, n := range s.envs {
					t.Setenv(n, "")
				}
			}
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			c, err := Load(tt.file, tt.envFile)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr != nil {
				return
			}
			if c.MySQL.Host != tt.wantHost || c.MySQL.Password != tt.wantPass {
				t.Fatalf("host=%q pass=%q, want %q %q", c.MySQL.Host, c.MySQL.Password, tt.wantHost, tt.wantPass)
			}
		})
	}
}

// 仓库自带的 configs/app.yaml 默认开启记忆，环境变量可关闭（验收 4 的开关）。
func TestLoad_MemorySwitch(t *testing.T) {
	c, err := Load("../../configs/app.yaml", "")
	if err != nil {
		t.Fatal(err)
	}
	if !c.Memory.Enabled || c.Memory.EpisodicLimit != 5 || c.Memory.RuneBudget != 600 {
		t.Fatalf("memory = %+v, want enabled/5/600", c.Memory)
	}
	t.Setenv("AS_MEMORY_ENABLED", "false")
	if c, err = Load("../../configs/app.yaml", ""); err != nil || c.Memory.Enabled {
		t.Fatalf("env off: memory = %+v, err %v", c.Memory, err)
	}
}

func TestRequireSecrets(t *testing.T) {
	c := &Config{MySQL: MySQL{Password: "x"}}
	if err := c.RequireSecrets("mysql.password"); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	err := c.RequireSecrets("mysql.password", "auth.jwt_secret")
	if err == nil || !strings.Contains(err.Error(), "auth.jwt_secret") {
		t.Fatalf("want missing auth.jwt_secret, got %v", err)
	}
	if strings.Contains(err.Error(), "mysql.password") {
		t.Fatalf("error must only list missing secrets: %v", err)
	}
}

func TestDSN(t *testing.T) {
	m := MySQL{Host: "127.0.0.1", Port: 3306, User: "root", Password: "p@ss/word"}
	got := m.DSN("agri_steward", true)
	for _, want := range []string{"parseTime=true", "loc=UTC", "multiStatements=true", "/agri_steward?"} {
		if !strings.Contains(got, want) {
			t.Errorf("DSN missing %q", want)
		}
	}
	if strings.Contains(m.DSN("x", false), "multiStatements") {
		t.Error("multiStatements must be off for app connections")
	}
}

func TestLoad_RepoFile(t *testing.T) {
	c, err := Load("../../configs/app.yaml", "")
	if err != nil {
		t.Fatal(err)
	}
	if c.Env == "" || c.MySQL.Database != "agri_steward" || c.MinIO.BucketRaw != "agri-steward-raw" {
		t.Fatalf("unexpected config %+v", c)
	}
}
