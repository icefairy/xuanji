package proxy

import (
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/icefairy/xuanji/internal/config"
	"github.com/tidwall/gjson"
)

// TestApplyOpencodeGate_ForcesStream 验证闸门条件①：上游只接受流式。
func TestApplyOpencodeGate_ForcesStream(t *testing.T) {
	body := []byte(`{"model":"big-pickle","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	nb, changed := applyOpencodeGate(body, false)
	if !changed {
		t.Fatal("应改写请求体（stream 由 false 强制为 true）")
	}
	if !gjson.GetBytes(nb, "stream").Bool() {
		t.Errorf("stream = %v, 期望 true", gjson.GetBytes(nb, "stream"))
	}
}

// TestApplyOpencodeGate_InjectsRequiredTools 验证闸门条件②：
// tools 必须同时声明小写 read 与 shell（大小写敏感，实测 Read/Bash 会 403）。
func TestApplyOpencodeGate_InjectsRequiredTools(t *testing.T) {
	body := []byte(`{"model":"big-pickle","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	nb, changed := applyOpencodeGate(body, true)
	if !changed {
		t.Fatal("应改写请求体（补齐 tools）")
	}
	names := opencodeToolNames(nb)
	if !names["read"] {
		t.Errorf("tools 缺少 read，实际 = %v", names)
	}
	if !names["shell"] {
		t.Errorf("tools 缺少 shell，实际 = %v", names)
	}
}

// TestApplyOpencodeGate_NoToolsMeansNoToolChoice 验证：
// 客户端原本没传 tools 时，注入桩工具必须同时设 tool_choice=none，
// 否则模型可能真的去调用桩工具，客户端收到无法处理的 tool_calls。
func TestApplyOpencodeGate_NoToolsMeansNoToolChoice(t *testing.T) {
	body := []byte(`{"model":"big-pickle","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	nb, _ := applyOpencodeGate(body, true)
	if got := gjson.GetBytes(nb, "tool_choice").String(); got != "none" {
		t.Errorf("tool_choice = %q, 期望 none（无工具客户端不应被诱导调用桩工具）", got)
	}
}

// TestApplyOpencodeGate_KeepsClientTools 验证：客户端自带工具时不得改写 tool_choice，
// 否则真实 agent 的工具调用会被禁用（功能性破坏）。
func TestApplyOpencodeGate_KeepsClientTools(t *testing.T) {
	body := []byte(`{"model":"big-pickle","messages":[{"role":"user","content":"hi"}],"stream":true,` +
		`"tools":[{"type":"function","function":{"name":"Bash","parameters":{"type":"object"}}}],` +
		`"tool_choice":"auto"}`)
	nb, _ := applyOpencodeGate(body, true)
	if got := gjson.GetBytes(nb, "tool_choice").String(); got != "auto" {
		t.Errorf("tool_choice = %q, 期望保持 auto（客户端有自己的工具）", got)
	}
	names := opencodeToolNames(nb)
	for _, want := range []string{"Bash", "read", "shell"} {
		if !names[want] {
			t.Errorf("tools 缺 %s，实际 = %v", want, names)
		}
	}
}

// TestApplyOpencodeGate_AlreadySatisfied 验证幂等：
// 请求已同时含 read 与 shell 时不再重复追加。
func TestApplyOpencodeGate_AlreadySatisfied(t *testing.T) {
	body := []byte(`{"model":"big-pickle","messages":[{"role":"user","content":"hi"}],"stream":true,` +
		`"tools":[{"type":"function","function":{"name":"read","parameters":{"type":"object"}}},` +
		`{"type":"function","function":{"name":"shell","parameters":{"type":"object"}}}]}`)
	nb, _ := applyOpencodeGate(body, true)
	if n := gjson.GetBytes(nb, "tools.#").Int(); n != 2 {
		t.Errorf("tools 数量 = %d, 期望 2（不重复追加）", n)
	}
}

// TestApplyOpencodeGate_BashCountsAsShell 验证服务端接受 bash 替代 shell。
func TestApplyOpencodeGate_BashCountsAsShell(t *testing.T) {
	body := []byte(`{"model":"big-pickle","messages":[{"role":"user","content":"hi"}],"stream":true,` +
		`"tools":[{"type":"function","function":{"name":"read","parameters":{"type":"object"}}},` +
		`{"type":"function","function":{"name":"bash","parameters":{"type":"object"}}}]}`)
	nb, _ := applyOpencodeGate(body, true)
	names := opencodeToolNames(nb)
	if !names["read"] || !names["bash"] {
		t.Errorf("原工具应保留，实际 = %v", names)
	}
	if names["shell"] {
		t.Errorf("已有 bash 时不应再追加 shell，实际 = %v", names)
	}
}

// TestOpencodeSessionID_Format 验证 session id 必须匹配服务端校验格式
// ^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$（实测格式不符直接 403）。
func TestOpencodeSessionID_Format(t *testing.T) {
	re := regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`)
	req, _ := http.NewRequest("POST", "/v1/chat/completions", nil)
	for i := 0; i < 20; i++ {
		if id := opencodeSessionID(req); !re.MatchString(id) {
			t.Fatalf("session id = %q 不符合格式 %s", id, re)
		}
	}
}

// TestOpencodeSessionID_StablePerClientSession 验证同一客户端会话稳定（利于上游 prompt 缓存），
// 不同客户端会话互相隔离（避免并行任务被上游视为同一会话）。
func TestOpencodeSessionID_StablePerClientSession(t *testing.T) {
	mk := func(v string) *http.Request {
		r, _ := http.NewRequest("POST", "/v1/chat/completions", nil)
		if v != "" {
			r.Header.Set("x-conversation-id", v)
		}
		return r
	}
	a1, a2 := opencodeSessionID(mk("conv-a")), opencodeSessionID(mk("conv-a"))
	b := opencodeSessionID(mk("conv-b"))
	if a1 != a2 {
		t.Errorf("同一会话两次生成不一致: %q vs %q", a1, a2)
	}
	if a1 == b {
		t.Errorf("不同会话生成了相同 id: %q", a1)
	}
}

// TestSetOpencodeHeaders 验证身份头（闸门条件③）：UA 与 session 必需。
func TestSetOpencodeHeaders(t *testing.T) {
	req, _ := http.NewRequest("POST", "/v1/chat/completions", nil)
	setOpencodeHeaders(req, "ses_0123456789abABCDEFGHIJKLMN")
	if ua := req.Header.Get("User-Agent"); !strings.HasPrefix(ua, "opencode/") {
		t.Errorf("User-Agent = %q, 期望 opencode/ 前缀", ua)
	}
	if s := req.Header.Get("x-opencode-session"); s != "ses_0123456789abABCDEFGHIJKLMN" {
		t.Errorf("x-opencode-session = %q", s)
	}
}

// TestSetOpencodeAuth_PublicFallback 验证免费层用字面量 public 作凭证：
// 未配 key 时不发空 Bearer（上游会 401 Missing API key），而是 Bearer public。
func TestSetOpencodeAuth_PublicFallback(t *testing.T) {
	req, _ := http.NewRequest("POST", "/v1/chat/completions", nil)
	setOpencodeAuth(req, &config.Upstream{Name: "zen", Type: "opencode"})
	if got := req.Header.Get("Authorization"); got != "Bearer public" {
		t.Errorf("Authorization = %q, 期望 Bearer public", got)
	}
	req2, _ := http.NewRequest("POST", "/v1/chat/completions", nil)
	setOpencodeAuth(req2, &config.Upstream{Name: "zen", Type: "opencode", APIKey: "sk-real"})
	if got := req2.Header.Get("Authorization"); got != "Bearer sk-real" {
		t.Errorf("Authorization = %q, 期望透传用户 key", got)
	}
}

// opencodeToolNames 收集请求体里 tools[].function.name 的集合。
func opencodeToolNames(body []byte) map[string]bool {
	out := map[string]bool{}
	for _, tl := range gjson.GetBytes(body, "tools").Array() {
		if n := tl.Get("function.name").String(); n != "" {
			out[n] = true
		}
	}
	return out
}
