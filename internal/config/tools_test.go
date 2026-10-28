package config

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const toolsBase = `tools:
  mock_dir: configs/mock
  weather: {mode: mock, timeout: 3s, level: read, real: {base_url: ""}}
  crop: {mode: mock, timeout: 3s, level: read}
  field: {mode: mock, timeout: 2s, level: read}
  device: {mode: mock, timeout: 3s, level: read}
`

func TestLoadTools(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string { return writeFile(t, dir, name, body) }
	base := write("base.yaml", toolsBase)
	tests := []struct {
		name      string
		file      string
		env       map[string]string
		wantErr   error  // errors.Is 目标
		errSubstr string // 非 sentinel 错误的关键词
		check     func(t *testing.T, c *Tools)
	}{
		{name: "ok", file: base, check: func(t *testing.T, c *Tools) {
			if c.MockDir != "configs/mock" || c.Field.Timeout != 2*time.Second || c.Weather.Mode != ToolModeMock {
				t.Fatalf("cfg = %+v", c)
			}
			if names := c.Servers(); len(names) != 4 || names[0].Name != "weather" || names[3].Name != "device" {
				t.Fatalf("servers = %+v", names)
			}
		}},
		{name: "env switches to real with key", file: base,
			env: map[string]string{"AS_TOOLS_WEATHER_MODE": "real", "AS_TOOLS_WEATHER_REAL_API_KEY": "k-1"},
			check: func(t *testing.T, c *Tools) {
				if c.Weather.Mode != ToolModeReal || c.Weather.Real.APIKey != "k-1" || c.Crop.Mode != ToolModeMock {
					t.Fatalf("weather = %+v", c.Weather)
				}
			}},
		{name: "api_key in yaml rejected", wantErr: ErrSecretInFile,
			file: write("secret.yaml", strings.Replace(toolsBase, `base_url: ""`, `base_url: "", api_key: leaked`, 1))},
		{name: "bad mode", errSubstr: "tools.crop.mode",
			file: write("mode.yaml", strings.Replace(toolsBase, "crop: {mode: mock", "crop: {mode: stub", 1))},
		{name: "zero timeout", errSubstr: "tools.field.timeout",
			file: write("timeout.yaml", strings.Replace(toolsBase, "timeout: 2s", "timeout: 0s", 1))},
		{name: "bad level", errSubstr: "tools.device.level",
			file: write("level.yaml", strings.Replace(toolsBase, "device: {mode: mock, timeout: 3s, level: read}", "device: {mode: mock, timeout: 3s, level: admin}", 1))},
		{name: "mock without mock_dir", errSubstr: "tools.mock_dir",
			file: write("nodir.yaml", strings.Replace(toolsBase, "  mock_dir: configs/mock\n", "", 1))},
		{name: "missing file", file: filepath.Join(dir, "nope.yaml"), errSubstr: "read"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, n := range toolServerNames { // 隔离宿主环境
				t.Setenv("AS_TOOLS_"+strings.ToUpper(n)+"_REAL_API_KEY", "")
				t.Setenv("AS_TOOLS_"+strings.ToUpper(n)+"_MODE", "")
			}
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			c, err := LoadTools(tt.file)
			switch {
			case tt.wantErr != nil:
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
			case tt.errSubstr != "":
				if err == nil || !strings.Contains(err.Error(), tt.errSubstr) {
					t.Fatalf("err = %v, want mention of %q", err, tt.errSubstr)
				}
			default:
				if err != nil {
					t.Fatal(err)
				}
				tt.check(t, c)
			}
		})
	}
}

func TestLoadTools_RepoFile(t *testing.T) {
	c, err := LoadTools(filepath.Join("..", "..", "configs", "tools.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range c.Servers() {
		if s.Mode != ToolModeMock || s.Level != "read" {
			t.Errorf("%s: mode=%s level=%s, want mock/read in P0", s.Name, s.Mode, s.Level)
		}
	}
}
