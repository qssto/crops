package config

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadDevices(t *testing.T) {
	repo, err := LoadDevices(filepath.Join("..", "..", "configs", "devices.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if repo.Devicesim.Enabled || repo.Devicesim.Timeout != 3*time.Second || len(repo.Devicesim.DeviceTypes) != 1 {
		t.Fatalf("configs/devices.yaml = %+v, want devicesim disabled by default", repo.Devicesim)
	}
	dir := t.TempDir()
	tests := []struct {
		name      string
		body      string
		env       map[string]string
		errSubstr string
		enabled   bool
	}{
		{name: "env enables", body: "devicesim: {enabled: false, base_url: http://127.0.0.1:8090, timeout: 2s, device_types: [uav]}\n",
			env: map[string]string{"AS_DEVICESIM_ENABLED": "true"}, enabled: true},
		{name: "enabled without base url", body: "devicesim: {enabled: true, timeout: 2s, device_types: [uav]}\n",
			errSubstr: "base_url"},
		{name: "enabled without timeout", body: "devicesim: {enabled: true, base_url: http://localhost:8090, device_types: [uav]}\n",
			errSubstr: "timeout"},
		{name: "enabled without types", body: "devicesim: {enabled: true, base_url: http://localhost:8090, timeout: 1s}\n",
			errSubstr: "device_types"},
		{name: "duplicate type", body: "devicesim: {enabled: true, base_url: http://localhost:8090, timeout: 1s, device_types: [uav, uav]}\n",
			errSubstr: "duplicate"},
		{name: "non-loopback base url", body: "devicesim: {enabled: true, base_url: http://10.0.0.5:8090, timeout: 1s, device_types: [uav]}\n",
			errSubstr: "loopback"},
		{name: "ipv6 loopback", body: "devicesim: {enabled: true, base_url: \"http://[::1]:8090\", timeout: 1s, device_types: [uav]}\n",
			enabled: true},
		{name: "disabled skips validation", body: "devicesim: {enabled: false}\n"},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			d, err := LoadDevices(writeFile(t, dir, "d"+string(rune('a'+i))+".yaml", tt.body))
			if tt.errSubstr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.errSubstr) {
					t.Fatalf("err = %v, want %q", err, tt.errSubstr)
				}
				return
			}
			if err != nil || d.Devicesim.Enabled != tt.enabled {
				t.Fatalf("LoadDevices = %+v, %v", d, err)
			}
		})
	}
	if _, err := LoadDevices(filepath.Join(dir, "missing.yaml")); err == nil {
		t.Fatal("missing file must fail")
	}
}

// TestDevicesimCheckDeploy：生产保护。未开启任何 env 都通过；开启时 env=prod 拒绝，base_url 必须回环。
func TestDevicesimCheckDeploy(t *testing.T) {
	on := func(u string) Devicesim { return Devicesim{Enabled: true, BaseURL: u} }
	tests := []struct {
		name string
		sim  Devicesim
		env  string
		ok   bool
	}{
		{name: "disabled in prod", sim: Devicesim{BaseURL: "http://devices.example.com"}, env: EnvProd, ok: true},
		{name: "dev localhost", sim: on("http://localhost:8090"), env: "dev", ok: true},
		{name: "staging 127.0.0.2", sim: on("https://127.0.0.2:8090"), env: "staging", ok: true},
		{name: "dev ::1", sim: on("http://[::1]:8090"), env: "dev", ok: true},
		{name: "prod loopback", sim: on("http://127.0.0.1:8090"), env: EnvProd},
		{name: "dev private ip", sim: on("http://192.168.1.10:8090"), env: "dev"},
		{name: "dev hostname", sim: on("http://devicesim.internal:8090"), env: "dev"},
		{name: "dev localhost-like", sim: on("http://localhost.example.com"), env: "dev"},
		{name: "dev non-http", sim: on("ftp://127.0.0.1"), env: "dev"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.sim.CheckDeploy(tt.env)
			if tt.ok != (err == nil) || (err != nil && !errors.Is(err, ErrDevicesimNotAllowed)) {
				t.Fatalf("CheckDeploy(%q) = %v, want ok=%v", tt.env, err, tt.ok)
			}
		})
	}
}
