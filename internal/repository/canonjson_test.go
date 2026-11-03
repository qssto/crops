package repository

import (
	"errors"
	"math"
	"testing"
)

func TestCanonicalJSON_RFC8785Samples(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{ // RFC 8785 §3.2.2 示例
			name: "rfc_3.2.2",
			in: `{
  "numbers": [333333333.33333329, 1E30, 4.50, 2e-3, 0.000000000000000000000000001],
  "string": "\u20ac$\u000F\u000aA'\u0042\u0022\u005c\\\"\/",
  "literals": [null, true, false]
}`,
			want: `{"literals":[null,true,false],"numbers":[333333333.3333333,1e+30,4.5,0.002,1e-27],"string":"€$\u000f\nA'B\"\\\\\"/"}`,
		},
		{ // RFC 8785 §3.2.3 排序示例：按 UTF-16 码元，😀（代理对 D83D）排在 U+FB33 之前
			name: "rfc_3.2.3_sort",
			in: `{"\u20ac":"Euro Sign","\r":"Carriage Return","\ufb33":"Hebrew Letter Dalet With Dagesh","1":"One",` +
				`"\ud83d\ude00":"Emoji: Grinning Face","\u0080":"Control","\u00f6":"Latin Small Letter O With Diaeresis"}`,
			want: "{\"\\r\":\"Carriage Return\",\"1\":\"One\",\"\u0080\":\"Control\",\"ö\":\"Latin Small Letter O With Diaeresis\"," +
				"\"€\":\"Euro Sign\",\"😀\":\"Emoji: Grinning Face\",\"\ufb33\":\"Hebrew Letter Dalet With Dagesh\"}",
		},
		{name: "nested_whitespace", in: " { \"b\" : [ 1 , { \"d\":1, \"c\":2 } ] , \"a\" : \"x\" } ", want: `{"a":"x","b":[1,{"c":2,"d":1}]}`},
		{name: "dup_key_last_wins", in: `{"a":1,"a":2}`, want: `{"a":2}`},
		{name: "control_escape", in: `"\u0001\b\f\t\u001f"`, want: `"\u0001\b\f\t\u001f"`},
		{name: "unicode_raw", in: `"\u4e2d文"`, want: `"中文"`},
		{name: "number_forms", in: `[-0, 1e2, 1.0, -1.5e-7, 100e-2]`, want: `[0,100,1,-1.5e-7,1]`},
		{name: "beyond_2^53_rounds", in: `9007199254740993`, want: `9007199254740992`},
		{name: "empty_containers", in: `{"a":{},"b":[]}`, want: `{"a":{},"b":[]}`},
		{name: "scalar_literals", in: `null`, want: `null`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := CanonicalJSON([]byte(c.in))
			if err != nil {
				t.Fatalf("CanonicalJSON: %v", err)
			}
			if string(got) != c.want {
				t.Fatalf("got  %s\nwant %s", got, c.want)
			}
			again, err := CanonicalJSON(got) // 幂等：规范化结果再规范化不变
			if err != nil || string(again) != c.want {
				t.Fatalf("not idempotent: %s (%v)", again, err)
			}
		})
	}
}

func TestCanonicalJSON_Rejects(t *testing.T) {
	for _, in := range []string{
		``, `{`, `[1,]`, `{"a" 1}`, `{} {}`, `NaN`, `Infinity`, `[1e400]`, `{1:2}`, `'a'`, `"unterminated`,
	} {
		t.Run(in, func(t *testing.T) {
			if got, err := CanonicalJSON([]byte(in)); !errors.Is(err, ErrNotCanonicalizable) {
				t.Fatalf("want ErrNotCanonicalizable, got %q / %v", got, err)
			}
		})
	}
}

// RFC 8785 附录 B：IEEE-754 位模式 → ES6 文本。
func TestES6Number_AppendixB(t *testing.T) {
	cases := []struct {
		bits uint64
		want string
	}{
		{0x0000000000000000, "0"},
		{0x8000000000000000, "0"},
		{0x0000000000000001, "5e-324"},
		{0x8000000000000001, "-5e-324"},
		{0x7fefffffffffffff, "1.7976931348623157e+308"},
		{0xffefffffffffffff, "-1.7976931348623157e+308"},
		{0x4340000000000000, "9007199254740992"},
		{0xc340000000000000, "-9007199254740992"},
		{0x4430000000000000, "295147905179352830000"},
		{0x44b52d02c7e14af5, "9.999999999999997e+22"},
		{0x44b52d02c7e14af6, "1e+23"},
		{0x44b52d02c7e14af7, "1.0000000000000001e+23"},
		{0x444b1ae4d6e2ef4e, "999999999999999700000"},
		{0x444b1ae4d6e2ef4f, "999999999999999900000"},
		{0x444b1ae4d6e2ef50, "1e+21"},
		{0x3eb0c6f7a0b5ed8c, "9.999999999999997e-7"},
		{0x3eb0c6f7a0b5ed8d, "0.000001"},
		{0x41b3de4355555553, "333333333.3333332"},
		{0x41b3de4355555554, "333333333.33333325"},
		{0x41b3de4355555555, "333333333.3333333"},
		{0x41b3de4355555556, "333333333.3333334"},
		{0x41b3de4355555557, "333333333.33333343"},
		{0xbecbf647612f3696, "-0.0000033333333333333333"},
		{0x43143ff3c1cb0959, "1424953923781206.2"},
	}
	for _, c := range cases {
		got, err := es6Number(math.Float64frombits(c.bits))
		if err != nil || got != c.want {
			t.Errorf("%016x: got %q (%v), want %q", c.bits, got, err, c.want)
		}
	}
	for _, f := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if _, err := es6Number(f); !errors.Is(err, ErrNotCanonicalizable) {
			t.Errorf("%v: want ErrNotCanonicalizable, got %v", f, err)
		}
	}
}

func TestCompareUTF16_SurrogateOrder(t *testing.T) {
	// 按码点 U+1F600 > U+FB33，按 UTF-16 码元 0xD83D < 0xFB33。
	if compareUTF16("😀", "\ufb33") >= 0 {
		t.Fatal("surrogate pair must sort before U+FB33")
	}
	if compareUTF16("a", "a") != 0 || compareUTF16("a", "ab") >= 0 {
		t.Fatal("prefix order broken")
	}
}
