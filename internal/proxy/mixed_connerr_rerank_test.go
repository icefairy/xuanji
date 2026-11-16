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

// 本文件覆盖 rerank / embeddings 链路的同一缺陷（与 ChatCompletions 同源）：
// 「部分候选连接类错误 + 部分候选 HTTP 错误」混合失败时，
// 不得把**仅 HTTP 错误**的故障上游黑名单一起清掉。
//
// 2026-09-22 实测（chat 链路，8 次/天）：glm-5.3-flash 候选
// [codebuddy(超时/连接类), 基元律动gx/lxr/(纯 504)] 混合失败 → cleared=4
// 把纯 504 上游的黑名单一并解除 → 下次请求立即重打 504 上游，单请求白耗 26 分钟。
// 三条链路（chat/rerank/embeddings）清黑名单逻辑同构，故一并修 + 一并测。

// mixedErrCfg 构造「一个 HTTP 502 上游 + 一个连接拒绝上游」的配置。
func mixedErrCfg(t *testing.T, model, realModel, httpURL, deadAddr string) *config.Config {
	t.Helper()
	return &config.Config{
		Upstreams: []config.Upstream{
			// Weight 降序：HTTP 故障上游先试（建立自己的黑名单），再轮到连接失败的上游
			{
				Name: "up-http", BaseURL: httpURL, APIKey: "k", Weight: 100,
				Models: []string{realModel}, ModelMapping: map[string]string{model: realModel},
			},
			{Name: "up-conn", BaseURL: "http://" + deadAddr, APIKey: "k", Weight: 10,
				Models: []string{realModel}, ModelMapping: map[string]string{model: realModel}},
		},
		Routing: config.Routing{
			DefaultStrategy: "primary_backup",
			Rules:           []config.Rule{{Model: model, Upstreams: []string{"up-http", "up-conn"}, Strategy: "primary_backup"}},
		},
		Retry: config.Retry{MaxRetries: 10, RetryStatuses: []int{429, 500, 502, 503, 504}},
	}
}

// deadAddr 返回一个必然 connection refused 的地址。
func deadAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

// TestRerank_MixedConnAndHTTPErrorKeepsHTTPBlacklist 见文件头说明。
func TestRerank_MixedConnAndHTTPErrorKeepsHTTPBlacklist(t *testing.T) {
	upHTTP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html><body>502 Bad Gateway</body></html>"))
	}))
	defer upHTTP.Close()

	cfg := mixedErrCfg(t, "rerankv2m3", "BAAI/bge-reranker-v2-m3", upHTTP.URL, deadAddr(t))
	h := New(cfg, router.New(cfg), nil)
	ff := NewFastFailCache(time.Minute)
	h.SetFastFail(ff)

	doRerank(t, h, `{"model":"rerankv2m3","query":"apple","documents":["a"]}`)

	if !ff.IsBlacklisted("up-http", "BAAI/bge-reranker-v2-m3") {
		t.Error("rerank：纯 HTTP 502 上游的黑名单被混合场景清掉了" +
			"（与注释「仅 HTTP 错误不清空」相悖）")
	}
}

// TestEmbeddings_MixedConnAndHTTPErrorKeepsHTTPBlacklist 见文件头说明。
func TestEmbeddings_MixedConnAndHTTPErrorKeepsHTTPBlacklist(t *testing.T) {
	upHTTP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":{"message":"bad gateway"}}`))
	}))
	defer upHTTP.Close()

	cfg := mixedErrCfg(t, "bge-m3", "BAAI/bge-m3", upHTTP.URL, deadAddr(t))
	h := New(cfg, router.New(cfg), nil)
	ff := NewFastFailCache(time.Minute)
	h.SetFastFail(ff)

	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(`{"model":"bge-m3","input":["hi"]}`))
	req.Header.Set("Content-Type", "application/json")
	h.Embeddings(httptest.NewRecorder(), req)

	if !ff.IsBlacklisted("up-http", "BAAI/bge-m3") {
		t.Error("embeddings：纯 HTTP 502 上游的黑名单被混合场景清掉了" +
			"（与注释「仅 HTTP 错误不清空」相悖）")
	}
}
