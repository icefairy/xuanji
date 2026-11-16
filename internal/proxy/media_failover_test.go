package proxy

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/icefairy/xuanji/internal/config"
	"github.com/icefairy/xuanji/internal/health"
	"github.com/icefairy/xuanji/internal/router"
	"github.com/icefairy/xuanji/internal/store"
)

// 本文件覆盖 media 链路（TTS/ASR/图像）的 4xx 故障转移。
//
// 背景（2026-11-06 日志实测）：
//
//	07:38:00 WARN upstream 4xx/5xx detail upstream=stepfun model=stepaudio-2.5-tts status=404
//	         upstream_error="The model \"stepaudio-2.5-tts\" does not exist or you do not have access to it"
//	07:38:00 INFO audio/speech model=stepaudio-2.5-tts upstream=stepfun status=404
//
// 路由规则是 `stepaudio-2.5-tts → ["stepfun","piper-local"] primary_backup`，
// 即**配了备用上游**。但 forwardMediaJSON 的 4xx 分支直接 `return true, false`：
// retryable=false → 候选循环 `break` → 备用上游（piper-local）从未被尝试。
// 实测 11-05 单日 156 次 404 直接透传给客户端，piper-local 零次承接。
//
// 对比 chat / rerank 链路：命中 isClientRequestError 时返回 `false, true`，
// 虽不标记上游故障，但**会继续换下一个候选上游**。media 链路漏了这一层。

