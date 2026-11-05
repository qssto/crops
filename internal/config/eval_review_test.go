package config

import (
	"errors"
	"strings"
	"testing"
)

func TestEvalReview_Validate(t *testing.T) {
	ok := DefaultEvalReview()
	ok.Enabled, ok.User = true, "eval_reviewer"
	tests := []struct {
		name    string
		mutate  func(*EvalReview)
		wantErr string // 空表示应通过
	}{
		{name: "disabled skips checks", mutate: func(e *EvalReview) { *e = EvalReview{} }},
		{name: "enabled defaults", mutate: func(*EvalReview) {}},
		{name: "empty host", mutate: func(e *EvalReview) { e.Host = "" }, wantErr: "host"},
		{name: "bad port", mutate: func(e *EvalReview) { e.Port = 70000 }, wantErr: "port"},
		{name: "empty database", mutate: func(e *EvalReview) { e.Database = "" }, wantErr: "database"},
		{name: "zero conns", mutate: func(e *EvalReview) { e.MaxOpenConns = 0 }, wantErr: "max_open_conns"},
		{name: "zero interval", mutate: func(e *EvalReview) { e.EnqueueInterval = 0 }, wantErr: "enqueue_interval"},
		{name: "zero batch", mutate: func(e *EvalReview) { e.EnqueueBatch = 0 }, wantErr: "enqueue_batch"},
		{name: "max batch", mutate: func(e *EvalReview) { e.EnqueueBatch = MaxEvalReviewBatch }},
		{name: "batch over max", mutate: func(e *EvalReview) { e.EnqueueBatch = MaxEvalReviewBatch + 1 }, wantErr: "enqueue_batch"},
		// 红线：独立账号，不得复用业务库账号
		{name: "same account as business db", mutate: func(e *EvalReview) { e.User = "root" }, wantErr: "differ from mysql.user"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := ok
			tt.mutate(&e)
			err := e.Validate("root")
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected: %v", err)
				}
				return
			}
			if !errors.Is(err, ErrBadEvalReview) || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want ErrBadEvalReview mentioning %q", err, tt.wantErr)
			}
		})
	}
}

func TestEvalReview_DSN(t *testing.T) {
	e := DefaultEvalReview()
	e.User, e.Password = "eval_reviewer", "p@ss"
	got := e.DSN()
	for _, want := range []string{"eval_reviewer:", "tcp(127.0.0.1:3306)/agri_eval?", "parseTime=true", "loc=UTC"} {
		if !strings.Contains(got, want) {
			t.Errorf("DSN %q missing %q", "<redacted>", want)
		}
	}
	if strings.Contains(got, "multiStatements") {
		t.Error("multiStatements must be off")
	}
}

// 红线：账号与口令只来自环境变量 / deploy/.env；yaml 中写了即启动失败；仓库配置缺省不启用。
func TestLoad_EvalReview(t *testing.T) {
	for _, s := range secrets {
		for _, n := range s.envs {
			t.Setenv(n, "")
		}
	}
	c, err := Load("../../configs/app.yaml", "")
	if err != nil {
		t.Fatal(err)
	}
	if c.EvalReview != DefaultEvalReview() {
		t.Fatalf("eval_review = %+v, want %+v", c.EvalReview, DefaultEvalReview())
	}
	if err := c.RequireSecrets("eval_review.user", "eval_review.password"); err == nil {
		t.Fatal("unset eval_review secrets must be reported")
	}

	dir := t.TempDir()
	for _, key := range []string{"user", "password"} {
		leaked := writeFile(t, dir, key+".yaml", "eval_review: {enabled: true, "+key+": leaked}\n")
		if _, err := Load(leaked, ""); !errors.Is(err, ErrSecretInFile) {
			t.Fatalf("eval_review.%s in yaml: err = %v, want ErrSecretInFile", key, err)
		}
	}

	dotenv := writeFile(t, dir, ".env", "EVAL_REVIEW_USER=eval_reviewer\nEVAL_REVIEW_PASSWORD=from-dotenv\n")
	t.Setenv("AS_EVAL_REVIEW_ENABLED", "true")
	if c, err = Load("../../configs/app.yaml", dotenv); err != nil {
		t.Fatal(err)
	}
	if !c.EvalReview.Enabled || c.EvalReview.User != "eval_reviewer" || c.EvalReview.Password != "from-dotenv" {
		t.Fatalf("enabled from env: %+v", c.EvalReview.Enabled)
	}
	if err := c.RequireSecrets("eval_review.user", "eval_review.password"); err != nil {
		t.Fatal(err)
	}

	t.Setenv("AS_EVAL_REVIEW_USER", "root") // 与 mysql.user 相同
	if _, err := Load("../../configs/app.yaml", ""); !errors.Is(err, ErrBadEvalReview) {
		t.Fatalf("same account err = %v, want ErrBadEvalReview", err)
	}
}
