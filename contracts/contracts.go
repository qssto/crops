// Package contracts 内嵌 L1 冻结的契约 JSON Schema（本目录 *.schema.json 即真源），供运行期校验
// （guardrail.OutputValidator）使用，不依赖进程工作目录。
package contracts

import "embed"

// 契约文件名。
const (
	AdviceCardFile = "advice_card.schema.json"
	CitationFile   = "citation.schema.json"
	// P2 L8：采纳 / 差评请求。
	AdviceFeedbackRequestFile = "advice_feedback_request.schema.json"
	// P2-L2b：专家评审提交体与任务视图。
	ReviewSubmitRequestFile = "review_submit_request.schema.json"
	ReviewTaskViewFile      = "review_task_view.schema.json"
)

// FS 是本目录全部契约 schema。
//
//go:embed *.schema.json
var FS embed.FS
