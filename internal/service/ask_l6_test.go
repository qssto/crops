package service

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"agri-steward/internal/agent"
	"agri-steward/internal/model"
)

// P2 L6 会话续接建 run：enable_compaction 开时 prev_run_id / 复检取代走 CreateRunLinked，否则同 P1 走 CreateRun。

// compactionFlagReader：enable_advice 恒开；enable_compaction 取 on，cErr 非空时读失败。
type compactionFlagReader struct {
	on   bool
	cErr error
}

func (f compactionFlagReader) Bool(_ context.Context, key model.FlagKey, _ model.Scope) (bool, error) {
	if key == model.FlagEnableCompaction {
		return f.on, f.cErr
	}
	return key == model.FlagEnableAdvice, nil
}

type fakeSessions struct {
	links []model.RunLink
	err   error
}

func (f *fakeSessions) CreateRunLinked(_ context.Context, _ model.RunState, _, _ uint64, _ agent.RunMeta,
	link model.RunLink) (uint64, bool, error) {
	f.links = append(f.links, link)
	return 88, true, f.err
}

func TestAsk_SessionLink(t *testing.T) {
	const idle = 30 * time.Minute
	errLinkedDB := errors.New("db down")
	for _, tt := range []struct {
		name      string
		in        AskInput
		flags     compactionFlagReader
		noSess    bool
		sessErr   error
		wantRun   uint64
		wantLink  *model.RunLink
		wantErr   error
		wantP1Run bool
	}{
		{name: "no link uses P1 create", in: AskInput{FieldID: 3, Text: "x"}, flags: compactionFlagReader{on: true},
			wantRun: 77, wantP1Run: true},
		{name: "prev run with switch on", in: AskInput{FieldID: 3, Text: "x", PrevRunID: 5}, flags: compactionFlagReader{on: true},
			wantRun: 88, wantLink: &model.RunLink{Kind: model.RunLinkSession, RunID: 5, Idle: idle}},
		{name: "supersede with switch on", in: AskInput{FieldID: 3, Text: "x", Supersedes: 6}, flags: compactionFlagReader{on: true},
			wantRun: 88, wantLink: &model.RunLink{Kind: model.RunLinkSupersede, RunID: 6}},
		{name: "prev run with switch off", in: AskInput{FieldID: 3, Text: "x", PrevRunID: 5}, wantRun: 77, wantP1Run: true},
		{name: "flag read failed opens new session", in: AskInput{FieldID: 3, Text: "x", PrevRunID: 5},
			flags: compactionFlagReader{on: true, cErr: errors.New("redis down")}, wantRun: 77, wantP1Run: true},
		{name: "sessions not wired", in: AskInput{FieldID: 3, Text: "x", PrevRunID: 5}, flags: compactionFlagReader{on: true},
			noSess: true, wantRun: 77, wantP1Run: true},
		{name: "linked create error", in: AskInput{FieldID: 3, Text: "x", PrevRunID: 5}, flags: compactionFlagReader{on: true},
			sessErr: errLinkedDB, wantErr: errLinkedDB},
		{name: "prev and supersede exclusive", in: AskInput{FieldID: 3, Text: "x", PrevRunID: 5, Supersedes: 6},
			flags: compactionFlagReader{on: true}, wantErr: ErrBadInput},
	} {
		t.Run(tt.name, func(t *testing.T) {
			runs, sess := &fakeRuns{}, &fakeSessions{err: tt.sessErr}
			d := AskDeps{Fields: fakeFields{ok: true}, Limiter: fakeLimiter{allow: true}, Runs: runs, Advice: fakeAdvice{},
				Agent: &fakeAgent{}, Flags: tt.flags, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), SessionIdle: idle}
			if !tt.noSess {
				d.Sessions = sess
			}
			svc := NewAskService(d)
			id, err := svc.Ask(context.Background(), askScope, tt.in)
			svc.Wait()
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil || id != tt.wantRun {
				t.Fatalf("Ask = %d, %v; want %d", id, err, tt.wantRun)
			}
			if (runs.created == 1) != tt.wantP1Run {
				t.Fatalf("P1 CreateRun calls = %d, want P1 path %v", runs.created, tt.wantP1Run)
			}
			if tt.wantLink == nil {
				if len(sess.links) != 0 {
					t.Fatalf("linked creates = %+v, want none", sess.links)
				}
				return
			}
			if len(sess.links) != 1 || sess.links[0] != *tt.wantLink {
				t.Fatalf("links = %+v, want %+v", sess.links, *tt.wantLink)
			}
		})
	}
}