// newMediaFailoverHandler 构造「主上游返回指定错误 + 备用上游返回音频」的双上游 Handler。
func newMediaFailoverHandler(t *testing.T, primaryStatus int, primaryBody string) (*Handler, *int) {
	t.Helper()

	backupHits := 0
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(primaryStatus)
		fmt.Fprint(w, primaryBody)
	}))
	t.Cleanup(primary.Close)

	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		backupHits++
		w.Header().Set("Content-Type", "audio/mpeg")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("FAKE_MP3_BYTES"))
	}))
	t.Cleanup(backup.Close)

	cfg := &config.Config{
		Upstreams: []config.Upstream{
			{
				// tier=subscription(0) < free(1)：保证 primary 确定排在前面。
				//
				// ⚠ 必须用 tier 拉开顺序，不能只靠 Priority：selectCandidates 对
				// 【同 tier + 同有效权重 + 同折扣状态】的候选会**随机打乱**
				// （避免固定原序导致流量倾斜，见 selectCandidates 末尾）。
				// 2026-11-08 实测：两个上游都留空 tier（默认 payg）时，
				// backup 有约 50% 概率排在前面，导致本文件的反向保护用例
				// TestMediaFailover_ParamErrorStillPassThrough 假红（10 次跑挂 6 次）。
				Name: "primary-up", BaseURL: primary.URL, APIKey: "sk-1",
				Tier: "subscription", Priority: 10, Weight: 100, Models: []string{"tts-x"},
			},
			{
				Name: "backup-up", BaseURL: backup.URL, APIKey: "sk-2",
				Tier: "free", Priority: 20, Weight: 100, Models: []string{"tts-x"},
			},
		},
		Routing: config.Routing{
			DefaultStrategy: "primary_backup",
			Rules: []config.Rule{
				{Model: "tts-x", Upstreams: []string{"primary-up", "backup-up"}, Strategy: "primary_backup"},
			},
		},
		Retry: config.Retry{MaxRetries: 3, RetryStatuses: []int{429, 500, 502, 503, 504}},
	}
	hc := health.New(cfg)
	t.Cleanup(hc.Close)

	st, err := store.Open(filepath.Join(t.TempDir(), "xuanji.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	h := New(cfg, router.New(cfg), hc)
	h.SetStore(st)
	h.SetRecorder(store.NewRecorder(st))
	return h, &backupHits
}

func doSpeech(h *Handler, model string) *httptest.ResponseRecorder {
	body := fmt.Sprintf(`{"model":%q,"input":"测试","voice":"zh-CN"}`, model)
	req := httptest.NewRequest(http.MethodPost, "/v1/audio/speech", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.AudioSpeech(rec, req)
	return rec
}

// TestMediaFailover_ModelNotExistSwitchesToBackup 回归测试：
// 主上游 404「模型不存在」时，必须继续尝试备用上游（router 已配 primary_backup）。
//
// 修复前：4xx 直接透传，retryable=false，backup 从不被调用。
func TestMediaFailover_ModelNotExistSwitchesToBackup(t *testing.T) {
	const body = `{"error":{"message":"The model \"tts-x\" does not exist or you do not have access to it.","type":"invalid_request_error","code":"model_not_found"}}`
	h, backupHits := newMediaFailoverHandler(t, http.StatusNotFound, body)

	rec := doSpeech(h, "tts-x")

	if *backupHits == 0 {
		t.Fatalf("备用上游从未被调用（4xx 直接透传，retryable=false 导致候选循环 break）; "+
			"客户端拿到 status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Code != http.StatusOK {
		t.Errorf("备用上游可用时应返回 200，实际 status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "FAKE_MP3_BYTES") {
		t.Errorf("响应体应来自备用上游: %s", rec.Body.String())
	}
}

// TestMediaFailover_ModelNotExistStillNotCountedAsUpstreamFailure 反向保护：
// 换上游不等于把「模型不存在」记成上游故障——主上游本身是健康的。
func TestMediaFailover_ModelNotExistStillNotCountedAsUpstreamFailure(t *testing.T) {
	const body = `{"error":{"message":"The model \"tts-x\" does not exist or you do not have access to it.","type":"invalid_request_error","code":"model_not_found"}}`
	h, _ := newMediaFailoverHandler(t, http.StatusNotFound, body)

	doSpeech(h, "tts-x")

	if got := h.health.Status("primary-up"); got != health.StateHealthy {
		t.Errorf("primary-up health = %q, want %q —— 模型不存在是客户端请求问题，"+
			"上游只是合法拒绝，不应计入上游故障", got, health.StateHealthy)
	}
}

// TestMediaFailover_ParamErrorStillPassThrough 反向保护：
// 请求参数类 4xx（换了上游也一样失败）不应触发切换，直接透传给客户端。
//
// 为什么重要：TTS 的 input/voice 参数错误属于确定性客户端错误，
// 挨个上游试一遍只会放大延迟与上游压力，且最终仍是 400。
func TestMediaFailover_ParamErrorStillPassThrough(t *testing.T) {
	const body = `{"error":{"message":"input is required and must not be empty","type":"invalid_request_error"}}`
	h, backupHits := newMediaFailoverHandler(t, http.StatusBadRequest, body)

	rec := doSpeech(h, "tts-x")

	if *backupHits != 0 {
		t.Errorf("参数类 400 不应切换上游，但 backup 被调用了 %d 次", *backupHits)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("应原样透传上游 400，实际 status=%d", rec.Code)
	}
}

// TestMediaFailover_5xxStillFailsOver 反向保护：
// 5xx 本来就该切换上游（既有行为，不能因本次修改而破坏）。
func TestMediaFailover_5xxStillFailsOver(t *testing.T) {
	h, backupHits := newMediaFailoverHandler(t, http.StatusBadGateway, `{"error":{"message":"bad gateway"}}`)

	rec := doSpeech(h, "tts-x")

	if *backupHits == 0 {
		t.Errorf("5xx 应当切换上游，但 backup 从未被调用")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("备用可用时应 200，实际 status=%d", rec.Code)
	}
}

// TestIsModelUnavailableError 单测特征判定的边界：
// 只认「该上游没有此模型/无权限」，不能把参数类错误也算进去
// （否则一个 input 为空的请求会把每个候选上游都打一遍）。
func TestIsModelUnavailableError(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{
			// 2026-11-06 实测 stepfun TTS 原文
			name:   "stepfun 模型不存在/无权限",
			status: http.StatusNotFound,
			body:   `{"error":{"message":"The model \"stepaudio-2.5-tts\" does not exist or you do not have access to it."}}`,
			want:   true,
		},
		{
			name:   "unisound 模型路由不存在",
			status: http.StatusNotFound,
			body:   `{"error":{"message":"model route not found","type":"not_found_error","code":"model_not_found"}}`,
			want:   true,
		},
		{
			name:   "通用 model not found",
			status: http.StatusBadRequest,
			body:   `{"error":{"message":"Model not found: foobar"}}`,
			want:   true,
		},
		// —— 反向保护：参数类错误不该触发换上游 ——
		{
			name:   "TTS input 为空（换上游也一样失败）",
			status: http.StatusBadRequest,
			body:   `{"error":{"message":"input is required and must not be empty"}}`,
			want:   false,
		},
		{
			name:   "音色不存在（换上游也一样失败）",
			status: http.StatusBadRequest,
			body:   `{"error":{"message":"Param Incorrect","param":"Unknown voice: female"}}`,
			want:   false,
		},
		{
			name:   "401 鉴权失败不属本判定",
			status: http.StatusUnauthorized,
			body:   `{"error":{"message":"model not found"}}`,
			want:   false,
		},
		{
			name:   "5xx 不属 4xx 判定",
			status: http.StatusBadGateway,
			body:   `does not exist`,
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isModelUnavailableError(tt.status, strings.NewReader(tt.body))
			if got != tt.want {
				t.Errorf("isModelUnavailableError(%d, %q) = %v, want %v", tt.status, tt.body, got, tt.want)
			}
		})
	}
}
