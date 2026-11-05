package compaction

import (
	"encoding/json"
	"fmt"
	"unicode/utf8"

	"agri-steward/internal/model"
)

// Block 是附加到模型输入的会话块（input.session）：白名单事实 + 更早轮次摘要 + 摘要之后的原文轮次。
type Block struct {
	CompactionVersion int                      `json:"compaction_version"`
	Facts             model.CompactionFacts    `json:"facts"`
	Summary           *model.CompactionSummary `json:"summary,omitempty"`
	Turns             []Turn                   `json:"turns,omitempty"`
}

// Turn 是一轮原文：StepID 为 model.TurnStepID(run_id)。
type Turn struct {
	StepID   string `json:"step_id"`
	Question string `json:"question"`
	Answer   string `json:"answer,omitempty"`
}

// turnsOf 把会话原料的轮次转成原文轮次（顺序不变）。
func turnsOf(src []model.SessionTurn) []Turn {
	out := make([]Turn, 0, len(src))
	for _, t := range src {
		out = append(out, Turn{StepID: model.TurnStepID(t.RunID), Question: t.Question, Answer: t.Answer})
	}
	return out
}

// encode 编码会话块；encoding/json 对 struct 按字段顺序、map 按键排序输出，同一输入字节稳定。
func encode(b Block) ([]byte, error) {
	out, err := json.Marshal(b)
	if err != nil {
		return nil, fmt.Errorf("compaction: encode block: %w", err)
	}
	return out, nil
}

// EstimateTokens 是确定性的 token 估算：非 ASCII 字符各计 1，ASCII 字符每 4 个计 1（向上取整）。
// 只用于触发判断与 history 记录，不是计费口径（待真实模型按 tokenizer 校准）。
func EstimateTokens(s string) int {
	ascii, other := 0, 0
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r < utf8.RuneSelf {
			ascii++
		} else {
			other++
		}
		i += size
	}
	return other + (ascii+3)/4
}
