package calc

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"agri-steward/internal/agent/knowledge"
)

func repoCalc(t *testing.T) *Calculator {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
	snap, err := knowledge.NewStore(knowledge.NewRepoReader(dir)).Open(context.Background(), knowledge.RefWorktree)
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(snap)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func use(product, target string) Use {
	return Use{ProductID: product, Crop: "rice", Target: target, Region: "jiangsu"}
}

const ddRef = "common/operation_window/dosage-dilution@v1#"

// 期望值均按知识条目与登记表手算（见每条注释）。
func TestNumeric(t *testing.T) {
	c := repoCalc(t)
	type fn func() (Result, error)
	tests := []struct {
		name    string
		run     fn
		want    float64 // 按 Precision 舍入后的值
		unit    string
		formula string
		wantErr error
	}{
		{name: "dosage pymetrozine 10 g × 25 亩 = 250 g", want: 250, unit: "g", formula: ddRef + "total_dose",
			run: func() (Result, error) { return c.Dosage(use("pymetrozine-50wg", "planthopper"), 10, 25) }},
		{name: "dosage chlorantraniliprole 10 mL × 32.5 亩 = 325 mL", want: 325, unit: "mL", formula: ddRef + "total_dose",
			run: func() (Result, error) { return c.Dosage(use("chlorantraniliprole-200sc", "stem_borer"), 10, 32.5) }},
		{name: "dosage at registered min 100 mL × 0.3 亩 = 30 mL", want: 30, unit: "mL", formula: ddRef + "total_dose",
			run: func() (Result, error) { return c.Dosage(use("validamycin-5sl", "false_smut"), 100, 0.3) }},
		{name: "dosage above registered max fails", wantErr: ErrOutOfRange,
			run: func() (Result, error) { return c.Dosage(use("pymetrozine-50wg", "planthopper"), 12.5, 25) }},
		{name: "dosage area 0 fails", wantErr: ErrOutOfRange,
			run: func() (Result, error) { return c.Dosage(use("pymetrozine-50wg", "planthopper"), 10, 0) }},
		{name: "dosage wrong target not registered", wantErr: ErrNotRegistered,
			run: func() (Result, error) { return c.Dosage(use("pymetrozine-50wg", "stem_borer"), 10, 25) }},
		{name: "dosage out_of_set product not registered on rice", wantErr: ErrNotRegistered,
			run: func() (Result, error) { return c.Dosage(use("boscalid-50wg", "sheath_blight"), 40, 10) }},
		{name: "dosage on ratio registration is unit mismatch", wantErr: ErrUnitMismatch,
			run: func() (Result, error) { return c.Dosage(use("prochloraz-25ec", "bakanae"), 2000, 10) }},
		{name: "dilution uav 1.5 L ÷ 10 mL = 150 倍", want: 150, unit: "倍", formula: ddRef + "dilution_ratio",
			run: func() (Result, error) {
				return c.Dilution(use("chlorantraniliprole-200sc", "leaf_roller"), 10, SprayerUAV, 1.5)
			}},
		{name: "dilution knapsack 30 L ÷ 10 g = 3000 倍", want: 3000, unit: "倍", formula: ddRef + "dilution_ratio",
			run: func() (Result, error) {
				return c.Dilution(use("pymetrozine-50wg", "planthopper"), 10, SprayerKnapsack, 30)
			}},
		{name: "dilution knapsack volume below kb range fails", wantErr: ErrOutOfRange,
			run: func() (Result, error) {
				return c.Dilution(use("pymetrozine-50wg", "planthopper"), 10, SprayerKnapsack, 15)
			}},
		{name: "dilution unknown sprayer fails", wantErr: ErrOutOfRange,
			run: func() (Result, error) { return c.Dilution(use("pymetrozine-50wg", "planthopper"), 10, "drone", 1.5) }},
		{name: "spray volume uav default 1.5 L/亩", want: 1.5, unit: "L/亩",
			formula: "common/operation_window/uav@v1#uav_spray_volume",
			run:     func() (Result, error) { return c.SprayVolume(SprayerUAV) }},
		{name: "bakanae soak 50 L at 2000 倍 = 25 mL", want: 25, unit: "mL",
			formula: "rice/pest/bakanae@v1#dose_from_dilution",
			run:     func() (Result, error) { return c.DoseFromDilution(use("prochloraz-25ec", "bakanae"), 2000, 50) }},
		{name: "bakanae ratio 1500 below registered 2000 fails", wantErr: ErrOutOfRange,
			run: func() (Result, error) { return c.DoseFromDilution(use("phenamacril-25sc", "bakanae"), 1500, 50) }},
		{name: "dose-from-dilution on per-mu registration is unit mismatch", wantErr: ErrUnitMismatch,
			run: func() (Result, error) { return c.DoseFromDilution(use("pymetrozine-50wg", "planthopper"), 2000, 50) }},
		{name: "water 28 亩 0→3 cm after drain = 672 m3", want: 672, unit: "m3",
			formula: "rice/irrigation/water-volume@v1#water_m3",
			run:     func() (Result, error) { return c.WaterVolume(28, 3, 0, true) }},
		{name: "water 30 亩 1→3 cm with standing water = 420 m3", want: 420, unit: "m3",
			formula: "rice/irrigation/water-volume@v1#water_m3",
			run:     func() (Result, error) { return c.WaterVolume(30, 3, 1, false) }},
		{name: "water 35 亩 0→3 cm after drain = 840 m3", want: 840, unit: "m3",
			formula: "rice/irrigation/water-volume@v1#water_m3",
			run:     func() (Result, error) { return c.WaterVolume(35, 3, 0, true) }},
		{name: "water target not above current fails", wantErr: ErrOutOfRange,
			run: func() (Result, error) { return c.WaterVolume(30, 3, 3, false) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := tt.run()
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				if r.Value != 0 || r.Trace.Formula != "" {
					t.Fatalf("failed call must not return a value, got %+v", r)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := r.Rounded(); got != tt.want || r.Unit != tt.unit || r.Trace.Formula != tt.formula {
				t.Fatalf("got %v %s %s, want %v %s %s", got, r.Unit, r.Trace.Formula, tt.want, tt.unit, tt.formula)
			}
			if len(r.Trace.Inputs) == 0 {
				t.Fatal("trace inputs empty")
			}
		})
	}
}

func TestFertilizer(t *testing.T) {
	c := repoCalc(t)
	tests := []struct {
		name            string
		in              FertInput
		n, perMu, total float64 // 舍入后：0.01 / 0.1 / 0.5
		wantErr         error
	}{
		// 20 × 0.25 = 5 kg N；÷ 0.46 = 10.87 → 10.9；× 25 = 271.74 → 271.5
		{name: "tillering urea soil low", in: FertInput{"低", FertTillering, ProductUrea, SplitNone, 25},
			n: 5, perMu: 10.9, total: 271.5},
		// 5 × 0.6 = 3；÷ 0.46 = 6.52 → 6.5；× 25 = 163.04 → 163
		{name: "tillering split first", in: FertInput{"低", FertTillering, ProductUrea, SplitFirst, 25},
			n: 3, perMu: 6.5, total: 163},
		// 5 × 0.4 = 2；÷ 0.46 = 4.35 → 4.3；× 25 = 108.70 → 108.5
		{name: "tillering split second", in: FertInput{"低", FertTillering, ProductUrea, SplitSecond, 25},
			n: 2, perMu: 4.3, total: 108.5},
		// 18 × 0.24 = 4.32；÷ 0.46 = 9.391 → 9.4；× 32.5 = 305.22 → 305
		{name: "panicle promote urea soil mid", in: FertInput{"中", FertPaniclePromote, ProductUrea, SplitNone, 32.5},
			n: 4.32, perMu: 9.4, total: 305},
		// 18 × 0.16 = 2.88；÷ 0.46 = 6.26 → 6.3；× 30 = 187.83 → 188
		{name: "panicle protect urea soil mid", in: FertInput{"中", FertPanicleProtect, ProductUrea, SplitNone, 30},
			n: 2.88, perMu: 6.3, total: 188},
		// 18 × 0.35 = 6.3；÷ 0.15 = 42；× 30 = 1260
		{name: "basal compound soil mid", in: FertInput{"中", FertBasal, ProductCompound, SplitNone, 30},
			n: 6.3, perMu: 42, total: 1260},
		// 16 × 0.35 = 5.6；÷ 0.15 = 37.33 → 37.3；× 35 = 1306.67 → 1306.5
		{name: "basal compound soil high", in: FertInput{"高", FertBasal, ProductCompound, SplitNone, 35},
			n: 5.6, perMu: 37.3, total: 1306.5},
		{name: "unknown soil level fails", in: FertInput{"很低", FertBasal, ProductUrea, SplitNone, 10}, wantErr: ErrOutOfRange},
		{name: "split on basal fails", in: FertInput{"中", FertBasal, ProductUrea, SplitFirst, 10}, wantErr: ErrOutOfRange},
		{name: "unknown product fails", in: FertInput{"中", FertBasal, "kcl", SplitNone, 10}, wantErr: ErrOutOfRange},
		{name: "negative area fails", in: FertInput{"中", FertBasal, ProductUrea, SplitNone, -1}, wantErr: ErrOutOfRange},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := c.Fertilizer(tt.in)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got := []float64{r.NPerMu.Rounded(), r.ProductPerMu.Rounded(), r.ProductTotal.Rounded()}
			if want := []float64{tt.n, tt.perMu, tt.total}; !slices.Equal(got, want) {
				t.Fatalf("got %v, want %v", got, want)
			}
			const nb = "rice/fertilization/n-budget@v1#"
			for _, p := range [][2]string{{r.NPerMu.Trace.Formula, nb + "n_for_stage"},
				{r.ProductPerMu.Trace.Formula, nb + "product_per_mu"}, {r.ProductTotal.Trace.Formula, nb + "product_total"}} {
				if p[0] != p[1] {
					t.Errorf("formula %s, want %s", p[0], p[1])
				}
			}
		})
	}
}

