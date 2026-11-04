package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"agri-steward/internal/evalkit"
	"agri-steward/internal/evalkit/report"
)

// keyPutter 记录上传的对象键与内容。
type keyPutter struct{ objs map[string]string }

func (p *keyPutter) PutObject(_ context.Context, _, key string, b []byte, _ string) error {
	p.objs[key] = string(b)
	return nil
}

// run 与 resume 共用的出报告步骤：holdout 明细只进 report/holdout/，本地报告里 holdout trial 的证据被脱敏。
func TestWriteReport_RedactsHoldout(t *testing.T) {
	const secret = "holdout-evidence-text"
	exp := &evalkit.Experiment{ID: "7", Samples: 2, Trials: []evalkit.Trial{
		{SampleID: "pub-1", Suite: "regression", Scores: []evalkit.Score{{Name: "x", Pass: true}}},
		{SampleID: "ho-1", Suite: "holdout", Holdout: true, Scores: []evalkit.Score{{Name: "x", Evidence: secret}}},
	}}
	obj := &keyPutter{objs: map[string]string{}}
	var out bytes.Buffer
	pub, path, err := writeReport(t.Context(), obj, "eval", t.TempDir(), exp, &out)
	if err != nil {
		t.Fatal(err)
	}
	hoKey := report.HoldoutReportKey("7")
	if !strings.Contains(obj.objs[hoKey], secret) || !strings.Contains(out.String(), hoKey) {
		t.Fatalf("holdout report not uploaded: keys=%v stdout=%q", obj.objs, out.String())
	}
	local, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(local), secret) || pub.Trials[1].Scores[0].Evidence == secret {
		t.Fatalf("local report leaks holdout evidence")
	}
	if exp.Trials[1].Scores[0].Evidence != secret {
		t.Fatal("writeReport modified its input")
	}
}

// 快照套件含 holdout 的实验须授权后才能补跑；不含 holdout 的不受影响。
func TestRequireHoldoutGrant(t *testing.T) {
	if err := requireHoldoutGrant([]string{"regression", "holdout"}, false); !errors.Is(err, errResumeHoldout) {
		t.Fatalf("unauthorized holdout resume: err = %v", err)
	}
	for _, c := range []struct {
		suites  []string
		granted bool
	}{{[]string{"regression", "holdout"}, true}, {[]string{"regression"}, false}, {nil, false}} {
		if err := requireHoldoutGrant(c.suites, c.granted); err != nil {
			t.Fatalf("%v granted=%v: err = %v", c.suites, c.granted, err)
		}
	}
}
