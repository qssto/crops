package app

import (
	"context"
	"errors"
	"testing"
)

func TestBuildP2Loops(t *testing.T) {
	run := func(context.Context) {}
	one := func(name string) p2LoopBuilder {
		return func(*App) ([]Loop, error) { return []Loop{{Name: name, Run: run}}, nil }
	}
	none := func(*App) ([]Loop, error) { return nil, nil }
	boom := errors.New("boom")

	got, err := buildP2Loops(nil, []p2LoopEntry{{"P2-L3", one("quality")}, {"P2-L4", none}, {"P2-L5", one("kb")}})
	if err != nil || len(got) != 2 || got[0].Name != "quality" || got[1].Name != "kb" {
		t.Fatalf("ordered build = %v, %v", got, err)
	}
	bad := map[string][]p2LoopEntry{
		"nil builder":    {{"P2-L3", nil}},
		"empty name":     {{"", none}},
		"dup entry":      {{"P2-L3", none}, {"P2-L3", none}},
		"dup loop name":  {{"P2-L4", one("x")}, {"P2-L5", one("x")}},
		"nil run":        {{"P2-L3", func(*App) ([]Loop, error) { return []Loop{{Name: "q"}}, nil }}},
		"builder errors": {{"P2-L3", func(*App) ([]Loop, error) { return nil, boom }}},
	}
	for name, entries := range bad {
		if _, err := buildP2Loops(nil, entries); err == nil {
			t.Fatalf("%s: want error", name)
		}
	}
	if _, err := buildP2Loops(nil, []p2LoopEntry{{"P2-L3", func(*App) ([]Loop, error) { return nil, boom }}}); !errors.Is(err, boom) {
		t.Fatalf("builder error not wrapped: %v", err)
	}
}
