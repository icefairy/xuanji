package proxy

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/icefairy/xuanji/internal/config"
	"github.com/icefairy/xuanji/internal/router"
)

// TestChatCompletions_MixedConnAndHTTPErrorKeepsHTTPBlacklist 回归测试（2026-09-22 实测）：
// 「部分候选连接类错误 + 部分候选 HTTP 错误（504/502）」混合失败时，
// 不得把**仅 HTTP 错误**的故障上游黑名单一起清掉。
//
// 设计意图（见 proxy.go 该分支注释）：
//
//	「全部候选失败 + 含连接类错误 = 本地网络/全局网络问题……
//	 仅 HTTP 错误（上游真实故障，如 502/429）不清空——单上游规则依赖黑名单跳过故障上游。」
//
// 实现却是：只要 connIssues（任一候选为连接类错误）为真，就清空**所有**候选的黑名单。
//
// 线上实证（2026-09-22，8 次/天，每次 cleared=4）：
// glm-5.3-flash 的候选是 [codebuddy, 基元律动gx, 基元律动lxr, 基元律动]：
//   - codebuddy 慢：`read upstream body: context deadline exceeded`（连接类）
//   - 基元律动系：`status=504 Gateway Time-out`（纯 HTTP 错误，健康探测正常）
//
// 混合失败 → cleared=4 → 连基元律动系的 504 黑名单也被解除 →
// 下一次请求立即重打 504 上游，每次多耗 ~3 分钟（单请求实测 26 分钟）。
// 504 上游本可被 5 分钟黑名单（fast_fail_minutes=5）自然跳过。
func TestChatCompletions_MixedConnAndHTTPErrorKeepsHTTPBlacklist(t *testing.T) {
	// up-http：上游返回 502（纯 HTTP 错误，应保留黑名单）
	upHTTP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html><body>502 Bad Gateway</body></html>"))
	}))
	defer upHTTP.Close()

	// up-conn：端口无服务监听 → 连接被拒（连接类错误 → connIssues=true）
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadAddr := l.Addr().String()
	l.Close()

	cfg := &config.Config{
		Upstreams: []config.Upstream{
			// Weight 降序：up-http 先试（把自己的黑名单建立起来），再轮到 up-conn
			{Name: "up-http", BaseURL: upHTTP.URL, APIKey: "k", Weight: 100, Models: []string{"m"}},
			{Name: "up-conn", BaseURL: "http://" + deadAddr, APIKey: "k", Weight: 10, Models: []string{"m"}},
		},
		Routing: config.Routing{
			DefaultStrategy: "primary_backup",
			Rules:           []config.Rule{{Model: "m", Upstreams: []string{"up-http", "up-conn"}, Strategy: "primary_backup"}},
		},
		// 与线上一致：5xx 可重试，且 max_retries 足够走到第二个候选
		Retry: config.Retry{MaxRetries: 10, RetryStatuses: []int{429, 500, 502, 503, 504}},
	}
	h := New(cfg, router.New(cfg), nil)
	ff := NewFastFailCache(time.Minute)
	h.SetFastFail(ff)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	h.ChatCompletions(httptest.NewRecorder(), req)

	// up-http 是纯 HTTP 故障（502），本轮请求内已被 fastfail 标记。
	// 「清理」只应针对因**本地网络问题**而失败的上游；up-http 的失败与本地网络无关，
	// 其黑名单必须保留，否则后续请求会立即重打这个 502 上游。
	if !ff.IsBlacklisted("up-http", "m") {
		t.Error("纯 HTTP 错误（502）的上游黑名单被混合场景清掉了 —— 与注释" +
			"「仅 HTTP 错误（上游真实故障，如 502/429）不清空」相悖；" +
			"线上实测这会让 504 上游每次被白打（单请求 26 分钟）")
	}
}
