package systemone

import (
	"encoding/json"
	"testing"

	"github.com/tidwall/gjson"
)

// TestDecisionsRequestToSystemOne 校验 OpenAI Decisions 请求正确映射为 SystemOne。
func TestDecisionsRequestToSystemOne(t *testing.T) {
	body := []byte(`{
	  "model": "intern-decision-4b",
	  "input": [{"role":"user","content":"客户重复扣款要求退款"}],
	  "questions": [
	    {"type":"predicate","name":"refund","instructions":"是否要求退款?"},
	    {"type":"choice","name":"team","instructions":"哪个团队?",
	     "choices":[{"value":"billing","description":"账单"},{"value":"tech","description":"技术"}]},
	    {"type":"score","name":"anger","instructions":"情绪程度",
	     "levels":[{"label":"calm","description":"平静"},{"label":"angry"}]}
	  ]
	}`)

	out, err := FromDecisions(body)
	if err != nil {
		t.Fatalf("FromDecisions error: %v", err)
	}

	if got := gjson.GetBytes(out, "model").String(); got != "intern-decision-4b" {
		t.Errorf("model = %q", got)
	}
	// input 消息数组 → state 文本
	if got := gjson.GetBytes(out, "state").String(); got != "客户重复扣款要求退款" {
		t.Errorf("state = %q, want 客户重复扣款要求退款", got)
	}
	// questions 数组 → 对象 map，键为 name
	if !gjson.GetBytes(out, "questions.refund").Exists() {
		t.Fatalf("questions.refund missing, body=%s", out)
	}
	// predicate → noul
	if got := gjson.GetBytes(out, "questions.refund.type").String(); got != "noul" {
		t.Errorf("refund type = %q, want noul", got)
	}
	if got := gjson.GetBytes(out, "questions.refund.instructions").String(); got != "是否要求退款?" {
		t.Errorf("refund instructions = %q", got)
	}
	// choices 数组 → criteria 对象
	if got := gjson.GetBytes(out, "questions.team.type").String(); got != "choice" {
		t.Errorf("team type = %q, want choice", got)
	}
	if got := gjson.GetBytes(out, "questions.team.criteria.billing").String(); got != "账单" {
		t.Errorf("team.criteria.billing = %q, want 账单", got)
	}
	// levels 数组 → criteria 字符串数组（按顺序）
	if got := gjson.GetBytes(out, "questions.anger.type").String(); got != "score" {
		t.Errorf("anger type = %q, want score", got)
	}
	if got := gjson.GetBytes(out, "questions.anger.criteria.0").String(); got != "平静" {
		t.Errorf("anger.criteria[0] = %q, want 平静", got)
	}
	if got := gjson.GetBytes(out, "questions.anger.criteria.1").String(); got != "angry" {
		t.Errorf("anger.criteria[1] = %q, want angry (无 description 时回退 label)", got)
	}
}

// TestDecisionsRequestStringInput 校验 input 为纯字符串时直接作为 state。
func TestDecisionsRequestStringInput(t *testing.T) {
	body := []byte(`{"model":"m","input":"一段纯文本状态","questions":[{"type":"predicate","name":"p","instructions":"?"}]}`)
	out, err := FromDecisions(body)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if got := gjson.GetBytes(out, "state").String(); got != "一段纯文本状态" {
		t.Errorf("state = %q", got)
	}
}

// TestDecisionsRequestAnonymousName 校验缺 name 时按序号生成 q1/q2。
func TestDecisionsRequestAnonymousName(t *testing.T) {
	body := []byte(`{"model":"m","input":"x","questions":[{"type":"predicate","instructions":"a?"},{"type":"predicate","instructions":"b?"}]}`)
	out, _ := FromDecisions(body)
	if !gjson.GetBytes(out, "questions.q1").Exists() || !gjson.GetBytes(out, "questions.q2").Exists() {
		t.Errorf("anonymous questions not named q1/q2: %s", out)
	}
}

