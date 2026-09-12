package admin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/icefairy/xuanji/internal/proxy"
	"github.com/icefairy/xuanji/internal/store"
)

// Upstreams 返回上游列表。优先从数据库读取（h.store 非 nil），否则从配置读取。
func (h *Handler) Upstreams(w http.ResponseWriter, _ *http.Request) {
	if h.store != nil {
		rows, err := h.store.ListUpstreams()
		if err == nil {
			resp := make([]upstreamResponse, 0, len(rows))
			for _, u := range rows {
				models := parseStringSlice(u.Models)
				resp = append(resp, upstreamResponse{
					Name:            u.Name,
					Type:            u.Type,
					Kind:            u.Kind,
					BaseURL:         u.BaseURL,
					APIKey:          u.APIKey,
					Tier:            u.Tier,
					Priority:        u.Priority,
					Weight:          u.Weight,
					Enabled:         u.Enabled == 1,
					BillingExempt:   u.BillingExempt == 1,
					Arrears:         u.Arrears == 1,
					PerModelBilling: h.configBool("upstream." + u.Name + ".per_model_billing"),
					FastFail:        h.fastFailState(u.Name),
					State:           string(h.hc.Status(u.Name)),
					LatencyMS:       h.hc.Latency(u.Name).Milliseconds(),
					Models:          models,
					ModelCount:      len(models),
					ModelMapping:    u.ModelMapping,
					RequestOverride: u.RequestOverride,
					Timeout:         u.Timeout,
				})
			}
			writeJSON(w, resp)
			return
		}
	}
	// fallback: 从静态配置读取
	resp := make([]upstreamResponse, 0, len(h.cfg.Upstreams))
	for _, up := range h.cfg.Upstreams {
		mm, _ := json.Marshal(up.ModelMapping)
		resp = append(resp, upstreamResponse{
			Name:            up.Name,
			Type:            up.Type,
			Kind:            up.Kind,
			BaseURL:         up.BaseURL,
			APIKey:          up.APIKey,
			Tier:            up.Tier,
			Priority:        up.Priority,
			Weight:          up.Weight,
			Enabled:         up.Enabled,
			BillingExempt:   false,
			Arrears:         false,
			PerModelBilling: up.PerModelBilling,
			FastFail:        h.fastFailState(up.Name),
			State:           string(h.hc.Status(up.Name)),
			LatencyMS:       h.hc.Latency(up.Name).Milliseconds(),
			Models:          up.Models,
			ModelCount:      len(up.Models),
			ModelMapping:    string(mm),
		})
	}
	writeJSON(w, resp)
}

// upstreamEnabled 返回上游是否启用；未找到时返回 false（视为异常）。
func (h *Handler) upstreamEnabled(name string) bool {
	if h.cfg == nil {
		return false
	}
	for i := range h.cfg.Upstreams {
		if h.cfg.Upstreams[i].Name == name {
			return h.cfg.Upstreams[i].Enabled
		}
	}
	return false
}

// upstreamTierWeight 返回上游的 tier 权重（0=free, 1=subscription, 2=payg）；未找到时返回 3（最低优先级）。
func (h *Handler) upstreamTierWeight(name string) int {
	if h.cfg == nil {
		return 3
	}
	for i := range h.cfg.Upstreams {
		if h.cfg.Upstreams[i].Name == name {
			return h.cfg.Upstreams[i].TierWeight()
		}
	}
	return 3
}

// upstreamWeight 返回上游的 weight；未找到时返回 0。
func (h *Handler) upstreamWeight(name string) int {
	if h.cfg == nil {
		return 0
	}
	for i := range h.cfg.Upstreams {
		if h.cfg.Upstreams[i].Name == name {
			return h.cfg.Upstreams[i].Weight
		}
	}
	return 0
}

// ruleFastFail 返回 rule 每个上游在指定模型下的黑名单状态（nil 表示未启用 fastfail）。
func (h *Handler) ruleFastFail(model string, upstreams []string) []bool {
	if h.ff == nil {
		return nil
	}
	out := make([]bool, len(upstreams))
	for i, name := range upstreams {
		out[i] = h.ff.IsBlacklisted(name, model)
	}
	return out
}

