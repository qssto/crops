package config

import (
	"errors"
	"testing"
	"time"
)

func validQuality() Quality {
	return Quality{Interval: time.Hour, SamplePerCategory: 5, SampleSalt: "s", JudgeTier: "standard", JudgeQPM: 6,
		JudgesDir: "eval/judges", ReportTimezone: "Asia/Shanghai"}
}

func TestQuality_Validate(t *testing.T) {
	if err := validQuality().Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*Quality){
		"interval 0": func(q *Quality) { q.Interval = 0 },
		"sample 0":   func(q *Quality) { q.SamplePerCategory = 0 },
		"qpm 0":      func(q *Quality) { q.JudgeQPM = 0 },
		"no salt":    func(q *Quality) { q.SampleSalt = "" },
		"no tier":    func(q *Quality) { q.JudgeTier = "" },
		"no judges":  func(q *Quality) { q.JudgesDir = "" },
		"no tz":      func(q *Quality) { q.ReportTimezone = "" },
		"bad tz":     func(q *Quality) { q.ReportTimezone = "Mars/Olympus" },
	} {
		q := validQuality()
		mut(&q)
		if err := q.Validate(); !errors.Is(err, ErrBadQuality) {
			t.Errorf("%s: err = %v, want ErrBadQuality", name, err)
		}
	}
}