func TestPHIDate(t *testing.T) {
	c := repoCalc(t)
	sh, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	d := func(s string, loc *time.Location) time.Time {
		v, err := time.ParseInLocation(time.DateOnly, s, loc)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	tests := []struct {
		name    string
		u       Use
		dir     PHIDirection
		at      time.Time
		loc     *time.Location
		want    string
		formula string
		wantErr error
	}{
		{name: "chlorantraniliprole PHI 7: harvest 10-15 → last spray 10-08", u: use("chlorantraniliprole-200sc", "leaf_roller"),
			dir: PHIBackward, at: d("2026-10-15", sh), loc: sh, want: "2026-10-08", formula: "common/safety/phi@v1#last_spray_date"},
		{name: "pymetrozine PHI 14: harvest 10-10 → 09-26", u: use("pymetrozine-50wg", "planthopper"),
			dir: PHIBackward, at: d("2026-10-10", sh), loc: sh, want: "2026-09-26", formula: "common/safety/phi@v1#last_spray_date"},
		{name: "triflumezopyrim PHI 21: harvest 10-10 → 09-19", u: use("triflumezopyrim-10sc", "planthopper"),
			dir: PHIBackward, at: d("2026-10-10", sh), loc: sh, want: "2026-09-19", formula: "common/safety/phi@v1#last_spray_date"},
		{name: "dinotefuran PHI 21: spray 09-19 → earliest harvest 10-10", u: use("dinotefuran-20sg", "planthopper"),
			dir: PHIForward, at: d("2026-09-19", sh), loc: sh, want: "2026-10-10", formula: "common/safety/phi@v1#earliest_harvest"},
		// UTC 2026-09-30T20:00 在上海已是 10-01；按农场时区取日历日
		{name: "utc instant converted to farm day", u: use("pymetrozine-50wg", "planthopper"),
			dir: PHIForward, at: time.Date(2026, 9, 30, 20, 0, 0, 0, time.UTC), loc: sh, want: "2026-10-15",
			formula: "common/safety/phi@v1#earliest_harvest"},
		// 跨美东夏令时结束（2026-11-01）：日历日 +7，不因 23/25 小时漂移
		{name: "calendar add across DST", u: use("chlorantraniliprole-200sc", "stem_borer"),
			dir: PHIForward, at: d("2026-10-28", ny), loc: ny, want: "2026-11-04", formula: "common/safety/phi@v1#earliest_harvest"},
		{name: "PHI null (herbicide) not applicable", u: use("bensulfuron-methyl-10wp", "weeds"),
			dir: PHIBackward, at: d("2026-10-10", sh), loc: sh, wantErr: ErrPHINotApplicable},
		{name: "nil farm timezone fails", u: use("pymetrozine-50wg", "planthopper"),
			dir: PHIBackward, at: d("2026-10-10", sh), wantErr: ErrOutOfRange},
		{name: "unknown direction fails", u: use("pymetrozine-50wg", "planthopper"),
			dir: "sideways", at: d("2026-10-10", sh), loc: sh, wantErr: ErrOutOfRange},
		{name: "unregistered product fails", u: use("bifenthrin-100ec", "planthopper"),
			dir: PHIBackward, at: d("2026-10-10", sh), loc: sh, wantErr: ErrNotRegistered},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := c.PHIDate(tt.u, tt.dir, tt.at, tt.loc)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if r.Date != tt.want || r.Trace.Formula != tt.formula || r.At.Location() != tt.loc {
				t.Fatalf("got %s %s %v, want %s %s", r.Date, r.Trace.Formula, r.At.Location(), tt.want, tt.formula)
			}
		})
	}
}