// ruleEnabled 返回 rule 每个上游的启用状态（nil 表示配置缺失，前端按未知处理）。
func (h *Handler) ruleEnabled(upstreams []string) []bool {
	if h.cfg == nil {
		return nil
	}
	out := make([]bool, len(upstreams))
	for i, name := range upstreams {
		out[i] = h.upstreamEnabled(name)
	}
	return out
}

// CreateUpstream 添加上游（POST /admin/upstreams）。
func (h *Handler) CreateUpstream(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, map[string]string{"error": "invalid request: " + err.Error()})
		return
	}
	var req store.UpstreamRow
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, map[string]string{"error": "invalid request: " + err.Error()})
		return
	}
	// enabled 显式传入时按传入值，未传默认启用（CreateUpstream 内部处理）
	var raw map[string]json.RawMessage
	if json.Unmarshal(body, &raw) == nil {
		if v, ok := raw["enabled"]; ok {
			var e int
			if json.Unmarshal(v, &e) == nil {
				req.EnabledPtr = &e
			}
		}
		if v, ok := raw["billing_exempt"]; ok {
			var b int
			if json.Unmarshal(v, &b) == nil {
				req.BillingExemptPtr = &b
			}
		}
	}
	if err := h.store.CreateUpstream(&req); err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	if h.reload != nil {
		h.reload()
	}
	writeJSON(w, req)
}

// UpdateUpstream 更新上游（PUT /admin/upstreams/{name}）。
func (h *Handler) UpdateUpstream(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, map[string]string{"error": "invalid request: " + err.Error()})
		return
	}
	var req store.UpstreamRow
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, map[string]string{"error": "invalid request: " + err.Error()})
		return
	}
	// 名称保护：body 未带 name 时用路径里的 name，避免被空串清空
	if strings.TrimSpace(req.Name) == "" {
		req.Name = name
	}
	// enabled 字段显式传入（含 false/0）时才允许改启停状态；
	// 未传时保持原值，避免编辑表单漏传导致上游被误禁用。
	var raw map[string]json.RawMessage
	if json.Unmarshal(body, &raw) == nil {
		if v, ok := raw["enabled"]; ok {
			var e int
			if json.Unmarshal(v, &e) == nil {
				req.EnabledPtr = &e
			}
		}
		// billing_exempt 同样：显式传入（含 false/0）才允许改，未传保持原值。
		if v, ok := raw["billing_exempt"]; ok {
			var b int
			if json.Unmarshal(v, &b) == nil {
				req.BillingExemptPtr = &b
			}
		}
		// timeout 同样：显式传入（含 0=跟随全局）才允许改，未传（旧客户端）保持原值。
		if v, ok := raw["timeout"]; ok {
			var t int
			if json.Unmarshal(v, &t) == nil {
				req.TimeoutPtr = &t
				req.Timeout = t
			}
		}
		// kind 同样：显式传入（含空串=回默认 chat）才允许改，未传（旧客户端）保持原值。
		if v, ok := raw["kind"]; ok {
			var k string
			if json.Unmarshal(v, &k) == nil {
				req.KindPtr = &k
				req.Kind = k
			}
		}
		// 部分更新语义：store.UpdateUpstream 是全列覆盖 UPDATE，raw 未出现的字段
		// 必须以 DB 旧行回填，否则稀疏 PUT（如只传 per_model_billing 开关）会把
		// base_url/api_key/models/model_mapping 等核心配置清成零值
		// （2026-08-27 实测清空硅基流动上游的事故）。显式传入新值仍可正常修改。
		if h.store != nil {
			if old, gerr := h.store.GetUpstream(name); gerr == nil {
				has := func(k string) bool { _, ok := raw[k]; return ok }
				if !has("type") {
					req.Type = old.Type
				}
				if !has("kind") {
					req.Kind = old.Kind
				}
				if !has("base_url") {
					req.BaseURL = old.BaseURL
				}
				if !has("api_key") {
					req.APIKey = old.APIKey
				}
				if !has("tier") {
					req.Tier = old.Tier
				}
				if !has("priority") {
					req.Priority = old.Priority
				}
				if !has("weight") {
					req.Weight = old.Weight
				}
				if !has("models") {
					req.Models = old.Models
				}
				if !has("model_mapping") {
					req.ModelMapping = old.ModelMapping
				}
				if !has("request_override") {
					req.RequestOverride = old.RequestOverride
				}
				if !has("enabled") {
					req.EnabledPtr = &old.Enabled
				}
				if !has("billing_exempt") {
					req.BillingExemptPtr = &old.BillingExempt
				}
				if !has("timeout") {
					req.TimeoutPtr = &old.Timeout
					req.Timeout = old.Timeout
				}
			}
		}
		// per_model_billing 模型独立计费开关：存 config 表键 upstream.<name>.per_model_billing
		// （upstreams 表无此列）。显式传入才改；兼容 bool 与 0/1 数字（Vue 表单传 1/0）；
		// 未传保持原值。
		if v, ok := raw["per_model_billing"]; ok && h.store != nil {
			var bb bool
			if err := json.Unmarshal(v, &bb); err != nil {
				var n int
				if err2 := json.Unmarshal(v, &n); err2 == nil {
					bb = n != 0
				}
			}
			val := "0"
			if bb {
				val = "1"
			}
			_ = h.store.SetConfig("upstream."+name+".per_model_billing", val)
		}
	}
	if err := h.store.UpdateUpstream(name, &req); err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	if h.reload != nil {
		h.reload()
	}
	writeJSON(w, req)
}

