package systemone

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
)

// 本文件实现 OpenAI Decisions 协议（POST /v1/decisions）与 SystemOne（POST /v1/systemone）
// 之间的双向转换。
//
// 背景：两套协议名字都叫"决策"，但结构差异明显——
//   - 请求：Decisions 用 input(消息数组) + questions(数组，类型 predicate/choice/score)；
//     SystemOne 用 state(文本/对象) + questions(对象 map，类型 noul/choice/score)。
//   - 响应：Decisions 的答案在 questions 数组里（按请求顺序），真假字段叫 probability；
//     SystemOne 的答案在 answers map 里，真假字段叫 noul。
//
// 实测上游（书生 intern-decision-4b）只提供 /v1/systemone，/v1/decisions 与 /v1/jev
// 均返回 404，因此 Decisions 请求必须由网关在此转换后再转发。

// FromDecisions 把 OpenAI Decisions 请求体转换为 SystemOne 请求体。
// 映射规则：
//   - input 为字符串 → state 原样；为消息数组 → 拼接各条 content 为 state 文本
//   - questions 数组 → 以 name（缺省按序 q1/q2…）为键的对象
//   - predicate → noul；choice 的 choices[] → criteria{value: description}
//   - score 的 levels[] → criteria[]（description 优先，缺省回退 label）
func FromDecisions(body []byte) ([]byte, error) {
	if !gjson.ValidBytes(body) {
		return nil, errors.New("invalid JSON body")
	}
	questions := gjson.GetBytes(body, "questions")
	if !questions.Exists() || !questions.IsArray() || len(questions.Array()) == 0 {
		return nil, errors.New("questions must be a non-empty array")
	}

	out := map[string]any{
		"model":     gjson.GetBytes(body, "model").String(),
		"state":     decisionsState(gjson.GetBytes(body, "input")),
		"questions": map[string]any{},
	}
	qs := out["questions"].(map[string]any)

	idx := 0
	for _, q := range questions.Array() {
		idx++
		name := q.Get("name").String()
		if name == "" {
			name = fmt.Sprintf("q%d", idx)
		}
		typ := q.Get("type").String()
		instructions := q.Get("instructions").String()

		switch typ {
		case "predicate":
			qs[name] = map[string]any{
				"type":         "noul",
				"instructions": instructions,
			}
		case "choice":
			criteria := map[string]any{}
			q.Get("choices").ForEach(func(_, c gjson.Result) bool {
				// value 允许字符串或布尔，统一转字符串作为选项键（SystemOne criteria 是 map）
				key := c.Get("value").String()
				if key == "" {
					key = c.Get("value").Raw
				}
				criteria[key] = c.Get("description").String()
				return true
			})
			qs[name] = map[string]any{
				"type":         "choice",
				"instructions": instructions,
				"criteria":     criteria,
			}
		case "score":
			var levels []string
			q.Get("levels").ForEach(func(_, l gjson.Result) bool {
				desc := l.Get("description").String()
				if desc == "" {
					desc = l.Get("label").String()
				}
				levels = append(levels, desc)
				return true
			})
			qs[name] = map[string]any{
				"type":         "score",
				"instructions": instructions,
				"criteria":     levels,
			}
		default:
			return nil, fmt.Errorf("unsupported question type %q", typ)
		}
	}

	return json.Marshal(out)
}

// decisionsState 把 Decisions 的 input 归一化为 SystemOne 的 state 文本。
// 字符串原样返回；消息数组按角色顺序拼接 content；其它类型原样序列化。
func decisionsState(input gjson.Result) string {
	switch {
	case !input.Exists():
		return ""
	case input.Type == gjson.String:
		return input.String()
	case input.IsArray():
		var parts []string
		input.ForEach(func(_, m gjson.Result) bool {
			content := m.Get("content")
			if content.Type == gjson.String {
				parts = append(parts, content.String())
				return true
			}
			// content 可能是分片数组 [{type:"input_text", text:"..."}]
			content.ForEach(func(_, p gjson.Result) bool {
				if txt := p.Get("text").String(); txt != "" {
					parts = append(parts, txt)
				}
				return true
			})
			return true
		})
		return strings.Join(parts, "\n")
	default:
		return input.Raw
	}
}

// ToDecisions 把 SystemOne 响应体转换为 OpenAI Decisions 响应体。
// 答案顺序与名称以原始 Decisions 请求为准（questions 是数组，不能丢顺序）。
// 上游缺某题答案时该题只保留 name/type，不伪造概率。
func ToDecisions(reqBody, respBody []byte) ([]byte, error) {
	if !gjson.ValidBytes(respBody) {
		return nil, errors.New("invalid upstream response")
	}
	answers := gjson.GetBytes(respBody, "answers")

	// 建立 名称 → 问题 的映射，用于按请求顺序输出并还原类型
	type qmeta struct {
		name string
		typ  string
	}
	var order []qmeta
	idx := 0
	gjson.GetBytes(reqBody, "questions").ForEach(func(_, q gjson.Result) bool {
		idx++
		name := q.Get("name").String()
		if name == "" {
			name = fmt.Sprintf("q%d", idx)
		}
		order = append(order, qmeta{name: name, typ: q.Get("type").String()})
		return true
	})

	outQuestions := make([]map[string]any, 0, len(order))
	for _, qm := range order {
		ans := answers.Get(qm.name)
		item := map[string]any{"name": qm.name}
		switch qm.typ {
		case "predicate":
			item["type"] = "predicate"
			if ans.Exists() {
				item["probability"] = ans.Get("noul").Float()
				item["confidence"] = ans.Get("confidence").Float()
			}
		case "choice":
			item["type"] = "choice"
			if ans.Exists() {
				item["choice"] = ans.Get("choice").String()
				item["confidence"] = ans.Get("confidence").Float()
				item["probabilities"] = json.RawMessage(ans.Get("probabilities").Raw)
			}
		case "score":
			item["type"] = "score"
			if ans.Exists() {
				item["score"] = ans.Get("score").Float()
				item["confidence"] = ans.Get("confidence").Float()
				item["probabilities"] = json.RawMessage(ans.Get("probabilities").Raw)
			}
		default:
			item["type"] = qm.typ
		}
		outQuestions = append(outQuestions, item)
	}

	out := map[string]any{
		"model":     gjson.GetBytes(respBody, "model").String(),
		"questions": outQuestions,
	}
	if u := gjson.GetBytes(respBody, "usage"); u.Exists() {
		out["usage"] = json.RawMessage(u.Raw)
	}
	return json.Marshal(out)
}