func TestCandidates(t *testing.T) {
	c := repoCalc(t)
	got, err := c.Candidates("rice", "planthopper", "jiangsu")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"pymetrozine-50wg", "triflumezopyrim-10sc", "dinotefuran-20sg"}; !slices.Equal(got, want) {
		t.Fatalf("planthopper got %v, want %v", got, want)
	}
	for _, target := range []string{"planthopper", "leaf_roller", "stem_borer", "sheath_blight", "blast", "false_smut", "bakanae", "weeds"} {
		got, err := c.Candidates("rice", target, "jiangsu")
		if err != nil {
			t.Fatalf("%s: %v", target, err)
		}
		for _, id := range []string{"boscalid-50wg", "bifenthrin-100ec"} {
			if slices.Contains(got, id) {
				t.Errorf("%s: out_of_set_test product %s in candidates", target, id)
			}
		}
	}
	for _, q := range [][3]string{{"rice", "kernel_smut", "jiangsu"}, {"wheat", "planthopper", "jiangsu"}, {"rice", "gray_mold", "jiangsu"}} {
		if got, err := c.Candidates(q[0], q[1], q[2]); !errors.Is(err, ErrNoCandidates) || got != nil {
			t.Errorf("%v: got %v, %v; want ErrNoCandidates", q, got, err)
		}
	}
}

func TestRound(t *testing.T) {
	for _, tt := range []struct{ v, p, want float64 }{
		{0.15, 0.1, 0.2}, {2.25, 0.5, 2.5}, {682.5006, 1, 683}, {-1.25, 0.1, -1.3},
		{0.1 * 3, 0.1, 0.3}, {271.739, 0.5, 271.5}, {1.005, 0.01, 1.01}, {7, 0, 7},
	} {
		if got := Round(tt.v, tt.p); got != tt.want {
			t.Errorf("Round(%v, %v) = %v, want %v", tt.v, tt.p, got, tt.want)
		}
	}
	if !Equal(10.87, 10.9, PrecisionFertPerMu) || Equal(10.8, 10.9, PrecisionFertPerMu) {
		t.Error("Equal by precision")
	}
	if math.IsNaN(Round(math.NaN(), 1)) == false {
		t.Error("NaN must stay NaN")
	}
}

func TestNew_MissingFormula(t *testing.T) {
	if _, err := New(nil); err == nil {
		t.Fatal("nil snapshot must be rejected")
	}
}
