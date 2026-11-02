package config

import (
	"testing"
	"time"
)

// dispatch 段：缺省 2 / 30m / 2h，yaml 与环境变量可覆盖，非法值启动失败（应失败：max_attempts=0 / device_wait_max=0 照常加载）。
func TestLoad_Dispatch(t *testing.T) {
	dir := t.TempDir()
	for name, tc := range map[string]struct {
		yaml    string
		env     map[string]string
		want    Dispatch
		wantErr bool
	}{
		"defaults":         {yaml: "env: dev\n", want: DefaultDispatch()},
		"yaml":             {yaml: "dispatch: {max_attempts: 3, receipt_timeout: 10m}\n", want: Dispatch{3, 10 * time.Minute, 2 * time.Hour}},
		"env override":     {yaml: "dispatch: {max_attempts: 3}\n", env: map[string]string{"AS_DISPATCH_MAX_ATTEMPTS": "1"}, want: Dispatch{1, 30 * time.Minute, 2 * time.Hour}},
		"device wait":      {yaml: "dispatch: {device_wait_max: 15m}\n", env: map[string]string{"AS_DISPATCH_DEVICE_WAIT_MAX": "45m"}, want: Dispatch{2, 30 * time.Minute, 45 * time.Minute}},
		"zero device wait": {yaml: "dispatch: {device_wait_max: 0s}\n", wantErr: true},
		"zero attempts":    {yaml: "dispatch: {max_attempts: 0}\n", wantErr: true},
		"negative timeout": {yaml: "dispatch: {receipt_timeout: -1s}\n", wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			c, err := Load(writeFile(t, dir, "app.yaml", tc.yaml), "")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got %+v", c.Dispatch)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if c.Dispatch != tc.want {
				t.Fatalf("dispatch = %+v, want %+v", c.Dispatch, tc.want)
			}
		})
	}
}