// ToggleUpstream 切换上游启用/禁用（PUT /admin/upstreams/{name}/toggle）。
// 禁用的上游不参与转发路由（selectCandidates 过滤），立即生效（触发 reload）。
func (h *Handler) ToggleUpstream(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if h.store == nil {
		writeJSON(w, map[string]string{"error": "store not available"})
		return
	}
	up, err := h.store.GetUpstream(name)
	if err != nil {
		writeJSON(w, map[string]string{"error": "upstream not found"})
		return
	}
	enabled := 1
	if up.Enabled == 1 {
		enabled = 0
	}
	if err := h.store.SetUpstreamEnabled(name, enabled); err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	if h.reload != nil {
		if err := h.reload(); err != nil {
			writeJSON(w, map[string]string{"error": err.Error()})
			return
		}
	}
	writeJSON(w, map[string]any{"status": "ok", "enabled": enabled == 1})
}

// DeleteUpstream 删除上游（DELETE /admin/upstreams/{name}）。
func (h *Handler) DeleteUpstream(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := h.store.DeleteUpstream(name); err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	if h.reload != nil {
		h.reload()
	}
	writeJSON(w, map[string]string{"status": "deleted"})
}

// ruleHealthState 返回 rule 每个上游的健康检查状态（nil 表示未启用健康检查）。
func (h *Handler) ruleHealthState(upstreams []string) []string {
	if h.hc == nil {
		return nil
	}
	out := make([]string, len(upstreams))
	for i, name := range upstreams {
		out[i] = string(h.hc.Status(name))
	}
	return out
}

// sortUpstreamsJSON 解析规则上游列表（JSON 数组字符串），按路由优先级排序后重新序列化。
// 排序规则与 selectCandidates 一致：tier 升序（free→subscription→payg）→ 同 tier 内 weight 降序。
// 兼容两种输入：JSON 数组字符串（"[\"a\",\"b\"]"）与逗号分隔字符串（"a,b"），
// 逗号串会先转成 JSON 数组再排序入库，避免 DB 存入非法 JSON 导致加载时规则失效（404）。
// 解析失败时原样返回，不阻断保存。
func (h *Handler) sortUpstreamsJSON(s string) string {
	var ups []string
	if err := json.Unmarshal([]byte(s), &ups); err != nil {
		// 非 JSON 数组：尝试按逗号拆分（兼容 "a,b" 旧式/直觉式输入）
		trimmed := strings.TrimSpace(s)
		if trimmed != "" {
			parts := strings.Split(trimmed, ",")
			ups = make([]string, 0, len(parts))
			for _, p := range parts {
				if p = strings.TrimSpace(p); p != "" {
					ups = append(ups, p)
				}
			}
		}
		if len(ups) == 0 {
			return s
		}
	}
	sorted, _, _, _ := sortRuleUpstreams(ups, nil, nil, nil, h)
	out, err := json.Marshal(sorted)
	if err != nil {
		return s
	}
	return string(out)
}

