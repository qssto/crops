package quality

import (
	"fmt"
	"time"
)

// Day 是农场时区下的一个业务日：Date 为 yyyy-mm-dd，[From, To) 为该日在 UTC 下的时间窗（advice_card.created_at 存 UTC）。
type Day struct {
	Date     string
	From, To time.Time
}

// PrevDay 是 now 在 loc 时区下的前一日（夏令时切换日按当地零点切，窗口可能是 23 / 25 小时）。
func PrevDay(now time.Time, loc *time.Location) Day {
	y, m, d := now.In(loc).Date()
	today := time.Date(y, m, d, 0, 0, 0, 0, loc)
	prev := time.Date(y, m, d-1, 0, 0, 0, 0, loc)
	return Day{Date: prev.Format(time.DateOnly), From: prev.UTC(), To: today.UTC()}
}

// FarmDay 是 now 在农场时区 tz（farm_profile.timezone）下的前一日；时区非法返回错误（该农场跳过）。
func FarmDay(now time.Time, tz string) (Day, error) {
	if tz == "" {
		return Day{}, fmt.Errorf("quality: empty farm timezone")
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return Day{}, fmt.Errorf("quality: farm timezone %q: %w", tz, err)
	}
	return PrevDay(now, loc), nil
}
