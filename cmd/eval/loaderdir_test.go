package main

import (
	"path/filepath"
	"testing"

	"agri-steward/internal/app"
)

// P2 L5：--loader 留空时跟 --config 同目录的 loader/（默认值与生产 app.DefaultLoaderDir 一致），显式传值时原样使用。
func TestResolvedLoaderDir(t *testing.T) {
	abs := filepath.Join(t.TempDir(), "configs", "app.yaml")
	cases := []struct {
		name string
		f    infraFlags
		want string
	}{
		{"default config", infraFlags{configFile: "configs/app.yaml"}, app.DefaultLoaderDir},
		{"absolute config", infraFlags{configFile: abs}, filepath.Join(filepath.Dir(abs), "loader")},
		{"explicit loader", infraFlags{configFile: abs, loaderDir: "x/loader"}, "x/loader"},
	}
	for _, c := range cases {
		if got := c.f.resolvedLoaderDir(); got != c.want {
			t.Errorf("%s: resolvedLoaderDir = %q, want %q", c.name, got, c.want)
		}
	}
}
