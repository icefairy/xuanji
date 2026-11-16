package admin

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/icefairy/xuanji/internal/config"
	"github.com/icefairy/xuanji/internal/proxy"
	"github.com/icefairy/xuanji/internal/store"
)

// modelsResponse 是 /v1/models 的响应结构（测试用）。
type modelsResponse struct {
	Object string `json:"object"`
	Data   []struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	} `json:"data"`
}

// getModels 请求 /v1/models 并返回解码结果。
func getModels(t *testing.T, h *Handler) modelsResponse {
	t.Helper()
	rr := httptest.NewRecorder()
	h.Models(rr, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	var out modelsResponse
	decodeBody(t, rr, &out)
	return out
}

// modelIDs 抽取返回的模型 id 列表。
func modelIDs(resp modelsResponse) []string {
	ids := make([]string, 0, len(resp.Data))
	for _, m := range resp.Data {
		ids = append(ids, m.ID)
	}
	return ids
}

// hasModel 判断返回结果里是否包含指定模型。
func hasModel(resp modelsResponse, id string) bool {
	for _, m := range resp.Data {
		if m.ID == id {
			return true
		}
	}
	return false
}

func TestModels_AllUpstreamsDisabled(t *testing.T) {
	cfg := testConfig()
	for i := range cfg.Upstreams {
		cfg.Upstreams[i].Enabled = false
	}
	h, hc := newTestHandler(t, cfg)
	defer hc.Close()

	out := getModels(t, h)
	if len(out.Data) != 0 {
		t.Fatalf("models = %v, want empty when all upstreams disabled", modelIDs(out))
	}
	if out.Object != "list" {
		t.Errorf("object = %q, want list", out.Object)
	}
}

func TestModels_FilterByUpstreamValidity(t *testing.T) {
	cfg := &config.Config{
		Upstreams: []config.Upstream{
			// 启用且支持 → 模型保留
			{Name: "live", Enabled: true, Models: []string{"alpha"}},
			// 唯一上游被禁用 → 模型过滤
			{Name: "off", Enabled: false, Models: []string{"beta"}},
			// 唯一上游整上游欠费 → 模型过滤
			{Name: "broke", Enabled: true, Arrears: true, Models: []string{"gamma"}},
			// 启用但不声明支持该模型 → 模型过滤
			{Name: "other", Enabled: true, Models: []string{"unrelated"}},
			// 未声明模型列表（视为全支持）→ 模型保留
			{Name: "open", Enabled: true},
			// 混合：一个禁用 + 一个启用 → 模型保留
			{Name: "mixed-off", Enabled: false, Models: []string{"delta"}},
			{Name: "mixed-on", Enabled: true, Models: []string{"delta"}},
			// 名字在规则里但配置里不存在 → 视为无效，模型过滤
		},
		Routing: config.Routing{Rules: []config.Rule{
			{Model: "alpha", Upstreams: []string{"live"}},
			{Model: "beta", Upstreams: []string{"off"}},
			{Model: "gamma", Upstreams: []string{"broke"}},
			{Model: "epsilon", Upstreams: []string{"other"}},
			{Model: "zeta", Upstreams: []string{"open"}},
			{Model: "delta", Upstreams: []string{"mixed-off", "mixed-on"}},
			{Model: "ghost", Upstreams: []string{"nonexistent"}},
		}},
	}
	h, hc := newTestHandler(t, cfg)
	defer hc.Close()

	out := getModels(t, h)
	want := map[string]bool{"alpha": true, "zeta": true, "delta": true}
	got := modelIDs(out)
	if len(got) != len(want) {
		t.Fatalf("models = %v, want only %v", got, want)
	}
	for _, id := range got {
		if !want[id] {
			t.Errorf("unexpected model %q in %v", id, got)
		}
	}
	for _, m := range out.Data {
		if m.Object != "model" || m.OwnedBy != "xuanji" || m.Created == 0 {
			t.Errorf("model %q has unexpected shape: %+v", m.ID, m)
		}
	}
}

func TestModels_DuplicateRuleModelListedOnce(t *testing.T) {
	cfg := &config.Config{
		Upstreams: []config.Upstream{
			{Name: "off", Enabled: false, Models: []string{"dup"}},
			{Name: "on", Enabled: true, Models: []string{"dup"}},
		},
		Routing: config.Routing{Rules: []config.Rule{
			{Model: "dup", Upstreams: []string{"off"}},
			{Model: "dup", Upstreams: []string{"on"}},
			{Model: "dup", Upstreams: []string{"on"}},
		}},
	}
	h, hc := newTestHandler(t, cfg)
	defer hc.Close()

	out := getModels(t, h)
	if len(out.Data) != 1 || out.Data[0].ID != "dup" {
		t.Fatalf("models = %v, want exactly one \"dup\"", modelIDs(out))
	}
}

func TestModels_PerModelBillingArrear(t *testing.T) {
	cfg := &config.Config{
		Upstreams: []config.Upstream{
			{Name: "bailian", Enabled: true, PerModelBilling: true, Models: []string{"a-model", "b-model", "c-model"}},
		},
		Routing: config.Routing{Rules: []config.Rule{
			{Model: "a-model", Upstreams: []string{"bailian"}},
			{Model: "b-model", Upstreams: []string{"bailian"}},
			{Model: "c-model", Upstreams: []string{"bailian"}},
		}},
	}
	h, hc := newTestHandler(t, cfg)
	defer hc.Close()

	// px 为 nil：无法查模型级欠费，退化处理为未欠费，三个模型都应返回。
	out := getModels(t, h)
	if len(out.Data) != 3 {
		t.Fatalf("models = %v with nil px, want all 3", modelIDs(out))
	}

	// 注入 px 并把 a-model 标记为欠费 → 只过滤 a-model。
	h.px = proxy.New(cfg, nil, hc)
	h.px.LoadModelArrears([]store.ModelArrearRow{{Upstream: "bailian", Model: "a-model"}})
	out = getModels(t, h)
	if hasModel(out, "a-model") {
		t.Errorf("a-model should be filtered after model-level arrear, got %v", modelIDs(out))
	}
	if !hasModel(out, "b-model") || !hasModel(out, "c-model") {
		t.Errorf("b-model/c-model should remain, got %v", modelIDs(out))
	}
}

// 路由规则名带通配时，不能按字面去比对上游 models 列表：
// 上游声明的是具体模型名，规则名是通配前缀，必须按通配语义判定，
// 否则整组模型会从列表里消失（真实回归：mimo-v2.5-tts*）。
func TestModels_WildcardRuleMatchesConcreteUpstreamModels(t *testing.T) {
	cfg := &config.Config{
		Upstreams: []config.Upstream{
			{Name: "mimo", Enabled: true, Models: []string{
				"mimo-v2.5-tts", "mimo-v2.5-tts-voiceclone", "mimo-v2.5-tts-voicedesign",
			}},
			{Name: "dead", Enabled: false, Models: []string{"mimo-v2.5-tts"}},
			{Name: "other", Enabled: true, Models: []string{"unrelated-model"}},
		},
		Routing: config.Routing{Rules: []config.Rule{
			{Model: "mimo-v2.5-tts*", Upstreams: []string{"mimo", "dead"}},
			// 通配但没有任何上游提供匹配模型 → 应被过滤
			{Model: "nope-*", Upstreams: []string{"other"}},
		}},
	}
	h, hc := newTestHandler(t, cfg)
	defer hc.Close()

	out := getModels(t, h)
	if !hasModel(out, "mimo-v2.5-tts*") {
		t.Errorf("wildcard rule should stay when an upstream provides a matching model, got %v", modelIDs(out))
	}
	if hasModel(out, "nope-*") {
		t.Errorf("wildcard rule with no matching upstream should be filtered, got %v", modelIDs(out))
	}
}
