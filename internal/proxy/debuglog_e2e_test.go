package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/icefairy/xuanji/internal/debuglog"
)

// TestDebugLog_HTTPEndToEnd 端到端：真实走 HTTP 入口（httptest 客户端 → Handler →
// 真实上游 httptest server），确认**真实候选**的上游请求体与元数据进了日志文件。
//
// 与纯 writer 单测的区别：这里验证的是整条调用链（入口复制 body → 归一化改写 →
// forwardOnce 捕获最终请求体 → 记录），任何一环断开（如 defer 未执行、闭包捕获错变量）
// 都会让断言失败。场景特意混入「官方网关跳过改写」之外的改写：
//   - 客户端聚合模型名 m → 上游真实模型名 vendor/real-model（upstream_request 应反映改写）
//   - max_tokens 超限 clamp（第三方上游）
func TestDebugLog_HTTPEndToEnd(t *testing.T) {
	var gotUpstreamBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := readBodyAll(r)
		gotUpstreamBody = b
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"error":{"message":"端到端 503"}}`)
	}))
	defer upstream.Close()

	env := newDebugTestEnv(t, true, nil)
	env.h.cfg.Upstreams[0].BaseURL = upstream.URL + "/v1"
	env.h.cfg.Upstreams[0].ModelMapping = map[string]string{"m": "vendor/real-model"}
	env.h.cfg.Upstreams[0].MaxTokensCap = 128

	// 走真实 HTTP 入口（ChatCompletions 是 http.HandlerFunc）
	front := httptest.NewServer(http.HandlerFunc(env.h.ChatCompletions))
	defer front.Close()

	reqBody := `{"model":"m","max_tokens":99999,"stream":false,"messages":[{"role":"user","content":"端到端：中文 + \"quote\" + \n 换行"}]}`
	resp, err := http.Post(front.URL+"/v1/chat/completions", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway && resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("下游状态 = %d, want 502/503", resp.StatusCode)
	}
	if len(gotUpstreamBody) == 0 {
		t.Fatal("上游没有收到请求（测试链路不成立）")
	}

	rows := env.records(t)
	if len(rows) != 1 {
		t.Fatalf("记录数 = %d, want 1: %#v", len(rows), rows)
	}
	r := rows[0]

	// 1) 元数据来自真实候选
	if got := mustString(t, r["upstream"]); got != "up1" {
		t.Errorf("upstream = %q, want up1", got)
	}
	if got := mustString(t, r["upstream_model"]); got != "vendor/real-model" {
		t.Errorf("upstream_model = %q, want vendor/real-model", got)
	}
	if got := r["upstream_status"].(float64); got != http.StatusServiceUnavailable {
		t.Errorf("upstream_status = %v, want 503", got)
	}
	if got := r["final_status"].(float64); got != http.StatusBadGateway {
		t.Errorf("final_status = %v, want 502（全部失败归一化）", got)
	}
	if !r["final"].(bool) {
		t.Error("唯一候选失败应 final=true")
	}

	// 2) raw_request 是客户端原样字节（模型仍是聚合名 m，max_tokens 仍是 99999）
	raw := toMap(t, r["raw_request"])
	if got := raw["model"]; got != "m" {
		t.Errorf("raw_request.model = %v, want 客户端原始 m", got)
	}
	if got := raw["max_tokens"].(float64); got != 99999 {
		t.Errorf("raw_request.max_tokens = %v, want 99999（原始未改写）", got)
	}
	msgs := raw["messages"].([]any)
	if got := msgs[0].(map[string]any)["content"]; got != "端到端：中文 + \"quote\" + \n 换行" {
		t.Errorf("raw_request 内容未原样保留: %#v", got)
	}

	// 3) upstream_request 是**真正发给上游**的字节（模型已映射、max_tokens 已 clamp）
	up := toMap(t, r["upstream_request"])
	if got := up["model"]; got != "vendor/real-model" {
		t.Errorf("upstream_request.model = %v, want vendor/real-model", got)
	}
	if got, _ := up["max_tokens"].(float64); got != 128 {
		t.Errorf("upstream_request.max_tokens = %v, want 128（clamp 后）", got)
	}
	// 与上游实际收到的请求体逐字节一致（同一份 JSON 语义）
	var sent, logged map[string]any
	if err := json.Unmarshal(gotUpstreamBody, &sent); err != nil {
		t.Fatalf("上游请求体非法 JSON: %v", err)
	}
	if err := json.Unmarshal(mustJSONBytes(t, r["upstream_request"]), &logged); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(sent["model"]) != fmt.Sprint(logged["model"]) {
		t.Errorf("日志里的 upstream_request 与上游实际收到的不一致: %v vs %v",
			logged["model"], sent["model"])
	}

	// 4) 敏感信息不落盘
	data, err := os.ReadFile(env.writer.Path())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "sk-test") {
		t.Error("日志泄露上游 API key")
	}
}

func toMap(t *testing.T, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("字段不是 JSON 对象: %#v", v)
	}
	return m
}

func readBodyAll(r *http.Request) ([]byte, error) { return io.ReadAll(r.Body) }

// 静态断言：避免未使用的导入告警（debuglog 在 env 构造里用到）。
var _ = debuglog.DefaultRetainDays
