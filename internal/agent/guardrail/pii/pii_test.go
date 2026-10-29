package pii

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

func TestMask(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"phone", "联系 13800138000 张三", "联系 138****8000 张三"},
		{"phone 86 prefix", "电话8613800138000", "电话861******8000"},
		{"phone split", "电话 138-0013-8000 或 139 1234 5678", "电话 138****8000 或 139****5678"},
		{"phone in chinese text", "我的手机号是13912345678，请回电", "我的手机号是139****5678，请回电"},
		{"id card 18", "身份证110101199001011234", "身份证110***********1234"},
		{"id card X", "身份证 11010119900101123X 号", "身份证 110***********123X 号"},
		{"id card 15", "旧证 110101900101123", "旧证 110********1123"},
		{"account 16", "卡号 6222021234567890", "卡号 622*********7890"},
		{"account 19", "卡号6222021234567890123结束", "卡号622************0123结束"},
		{"coord pair", "位置 31.234567,120.567890 附近", "位置 31.23,120.56 附近"},
		{"coord full-width comma", "31.234567，120.567890", "31.23，120.56"},
		{"coord negative spaced", "-33.865143, 151.209900", "-33.86, 151.20"},
		{"coarse coord kept", "约 31.2,120.5", "约 31.2,120.5"},
		{"not coord out of range", "比值 123.4567,456.7890", "比值 123.4567,456.7890"},
		{"short numbers kept", "施肥 3.5 kg/亩，第 12 天，20 元，2026-08-20", "施肥 3.5 kg/亩，第 12 天，20 元，2026-08-20"},
		{"10 digits kept", "编号 1380013800", "编号 1380013800"},
		{"12 digits not phone", "123800138000", "123800138000"},
		{"20 digits kept", "12345678901234567890", "12345678901234567890"},
		{"non mobile 11 digits", "10800138000", "10800138000"},
		{"sha256 digest kept", "digest=" + strings.Repeat("1a", 32), "digest=" + strings.Repeat("1a", 32)},
		{"phone in token", "user13800138000x", "user138****8000x"},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Mask(tt.in); got != tt.want {
				t.Fatalf("Mask(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestMask_Categories 是评审 P1-2 的回归：每类都有应掩与不应掩的写法；坐标在日志口径下保留 0.01°。
func TestMask_Categories(t *testing.T) {
	tests := []struct{ category, name, in, want string }{
		// 手机号
		{"phone", "full-width", "电话１３８１２３４５６７８", "电话138****5678"},
		{"phone", "full-width mixed text", "电话：１３８１２３４５６７８，北纬３１.２３４５６７", "电话：138****5678，北纬31.23"},
		{"phone", "chinese numerals", "手机一三八一二三四五六七八", "手机138****5678"},
		{"phone", "chinese numerals 幺 两 零", "打幺三九零零幺两三四五六", "打139****3456"},
		{"phone", "keep chinese words", "一般一定唯一十分", "一般一定唯一十分"},
		{"phone", "keep 12 chinese numerals", "一三八一二三四五六七八九", "一三八一二三四五六七八九"},
		{"phone", "keep non mobile chinese", "一二三四五六七八九〇一", "一二三四五六七八九〇一"},
		{"phone", "keep invalid utf8 neighbour", "\xff１２３", "\xff１２３"},
		{"phone", "invalid utf8 then phone", "\xff１３８１２３４５６７８", "\xff138****5678"},
		// 身份证
		{"id", "dash 6-8-4", "身份证 110105-19900307-1234", "身份证 110***********1234"},
		{"id", "space 6-8-4 X", "110105 19900307 123X", "110***********123X"},
		{"id", "space 6-6-3", "旧证 110105 900307 123", "旧证 110********7123"},
		{"id", "groups of 4", "1101 0519 9003 0712 34", "110***********1234"},
		{"id", "full-width", "１１０１０１１９９００１０１１２３４", "110***********1234"},
		{"id", "keep date", "2026-08-20 至 2026-09-01", "2026-08-20 至 2026-09-01"},
		{"id", "keep three groups", "订单 1234-5678-9012", "订单 1234-5678-9012"},
		// 账号
		{"account", "space groups", "卡号 6222 0212 3456 7890", "卡号 622*********7890"},
		{"account", "dash groups 19", "6222-0212-3456-7890-123", "622************0123"},
		{"account", "sentence period", "卡号6222021234567890.", "卡号622*********7890."},
		{"account", "keep long decimal", "比值 0.70833333333333334", "比值 0.70833333333333334"},
		{"account", "keep long integer part of decimal", "12345678901234567.5", "12345678901234567.5"},
		// 摘要
		{"digest", "34 chars not digest", "a13812345678bcdef0123456789abcdef0", "a138****5678bcdef0123456789abcdef0"},
		{"digest", "keep md5", "md5=d41d8cd98f00b204e9800998ecf8427e", "md5=d41d8cd98f00b204e9800998ecf8427e"},
		{"digest", "keep sha1", "13812345678abcdef13812345678abcdef123456", "13812345678abcdef13812345678abcdef123456"},
		{"digest", "keep uuid", "550e8400-e29b-41d4-a716-446655440000", "550e8400-e29b-41d4-a716-446655440000"},
		// 坐标
		{"coord", "lng first", "120.567890,31.234567", "120.56,31.23"},
		{"coord", "space separated", "31.234567 120.567890", "31.23 120.56"},
		{"coord", "labelled", "北纬31.234567 东经120.567890", "北纬31.23 东经120.56"},
		{"coord", "labelled colon", "纬度：31.2345", "纬度：31.23"},
		{"coord", "full-width digits", "３１.２３４５６７,１２０.５６７８９０", "31.23,120.56"},
		{"coord", "chain", "31.234567,120.567890,31.234568", "31.23,120.56,31.23"},
		{"coord", "keep pH", "pH6.512, 7.123", "pH6.512, 7.123"},
		{"coord", "keep small spaced", "1.250 2.500", "1.250 2.500"},
		{"coord", "keep small ratio", "比例 0.1234, 0.5678", "比例 0.1234, 0.5678"},
		{"coord", "keep spaced 3 decimals", "31.234 120.567", "31.234 120.567"},
		{"coord", "keep labelled coarse", "北纬31.2", "北纬31.2"},
		{"coord", "keep labelled out of range", "纬度 123.4567", "纬度 123.4567"},
		{"coord", "keep long integer", "1234.5678,120.5678", "1234.5678,120.5678"},
	}
	for _, tt := range tests {
		t.Run(tt.category+"/"+tt.name, func(t *testing.T) {
			if got := Mask(tt.in); got != tt.want {
				t.Fatalf("Mask(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestForModel(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"text coord", "地块在31.234567,120.567890，电话13800138000", "地块在[FIELD_REF]，电话138****8000"},
		{"json coord keys", `{"lat":31.234567,"lng":120.56789,"q":"13800138000"}`,
			`{"lat":"[FIELD_REF]","lng":"[FIELD_REF]","q":"138****8000"}`},
		{"json text coord", `{"task":"advise","input":{"question":"31.123456,120.654321 这块地"}}`,
			`{"input":{"question":"[FIELD_REF] 这块地"},"task":"advise"}`},
		{"json coarse kept", `{"lat":31.2}`, `{"lat":31.2}`},
		{"text lng first spaced", "位置 120.567890 31.234567", "位置 [FIELD_REF]"},
		{"text labelled", "北纬31.234567 东经120.567890", "北纬[FIELD_REF] 东经[FIELD_REF]"},
		{"text chain one ref", "31.234567,120.567890,31.234568", "[FIELD_REF]"},
		{"text pH kept", "pH6.512, 7.123", "pH6.512, 7.123"},
		{"json location array", `{"location":[120.56789,31.23456]}`, `{"location":["[FIELD_REF]","[FIELD_REF]"]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ForModel(tt.in); got != tt.want {
				t.Fatalf("ForModel(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestMaskJSON(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"nested", `{"a":{"phone":"13800138000","n":[1,"110101199001011234"]},"field_id":1}`,
			`{"a":{"n":[1,"110***********1234"],"phone":"138****8000"},"field_id":1}`},
		{"numeric phone", `{"tel":13800138000}`, `{"tel":"138****8000"}`},
		{"location point keys", `{"location":[120.567890,31.234567],"point":{"x":1}}`,
			`{"location":[120.56,31.23],"point":{"x":1}}`},
		{"geo position keys", `{"geo":31.234567,"position":"31.234567,120.567890"}`,
			`{"geo":31.23,"position":"31.23,120.56"}`},
		{"location coarse kept", `{"location":31.2}`, `{"location":31.2}`},
		{"full-width phone string", `{"tel":"１３８１２３４５６７８"}`, `{"tel":"138****5678"}`},
		{"coord keys truncated", `{"latitude":31.234567,"longitude":120.567890,"coordinates":[[120.1234,31.5678]]}`,
			`{"coordinates":[[120.12,31.56]],"latitude":31.23,"longitude":120.56}`},
		{"values preserved", `{"v":3.14159,"big":12345678901234567890123,"ok":true,"x":null}`,
			`{"big":12345678901234567890123,"ok":true,"v":3.14159,"x":null}`},
		{"html not escaped", `{"s":"a<b>&c"}`, `{"s":"a<b>&c"}`},
		{"invalid json becomes string", `not json 13800138000`, `"not json 138****8000"`},
		{"trailing data becomes string", `{} {}`, `"{} {}"`},
		{"empty", ``, ``},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MaskJSON(json.RawMessage(tt.in))
			if string(got) != tt.want {
				t.Fatalf("MaskJSON(%s) = %s, want %s", tt.in, got, tt.want)
			}
			if tt.in != "" && !json.Valid(got) {
				t.Fatalf("output is not valid JSON: %s", got)
			}
		})
	}
}

type secret struct {
	Phone string  `json:"phone"`
	Lat   float64 `json:"lat"`
}

type stringer struct{}

func (stringer) String() string { return "call 13800138000" }

func TestHandler(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(NewHandler(slog.NewJSONHandler(&buf, nil))).With("pre", "13800138000")
	wrapped := fmt.Errorf("upstream: %w", errors.New("user 110101199001011234 not found"))
	log.WithGroup("g").Info("ask from 13912345678",
		"q", "地块 31.234567,120.567890",
		"err", wrapped,
		"id", int64(13800138000),
		"field_id", 12,
		"lat", 31.234567,
		"lng", 120.5,
		"obj", secret{Phone: "13700001111", Lat: 30.123456},
		"raw", json.RawMessage(`{"p":"13600001111"}`),
		"s", stringer{},
		slog.Group("inner", "phone", "13500001111"),
	)
	out := buf.String()
	for _, leak := range []string{"13800138000", "13912345678", "110101199001011234", "31.234567", "120.567890",
		"13700001111", "30.123456", "13600001111", "13500001111"} {
		if strings.Contains(out, leak) {
			t.Errorf("log leaks %q: %s", leak, out)
		}
	}
	for _, keep := range []string{`"field_id":12`, `"lng":120.5`, `"lat":"31.23"`, "upstream: user 110***********1234 not found",
		`"pre":"138****8000"`, `"msg":"ask from 139****5678"`, `"q":"地块 31.23,120.56"`} {
		if !strings.Contains(out, keep) {
			t.Errorf("log missing %q: %s", keep, out)
		}
	}
	if h := NewHandler(slog.NewTextHandler(&buf, nil)); h.Unwrap() == nil {
		t.Fatal("Unwrap returned nil")
	}
}

func TestMaskAttrs_DoesNotMutateInput(t *testing.T) {
	in := []slog.Attr{slog.String("p", "13800138000")}
	out := MaskAttrs(in)
	if in[0].Value.String() != "13800138000" || out[0].Value.String() != "138****8000" {
		t.Fatalf("in = %v, out = %v", in, out)
	}
}
