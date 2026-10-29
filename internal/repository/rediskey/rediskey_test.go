package rediskey

import (
	"errors"
	"testing"
)

func TestKeys(t *testing.T) {
	k, err := New("dev")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct{ got, want string }{
		{k.Prefix(), "as:dev:"},
		{k.LockOrder(42), "as:dev:lock:order:42"},
		{k.LockScan("field_sync"), "as:dev:lock:scan:field_sync"},
		{k.LockRun(9), "as:dev:lock:run:9"},
		{k.RateFarmAsk(1), "as:dev:rl:farm:1:ask"},
		{k.Raw("sse:run:7"), "as:dev:sse:run:7"},
		{k.IdemTool("abc"), "as:dev:idem:tool:abc"},
		{k.IdemApprove("r-1"), "as:dev:idem:approve:r-1"},
		{k.CacheMCP("weather.forecast", "d1"), "as:dev:cache:mcp:weather.forecast:d1"},
		{EvalTrialEnv(3), "eval-t3"},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("got %q, want %q", tt.got, tt.want)
		}
	}
	ek, err := New(EvalTrialEnv(3))
	if err != nil || ek.LockOrder(1) != "as:eval-t3:lock:order:1" {
		t.Fatalf("eval key = %q, err %v", ek.LockOrder(1), err)
	}
}

func TestNew_BadEnv(t *testing.T) {
	for _, env := range []string{"", "Dev", "a:b", "x*", "-dev", "a b"} {
		if _, err := New(env); !errors.Is(err, ErrBadEnv) {
			t.Errorf("New(%q) err = %v, want ErrBadEnv", env, err)
		}
	}
}
