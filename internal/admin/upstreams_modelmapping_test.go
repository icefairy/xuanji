package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/icefairy/xuanji/internal/health"
	"github.com/icefairy/xuanji/internal/store"
)

// TestUpdateUpstream_RejectsInvalidModelMappingJSON 回归测试（2026-09-14 实测事故）：
// admin 的编辑表单把 model_mapping 当自由文本框（placeholder='{"简单名":"上游真实模型名"}'），
// UpdateUpstream 把它当**不透明字符串**直接落库，从不做 JSON 语法校验。
// 用户手写时漏一个逗号（如 `...,"chat":"X""asr":"Y"}`）就把上游的映射表写坏；而
// config.LoadFromDB 用 json.Unmarshal 解析，语法错误时 **整个 map 为空**
// （实测 Go 行为：err != nil 且 len(m)==0，不做部分填充）→ 该上游**全部**模型映射丢失，
// 网关随后把客户端短名（bge-m3 / rerankv2m3 / asr）原样发给上游，
// 上游回 400 "Model does not exist"（当天 87 例，且被误当成上游故障拉黑）。
//
// 修复：UpdateUpstream 在写库前校验 model_mapping 必须是合法 JSON 对象，
// 非法时返回 400 且**不写库**（保住 DB 中已有的好配置）。
func TestUpdateUpstream_RejectsInvalidModelMappingJSON(t *testing.T) {
	s, err := store.Open(t.TempDir() + "/xuanji.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()

	cfg := testConfig()
	hc := health.New(cfg)
	defer hc.Close()
	h := New(cfg, hc)
	h.SetStore(s)

	good := `{"bge-m3":"BAAI/bge-m3","asr":"FunAudioLLM/SenseVoiceSmall"}`
	if err := s.CreateUpstream(&store.UpstreamRow{
		Name: "lxr", Type: "openai", BaseURL: "https://api.siliconflow.cn/v1",
		APIKey: "sk-x", Tier: "free", Priority: 5, Weight: 100,
		Models: `["bge-m3","asr"]`, ModelMapping: good,
	}); err != nil {
		t.Fatalf("create upstream: %v", err)
	}

	// 缺逗号的非法 JSON（真实事故形态：`"chat":"X""asr":"Y"`）
	broken := `{"bge-m3":"BAAI/bge-m3","chat":"THUDM/GLM-4-9B-0414""asr":"FunAudioLLM/SenseVoiceSmall"}`

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/admin/upstreams/lxr",
		strings.NewReader(`{"model_mapping":`+jsonString(broken)+`}`))
	req.SetPathValue("name", "lxr")
	h.UpdateUpstream(rr, req)

	if rr.Code == http.StatusOK {
		t.Errorf("status = %d, want 4xx（非法 JSON 的 model_mapping 必须被拒绝）; body=%s",
			rr.Code, rr.Body.String())
	}
	// 关键：DB 中已有的合法配置不得被写坏
	got, gerr := s.GetUpstream("lxr")
	if gerr != nil {
		t.Fatalf("get upstream: %v", gerr)
	}
	if got.ModelMapping != good {
		t.Errorf("model_mapping 被写坏：got %q, want %q（拒绝时必须原样保留旧值）",
			got.ModelMapping, good)
	}
}

// TestUpdateUpstream_AcceptsValidModelMapping 反向保护：合法 JSON 对象必须仍能保存，
// 修复不能变成"禁止编辑 model_mapping"。
func TestUpdateUpstream_AcceptsValidModelMapping(t *testing.T) {
	for _, tc := range []struct{ name, mm string }{
		{"普通对象", `{"a":"b"}`},
		{"空对象", `{}`},
		{"空串（允许清空）", ``},
		{"含空白的对象", "  {\n  \"a\" : \"b\"\n}  "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := store.Open(t.TempDir() + "/xuanji.db")
			if err != nil {
				t.Fatalf("open store: %v", err)
			}
			defer s.Close()

			cfg := testConfig()
			hc := health.New(cfg)
			defer hc.Close()
			h := New(cfg, hc)
			h.SetStore(s)

			if err := s.CreateUpstream(&store.UpstreamRow{
				Name: "u1", Type: "openai", BaseURL: "http://x", APIKey: "k",
				Tier: "free", Priority: 1, Weight: 1, Models: `["a"]`,
			}); err != nil {
				t.Fatalf("create upstream: %v", err)
			}

			rr := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPut, "/admin/upstreams/u1",
				strings.NewReader(`{"model_mapping":`+jsonString(tc.mm)+`}`))
			req.SetPathValue("name", "u1")
			h.UpdateUpstream(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
			}
			got, _ := s.GetUpstream("u1")
			if got.ModelMapping != tc.mm {
				t.Errorf("model_mapping = %q, want %q", got.ModelMapping, tc.mm)
			}
		})
	}
}

// TestCreateUpstream_RejectsInvalidModelMappingJSON POST 接口同样必须拒绝非法 JSON：
// 新建上游时表单就是那块自由文本框，非法值不能落库（否则创建即坏）。
func TestCreateUpstream_RejectsInvalidModelMappingJSON(t *testing.T) {
	s, err := store.Open(t.TempDir() + "/xuanji.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()

	cfg := testConfig()
	hc := health.New(cfg)
	defer hc.Close()
	h := New(cfg, hc)
	h.SetStore(s)

	broken := `{"bge-m3":"BAAI/bge-m3""asr":"FunAudioLLM/SenseVoiceSmall"}`
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/upstreams",
		strings.NewReader(`{"name":"newup","type":"openai","base_url":"http://x","api_key":"k","model_mapping":`+jsonString(broken)+`}`))
	h.CreateUpstream(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
	if _, gerr := s.GetUpstream("newup"); gerr == nil {
		t.Error("非法 model_mapping 的上游不应被创建")
	}
}

// jsonString 把字符串编码为 JSON 字面量（用于拼 PUT body）。
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