// CloneUpstream 克隆上游：读取原上游配置，改名称和 API key 后创建（POST /admin/upstreams/{name}/clone）。
func (h *Handler) CloneUpstream(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if h.store == nil {
		writeJSON(w, map[string]string{"error": "store not available"})
		return
	}
	orig, err := h.store.GetUpstream(name)
	if err != nil {
		writeJSON(w, map[string]string{"error": "upstream not found: " + err.Error()})
		return
	}
	var req struct {
		Name   string `json:"name"`
		APIKey string `json:"api_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, map[string]string{"error": "invalid request: " + err.Error()})
		return
	}
	if req.Name == "" {
		writeJSON(w, map[string]string{"error": "new name is required"})
		return
	}
	clone := *orig
	clone.Name = req.Name
	if req.APIKey != "" {
		clone.APIKey = req.APIKey
	}
	// 保留原上游的启用状态与计费豁免状态（克隆语义）
	clone.EnabledPtr = &clone.Enabled
	clone.BillingExemptPtr = &clone.BillingExempt
	if err := h.store.CreateUpstream(&clone); err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	if h.reload != nil {
		h.reload()
	}
	writeJSON(w, clone)
}

// UpstreamModels 从上游拉取全部模型（GET /admin/upstreams/{name}/models）。
// 用该上游自己的 api_key 调用 {base_url}/models，返回原始模型名列表。
func (h *Handler) UpstreamModels(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	up := h.upstreamByName(name)
	if up == nil {
		writeJSON(w, map[string]string{"error": "upstream not found"})
		return
	}

	base := strings.TrimSuffix(up.BaseURL, "/")
	target := base + "/models"

	httpReq, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	// 鉴权头按上游协议区分：Gemini 用 x-goog-api-key，其余用 Bearer。
	if up.IsGemini() {
		httpReq.Header.Set("x-goog-api-key", up.APIKey)
	} else if up.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+up.APIKey)
	}

	client := upstreamTestClient(upstreamTestTimeoutFor(up, h.cfg))
	resp, err := client.Do(httpReq)
	if err != nil {
		writeJSON(w, map[string]string{"error": "请求失败: " + err.Error()})
		return
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		writeJSON(w, map[string]any{"error": fmt.Sprintf("上游返回 HTTP %d: %s", resp.StatusCode, truncateStr(string(respBody), 200))})
		return
	}

	var models []string
	if up.IsGemini() {
		// Gemini 格式：{"models":[{"name":"models/gemini-2.0-flash",...}]}，name 带 models/ 前缀。
		var gdata struct {
			Models []struct {
				Name string `json:"name"`
			} `json:"models"`
		}
		if err := json.Unmarshal(respBody, &gdata); err != nil {
			writeJSON(w, map[string]string{"error": "解析响应失败: " + err.Error()})
			return
		}
		for _, m := range gdata.Models {
			id := strings.TrimPrefix(m.Name, "models/")
			if id != "" {
				models = append(models, id)
			}
		}
	} else {
		// OpenAI 兼容格式：{"data":[{"id":"...",...}]}
		var data struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(respBody, &data); err != nil {
			writeJSON(w, map[string]string{"error": "解析响应失败: " + err.Error()})
			return
		}
		for _, m := range data.Data {
			if m.ID != "" {
				models = append(models, m.ID)
			}
		}
	}
	writeJSON(w, map[string]any{"status": "ok", "models": models, "count": len(models)})
}

// TestUpstream 直接使用上游自己的 API Key 测试（POST /admin/upstreams/{name}/test）。
// 绕过网关路由，直连该上游的 /v1/chat/completions，验证 key 与模型可用性。
func (h *Handler) TestUpstream(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	up := h.upstreamByName(name)
	if up == nil {
		writeJSON(w, map[string]string{"error": "upstream not found"})
		return
	}

	var req struct {
		Model           string `json:"model"`
		Content         string `json:"content"`
		MaxTokens       int    `json:"max_tokens"`
		ReasoningEffort string `json:"reasoning_effort"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.Model == "" {
		req.Model = "deepseek-v4-flash"
	}
	if req.Content == "" {
		req.Content = "你好，请用一句话回复"
	}
	if req.MaxTokens <= 0 {
		req.MaxTokens = 512
	}

	// 应用 model_mapping：把客户端简单名还原为上游真实模型名
	realModel := req.Model
	if len(up.ModelMapping) > 0 {
		if v, ok := up.ModelMapping[req.Model]; ok && v != "" {
			realModel = v
			// 竖线多模型映射(如 "mimo-v2.5-free|hy3-free")：测试仅取第一个真实模型，
			// 与真实转发路径 router.MapModel 的随机选择保持语义一致，避免把含 "|" 的串发给上游。
			if i := strings.Index(realModel, "|"); i >= 0 {
				realModel = realModel[:i]
			}
		}
	}

	// 构造直连请求体（用还原后的真实模型名）
	body := map[string]any{
		"model":      realModel,
		"messages":   []map[string]string{{"role": "user", "content": req.Content}},
		"max_tokens": req.MaxTokens,
	}
	// 思考等级：非空时随测试请求带上（auto/自动=不传；none 也透传以便关闭思考）
	if req.ReasoningEffort != "" && req.ReasoningEffort != "auto" {
		body["reasoning_effort"] = req.ReasoningEffort
	}
	payload, _ := json.Marshal(body)

	base := strings.TrimSuffix(up.BaseURL, "/")
	target := base + "/chat/completions"

	httpReq, err := http.NewRequest(http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if up.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+up.APIKey)
	}

	client := upstreamTestClient(upstreamTestTimeoutFor(up, h.cfg))
	started := time.Now()
	resp, err := client.Do(httpReq)
	if err != nil {
		writeJSON(w, map[string]any{
			"error":       "请求失败: " + err.Error(),
			"duration_ms": time.Since(started).Milliseconds(),
		})
		return
	}
	defer resp.Body.Close()
	// 首字节时间（Do 返回即收到响应头）
	ttfbMs := time.Since(started).Milliseconds()
	respBody, _ := io.ReadAll(resp.Body)
	totalMs := time.Since(started).Milliseconds()

	if resp.StatusCode >= 400 {
		writeJSON(w, map[string]any{
			"status":      "fail",
			"code":        resp.StatusCode,
			"error":       string(respBody),
			"duration_ms": totalMs,
			"ttfb_ms":     ttfbMs,
		})
		return
	}
	// 200 但内容是空完成（思考型模型 max_tokens 不足被截断）：HTTP 正常不代表响应有效
	warning := ""
	if proxy.IsEmptyCompletion(respBody) {
		warning = "⚠ 响应内容为空：疑似思考型模型（商汤日日新等默认开启思考）max_tokens 不足，思考未完成即被截断（finish_reason=length）。请调大 max_tokens（如 512+）或检查模型思考模式。"
	}
	// 欠费恢复：测试通过（HTTP <400，含空完成警告——模型可达性已验证）时清除欠费标记。
	// 粒度与标记一致：per_model_billing 上游仅清该模型的模型级欠费记录；
	// 其它上游清上游级 arrears。用户充值后无需重启/改库：标记清除 + 热重载立即恢复。
	if h.store != nil {
		cleared := false
		if up.PerModelBilling && h.store.IsModelArrearRecorded(name, req.Model) {
			if err := h.store.ClearModelArrears(name, req.Model); err == nil {
				cleared = true
				if h.px != nil {
					h.px.ClearModelArrearCache(name, req.Model)
				}
			}
		} else if up.Arrears {
			if err := h.store.SetUpstreamArrears(name, 0); err == nil {
				cleared = true
			}
		}
		if cleared {
			slog.Info("upstream arrear cleared via test", "upstream", name, "model", req.Model)
			if h.reload != nil {
				_ = h.reload()
			}
		}
	}
	writeJSON(w, map[string]any{
		"status":      "ok",
		"body":        json.RawMessage(respBody),
		"warning":     warning,
		"duration_ms": totalMs,
		"ttfb_ms":     ttfbMs,
	})
}
