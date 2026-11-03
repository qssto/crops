package service

import (
	"time"

	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

// P1 L7-B 看板运营指标（口径见 docs/metrics.md「P1-L7-B 运营指标」）：
//   - dispatch_success_rate：REAL 下发尝试中设备平台已受理的比例（按 sent_at 归日）；
//   - job_success_rate：REAL 作业单进入终态时 DONE / PARTIALLY_DONE 的比例（分母 DONE + PARTIALLY_DONE + FAILED，按迁移时刻归日）；
//   - recheck_returned：下发前复检退回次数（reason RECHECK_FAILED 的迁移，含 DRY_RUN）；
//   - expired：作业单转 EXPIRED 次数（含 DRY_RUN）；
//   - approval_backlog（MetricsReport 顶层）：查询时刻 OPEN 的 ORDER 票数，不随时间窗。

// opsBucket 是一天的运营原始量（嵌入 bucket）。
type opsBucket struct {
	attempts, accepted       int
	jobOK, jobEnded          int
	recheckReturned, expired int
}

// OpsMetrics 是一天的运营指标（嵌入 DayMetrics，JSON 字段平铺）。
type OpsMetrics struct {
	DispatchSuccess Ratio `json:"dispatch_success_rate"`
	JobSuccess      Ratio `json:"job_success_rate"`
	RecheckReturned int   `json:"recheck_returned"`
	Expired         int   `json:"expired"`
}

// addOpsFacts 把下发尝试与作业单关键迁移归桶。
func addOpsFacts(f repository.MetricFacts, add func(time.Time, func(*bucket))) {
	for _, a := range f.Attempts {
		accepted := a.Accepted
		add(a.SentAt, func(b *bucket) {
			b.attempts++
			if accepted {
				b.accepted++
			}
		})
	}
	for _, tr := range f.Transitions {
		isReal := tr.DispatchMode == string(model.DispatchReal)
		add(tr.CreatedAt, func(b *bucket) {
			switch tr.To {
			case model.OrderDone, model.OrderPartiallyDone:
				if isReal {
					b.jobOK++
					b.jobEnded++
				}
			case model.OrderFailed:
				if isReal {
					b.jobEnded++
				}
			case model.OrderExpired:
				b.expired++
			}
			if tr.ReasonCode == model.ReasonRecheckFailed {
				b.recheckReturned++
			}
		})
	}
}

func (o opsBucket) metrics() OpsMetrics {
	return OpsMetrics{DispatchSuccess: ratio(o.accepted, o.attempts), JobSuccess: ratio(o.jobOK, o.jobEnded),
		RecheckReturned: o.recheckReturned, Expired: o.expired}
}