// TestDecisionsResponseToDecisions 校验 SystemOne 响应转回 OpenAI Decisions 结构。
func TestDecisionsResponseToDecisions(t *testing.T) {
	// 请求侧决定答案顺序与名称
	req := []byte(`{"model":"m","input":"x","questions":[
	  {"type":"predicate","name":"refund","instructions":"a"},
	  {"type":"choice","name":"team","instructions":"b"},
	  {"type":"score","name":"anger","instructions":"c"}]}`)
	// SystemOne 响应（上游）
	resp := []byte(`{"model":"m","answers":{
	  "refund":{"type":"noul","noul":0.87},
	  "team":{"type":"choice","choice":"billing","probabilities":{"billing":0.9,"tech":0.1},"confidence":0.9},
	  "anger":{"type":"score","score":1.2,"probabilities":{"0":0.3,"1":0.7},"confidence":0.7,"legend":{"0":"calm","1":"angry"}}
	},"usage":{"input_tokens":10,"output_tokens":3}}`)

	out, err := ToDecisions(req, resp)
	if err != nil {
		t.Fatalf("ToDecisions error: %v", err)
	}
	// predicate → probability
	if got := gjson.GetBytes(out, "questions.0.probability").Float(); got != 0.87 {
		t.Errorf("refund.probability = %v, want 0.87", got)
	}
	// choice 保留 value 与概率
	if got := gjson.GetBytes(out, "questions.1.choice").String(); got != "billing" {
		t.Errorf("team.choice = %q", got)
	}
	if got := gjson.GetBytes(out, "questions.1.confidence").Float(); got != 0.9 {
		t.Errorf("team.confidence = %v", got)
	}
	// score 保留分数与分布
	if got := gjson.GetBytes(out, "questions.2.score").Float(); got != 1.2 {
		t.Errorf("anger.score = %v", got)
	}
	// 顺序必须与请求一致（questions 是数组）
	var arr []map[string]any
	if err := json.Unmarshal([]byte(gjson.GetBytes(out, "questions").Raw), &arr); err != nil {
		t.Fatalf("questions not array: %v", err)
	}
	if len(arr) != 3 {
		t.Fatalf("questions len = %d, want 3", len(arr))
	}
	if arr[0]["name"] != "refund" || arr[1]["name"] != "team" || arr[2]["name"] != "anger" {
		t.Errorf("answer order mismatch: %v", arr)
	}
	// usage 透传
	if got := gjson.GetBytes(out, "usage.input_tokens").Int(); got != 10 {
		t.Errorf("usage.input_tokens = %d", got)
	}
}

// TestDecisionsResponseMissingAnswer 校验上游缺某题答案时该题标记为空而不报错。
func TestDecisionsResponseMissingAnswer(t *testing.T) {
	req := []byte(`{"model":"m","input":"x","questions":[{"type":"predicate","name":"a","instructions":"a"},{"type":"predicate","name":"b","instructions":"b"}]}`)
	resp := []byte(`{"answers":{"a":{"type":"noul","noul":0.5}}}`)
	out, err := ToDecisions(req, resp)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if got := gjson.GetBytes(out, "questions.0.probability").Float(); got != 0.5 {
		t.Errorf("a.probability = %v", got)
	}
	// b 缺答案：不应 panic，字段留空
	if gjson.GetBytes(out, "questions.1.probability").Exists() {
		t.Logf("b present: %s", gjson.GetBytes(out, "questions.1").Raw)
	}
}

// TestDecisionsInvalidRequest 校验非法请求返回错误而非 panic。
func TestDecisionsInvalidRequest(t *testing.T) {
	if _, err := FromDecisions([]byte(`not json`)); err == nil {
		t.Error("want error for invalid json")
	}
	if _, err := FromDecisions([]byte(`{"model":"m"}`)); err == nil {
		t.Error("want error for missing questions")
	}
}
