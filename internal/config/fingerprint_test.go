package config

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/icefairy/xuanji/internal/config/piheaders"
)

// ---- 头部集合 ----

// TestPiHeadersShape 验证指纹头部顺序常量符合 pi 0.85.1 实测形态。
func TestPiHeadersShape(t *testing.T) {
	want := []string{
		"host", "connection", "Accept",
		"X-Stainless-Retry-Count", "X-Stainless-Timeout", "X-Stainless-Lang",
		"X-Stainless-Package-Version", "X-Stainless-OS", "X-Stainless-Arch",
		"X-Stainless-Runtime", "X-Stainless-Runtime-Version",
		"authorization", "User-Agent", "content-type", "accept-language",
		"sec-fetch-mode", "accept-encoding", "content-length",
	}
	if len(piheaders.Order) != len(want) {
		t.Fatalf("头部顺序长度 = %d, 期望 %d: %v", len(piheaders.Order), len(want), piheaders.Order)
	}
	for i := range want {
		if !strings.EqualFold(piheaders.Order[i], want[i]) {
			t.Errorf("第 %d 个头 = %q, 期望 %q", i+1, piheaders.Order[i], want[i])
		}
	}
}

// TestPiDefaultHeadersValues 验证固定值头部与 pi 实测一致。
func TestPiDefaultHeadersValues(t *testing.T) {
	cases := map[string]string{
		"X-Stainless-Lang":            "js",
		"X-Stainless-OS":              "Linux",
		"X-Stainless-Arch":            "x64",
		"X-Stainless-Runtime":         "node",
		"X-Stainless-Runtime-Version": "v22.22.1",
		"X-Stainless-Retry-Count":     "0",
		"X-Stainless-Timeout":         "300",
		"X-Stainless-Package-Version": "6.40.0",
		"accept-language":             "*",
		"sec-fetch-mode":              "cors",
		"Accept":                      "application/json",
	}
	for k, want := range cases {
		if got := piheaders.Default[k]; got != want {
			t.Errorf("piheaders.Default[%q] = %q, 期望 %q", k, got, want)
		}
	}
	if piheaders.UserAgent != "pi/0.85.1 (linux; node/v22.22.1; x64)" {
		t.Errorf("UserAgent = %q, 期望 pi 0.85.1 实测值", piheaders.UserAgent)
	}
	if DefaultUpstreamUserAgent != piheaders.UserAgent {
		t.Error("DefaultUpstreamUserAgent 应与 piheaders.UserAgent 一致")
	}
}

// TestApplyPiFingerprintHeaders 验证注入结果：固定头齐全且不覆盖调用方值。
func TestApplyPiFingerprintHeaders(t *testing.T) {
	SetPiGzip(false)
	t.Cleanup(func() { SetPiGzip(false) })

	req := newTestRequest(t, "http://up.example.com/v1/chat/completions")
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("Content-Type", "application/json")
	ApplyPiFingerprintHeaders(req)

	for k, v := range piheaders.Default {
		if got := req.Header.Get(k); got != v {
			t.Errorf("头部 %q = %q, 期望 %q", k, got, v)
		}
	}
	if got := req.Header.Get("Authorization"); got != "Bearer sk-test" {
		t.Errorf("不应覆盖 Authorization, got %q", got)
	}
	if got := req.Header.Get("accept-encoding"); got != "" {
		t.Errorf("默认不应设置 accept-encoding, got %q", got)
	}
}

// TestApplyPiFingerprintHeaders_GzipOn 验证开启后补上 accept-encoding。
func TestApplyPiFingerprintHeaders_GzipOn(t *testing.T) {
	SetPiGzip(true)
	t.Cleanup(func() { SetPiGzip(false) })
	req := newTestRequest(t, "http://up.example.com/v1/chat/completions")
	ApplyPiFingerprintHeaders(req)
	if got := req.Header.Get("accept-encoding"); got != piheaders.AcceptEncoding {
		t.Errorf("accept-encoding = %q, 期望 %q", got, piheaders.AcceptEncoding)
	}
}

// TestApplyPiFingerprintHeaders_RespectsExisting 验证不覆盖调用方已设的同名头。
func TestApplyPiFingerprintHeaders_RespectsExisting(t *testing.T) {
	req := newTestRequest(t, "http://up.example.com/v1/chat/completions")
	req.Header.Set("X-Stainless-Lang", "python")
	req.Header.Set("accept-language", "zh-CN")
	ApplyPiFingerprintHeaders(req)
	if got := req.Header.Get("X-Stainless-Lang"); got != "python" {
		t.Errorf("不应覆盖已有 X-Stainless-Lang, got %q", got)
	}
	if got := req.Header.Get("accept-language"); got != "zh-CN" {
		t.Errorf("不应覆盖已有 accept-language, got %q", got)
	}
}

// TestApplyPiFingerprintHeaders_StripsConflicting 验证剥离他者身份头。
func TestApplyPiFingerprintHeaders_StripsConflicting(t *testing.T) {
	req := newTestRequest(t, "http://up.example.com/v1/chat/completions")
	req.Header.Set("X-Product", "SaaS")
	req.Header.Set("Traceparent", "00-abc-def-01")
	ApplyPiFingerprintHeaders(req)
		if got := req.Header.Get(k); got != "" {
			t.Errorf("应剥离 %q, got %q", k, got)
		}
	}
}

// ---- 头部顺序（连接层） ----

// TestReorderHeaderBlock 直接验证重排函数：输入 Go 字典序，输出 pi 顺序。
func TestReorderHeaderBlock(t *testing.T) {
	head := "POST /v1/chat/completions HTTP/1.1\r\n" +
		"Host: up.example.com\r\n" +
		"User-Agent: " + piheaders.UserAgent + "\r\n" +
		"Content-Length: 5\r\n" +
		"Accept: application/json\r\n" +
		"Accept-Encoding: gzip\r\n" +
		"Authorization: Bearer sk-x\r\n" +
		"Content-Type: application/json\r\n" +
		"X-Stainless-Lang: js\r\n"
	out := string(reorderHeaderBlock([]byte(head)))
	names := headerNames(out)
	// pi 名义顺序：Host(0) connection(1) Accept(2) X-Stainless-Lang(5) authorization(11)
	//              User-Agent(12) content-type(13) accept-encoding(16) content-length(17)
	wantByRank := []string{"Host", "connection", "Accept", "X-Stainless-Lang", "Authorization",
		"User-Agent", "Content-Type", "Accept-Encoding", "Content-Length"}
	for i, w := range wantByRank {
		if i >= len(names) || names[i] != w {
			t.Fatalf("重排后顺序 = %v, 期望 %v", names, wantByRank)
		}
	}
	// 请求行必须保持不变
	if !strings.HasPrefix(out, "POST /v1/chat/completions HTTP/1.1\r\n") {
		t.Errorf("请求行被破坏: %q", out[:60])
	}
}

// TestReorderHeaderBlock_UnknownKeptLast 验证未登记头保持相对顺序排在已知头之后。
func TestReorderHeaderBlock_UnknownKeptLast(t *testing.T) {
	head := "POST / HTTP/1.1\r\n" +
		"Accept: application/json\r\n" +
		"X-Custom-B: 2\r\n" +
		"X-Custom-A: 1\r\n"
	out := string(reorderHeaderBlock([]byte(head)))
	names := headerNames(out)
	// connection 名次为 1，排在未登记头与 Accept 之前
	want := []string{"connection", "Accept", "X-Custom-B", "X-Custom-A"}
	for i, w := range want {
		if i >= len(names) || names[i] != w {
			t.Fatalf("顺序 = %v, 期望 %v", names, want)
		}
	}
}

// TestReorderHeaderBlock_InvalidPassthrough 验证非法头部不破坏请求内容。
func TestReorderHeaderBlock_InvalidPassthrough(t *testing.T) {
	bad := []byte("NOT-HTTP-BLAH\r\nfoo")
	got := reorderHeaderBlock(bad)
	if !bytes.HasPrefix(got, bad) {
		t.Errorf("非法输入内容应保留, got %q", got)
	}
	if !bytes.HasSuffix(got, []byte("\r\n\r\n")) {
		t.Errorf("应补回头部结束空行, got %q", got)
	}
}

// TestPiFingerprintConn_SplitWrites 验证头部被分多次写入时仍能正确重排。
func TestPiFingerprintConn_SplitWrites(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	conn := newPiFingerprintConn(client)

	go func() {
		// 分 4 段写，模拟 Go 的流式写出
		for _, seg := range []string{
			"POST /v1/chat/completions HTTP/1.1\r\nHost: up.example.com\r\n",
			"User-Agent: " + piheaders.UserAgent + "\r\n",
			"Content-Length: 5\r\nAuthorization: Bearer sk-x\r\n",
			"Accept: application/json\r\n\r\nhello",
		} {
			conn.Write([]byte(seg))
		}
		conn.Close()
	}()

	out, _ := io.ReadAll(server)
	text := string(out)
	names := headerNames(text)
	want := []string{"Host", "connection", "Accept", "Authorization", "User-Agent", "Content-Length"}
	for i, w := range want {
		if i >= len(names) || names[i] != w {
			t.Fatalf("分段写入后顺序 = %v, 期望 %v", names, want)
		}
	}
	if !strings.HasSuffix(text, "\r\n\r\nhello") {
		t.Errorf("body 应完整保留, tail=%q", text[max(0, len(text)-16):])
	}
}

// TestPiFingerprintConn_PassthroughAfterHeader 验证头部之后的写入原样透传。
func TestPiFingerprintConn_PassthroughAfterHeader(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	conn := newPiFingerprintConn(client)

	go func() {
		conn.Write([]byte("POST / HTTP/1.1\r\nHost: h\r\n\r\n"))
		conn.Write([]byte("PART2-PASSTHROUGH"))
		conn.Close()
	}()
	out, _ := io.ReadAll(server)
	if !strings.HasSuffix(string(out), "PART2-PASSTHROUGH") {
		t.Errorf("头部之后应透传, got %q", out)
	}
}

// TestPiFingerprintConn_NonHTTPPassthrough 验证非 HTTP 数据不被破坏。
func TestPiFingerprintConn_NonHTTPPassthrough(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	conn := newPiFingerprintConn(client)
	go func() {
		conn.Write([]byte("GARBAGE-NOT-HTTP\r\n\r\n"))
		conn.Close()
	}()
	out, _ := io.ReadAll(server)
	if string(out) != "GARBAGE-NOT-HTTP\r\n\r\n" {
		t.Fatalf("非 HTTP 应原样透传, got %q", out)
	}
}

// TestPiFingerprintConn_LargeBodyPassthrough 验证大 body 不被缓冲破坏。
func TestPiFingerprintConn_LargeBodyPassthrough(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	conn := newPiFingerprintConn(client)

	body := bytes.Repeat([]byte("x"), 200000)
	go func() {
		conn.Write([]byte("POST / HTTP/1.1\r\nHost: h\r\n\r\n"))
		conn.Write(body)
		conn.Close()
	}()
	out, _ := io.ReadAll(server)
	idx := bytes.Index(out, []byte("\r\n\r\n"))
	if idx < 0 || !bytes.Equal(out[idx+4:], body) {
		t.Fatalf("大 body 应完整透传, got %d 字节", len(out))
	}
}

// ---- Transport 接线 ----

// TestNewForwardTransport_Wiring 验证转发 Transport 已挂指纹 DialTLS，
// 且保留连接复用与空闲超时设置。
func TestNewForwardTransport_Wiring(t *testing.T) {
	tr := NewForwardTransport(0)
	if tr.DialTLSContext == nil {
		t.Fatal("应设置 DialTLSContext 以重排头部顺序")
	}
	if tr.DialContext == nil {
		t.Fatal("应保留 DialContext")
	}
	if tr.DisableKeepAlives {
		t.Error("转发 Transport 不应禁用 keep-alive")
	}
	if tr.IdleConnTimeout != ForwardTransportIdleTimeout {
		t.Errorf("IdleConnTimeout = %v, 期望 %v", tr.IdleConnTimeout, ForwardTransportIdleTimeout)
	}
}

// TestForwardTransport_EndToEndHeaderOrder 端到端验证：真实走 TLS 请求，
// 服务端读到的头部顺序必须是 pi 顺序（Host 首位、UA 居次、Accept 在 UA 之后）。
func TestForwardTransport_EndToEndHeaderOrder(t *testing.T) {
	cert, err := tls.X509KeyPair(testCertPEM, testKeyPEM)
	if err != nil {
		t.Fatalf("证书解析失败: %v", err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	defer ln.Close()

	got := make(chan []string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			got <- nil
			return
		}
		defer c.Close()
		br := bufio.NewReader(c)
		var names []string
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				break
			}
			line = strings.TrimRight(line, "\r\n")
			if line == "" {
				break
			}
			if i := strings.Index(line, ":"); i > 0 {
				names = append(names, line[:i])
			}
		}
		got <- names
		// 回一个最小 HTTP 响应，避免客户端读响应时 EOF。
		c.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"))
	}()

	tr := NewForwardTransport(0)
	tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}

	body := `{"model":"m","messages":[]}`
	req, _ := http.NewRequest("POST", "https://"+ln.Addr().String()+"/v1/chat/completions",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-test")
	SetUpstreamUserAgent(piheaders.UserAgent)
	t.Cleanup(func() { SetUpstreamUserAgent("") })
	ApplyUpstreamUserAgent(req)
	ApplyPiFingerprintHeaders(req)

	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	resp.Body.Close()

	names := <-got
	if len(names) != len(piheaders.Order) {
		t.Fatalf("头部数 = %d, 期望 %d（实测 pi 顺序）: %v", len(names), len(piheaders.Order), names)
	}
	// 逐位比对 pi 实测顺序：这是最严格的断言。
	for i, want := range piheaders.Order {
		if !strings.EqualFold(names[i], want) {
			t.Errorf("第 %d 个头 = %q, 期望 %q\n实际顺序: %v", i+1, names[i], want, names)
		}
	}
	// 关键尖点：Host 首位，User-Agent 在 Authorization 之后（与 Go 默认"UA 第 2 位"不同）
	if names[0] != "Host" {
		t.Errorf("首个头应为 Host, got %v", names)
	}
	idx := func(n string) int {
		for i, v := range names {
			if strings.EqualFold(v, n) {
				return i
			}
		}
		return -1
	}
	accept, auth, ua := idx("Accept"), idx("authorization"), idx("User-Agent")
	if !(accept < auth && auth < ua) {
		t.Errorf("顺序应为 Accept < authorization < User-Agent, got %v", names)
	}
	// 指纹头应齐全
	for _, k := range []string{"X-Stainless-Lang", "sec-fetch-mode", "accept-language"} {
		if idx(k) < 0 {
			t.Errorf("缺少指纹头 %q: %v", k, names)
		}
	}
}

// ---- 透明解压 ----

// TestDecompressRoundTripper_Gzip 验证 gzip 响应被透明解压且剥离 Content-Encoding。
func TestDecompressRoundTripper_Gzip(t *testing.T) {
	body := []byte(`{"ok":true,"msg":"plain json"}`)
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write(body)
	zw.Close()

	rt := &decompressRoundTripper{base: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode:    200,
			Header:        http.Header{"Content-Encoding": {"gzip"}},
			Body:          io.NopCloser(bytes.NewReader(buf.Bytes())),
			ContentLength: int64(buf.Len()),
		}, nil
	})}
	req := newTestRequest(t, "http://up.example.com/v1/chat/completions")
	req.Header.Set("accept-encoding", piheaders.AcceptEncoding)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip 失败: %v", err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(got, body) {
		t.Errorf("解压后 = %q, 期望 %q", got, body)
	}
	if ce := resp.Header.Get("Content-Encoding"); ce != "" {
		t.Errorf("Content-Encoding 应剥离, got %q", ce)
	}
}

// TestDecompressRoundTripper_DeflateZlib 验证 zlib 包装的 deflate 可解。
func TestDecompressRoundTripper_DeflateZlib(t *testing.T) {
	body := []byte(`{"ok":true}`)
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	zw.Write(body)
	zw.Close()

	rt := &decompressRoundTripper{base: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Encoding": {"deflate"}},
			Body:       io.NopCloser(bytes.NewReader(buf.Bytes())),
		}, nil
	})}
	resp, err := rt.RoundTrip(newTestRequest(t, "http://up.example.com/x"))
	if err != nil {
		t.Fatalf("RoundTrip 失败: %v", err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(got, body) {
		t.Errorf("zlib 解压后 = %q, 期望 %q", got, body)
	}
}

// TestDecompressRoundTripper_RawDeflate 验证裸 deflate 可解。
func TestDecompressRoundTripper_RawDeflate(t *testing.T) {
	body := []byte(`{"raw":true}`)
	var buf bytes.Buffer
	fw, _ := flate.NewWriter(&buf, flate.DefaultCompression)
	fw.Write(body)
	fw.Close()

	rt := &decompressRoundTripper{base: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Encoding": {"deflate"}},
			Body:       io.NopCloser(bytes.NewReader(buf.Bytes())),
		}, nil
	})}
	resp, err := rt.RoundTrip(newTestRequest(t, "http://up.example.com/x"))
	if err != nil {
		t.Fatalf("RoundTrip 失败: %v", err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(got, body) {
		t.Errorf("裸 deflate 解压后 = %q, 期望 %q", got, body)
	}
}

// TestDecompressRoundTripper_PlainPassthrough 验证未压缩响应原样返回。
func TestDecompressRoundTripper_PlainPassthrough(t *testing.T) {
	body := []byte(`{"ok":true}`)
	rt := &decompressRoundTripper{base: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(body))}, nil
	})}
	resp, err := rt.RoundTrip(newTestRequest(t, "http://up.example.com/x"))
	if err != nil {
		t.Fatalf("RoundTrip 失败: %v", err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(got, body) {
		t.Errorf("明文应原样返回, got %q", got)
	}
}

// TestDecompressRoundTripper_Streaming 验证流式响应可边读边解（不整体缓冲）。
func TestDecompressRoundTripper_Streaming(t *testing.T) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	for _, chunk := range []string{"data: a\n\n", "data: b\n\n", "data: [DONE]\n\n"} {
		zw.Write([]byte(chunk))
		zw.Flush()
	}
	zw.Close()

	rt := &decompressRoundTripper{base: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Encoding": {"gzip"}},
			Body:       io.NopCloser(bytes.NewReader(buf.Bytes())),
		}, nil
	})}
	resp, err := rt.RoundTrip(newTestRequest(t, "http://up.example.com/x"))
	if err != nil {
		t.Fatalf("RoundTrip 失败: %v", err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(got), "data: [DONE]") {
		t.Errorf("流式内容应完整解出, got %q", got)
	}
}

// TestNewClientWithPiFingerprint 验证包装后的 client 可用。
func TestNewClientWithPiFingerprint(t *testing.T) {
	c := NewClientWithPiFingerprint(NewForwardTransport(0))
	if c == nil || c.Transport == nil {
		t.Fatal("client 及其 Transport 不应为 nil")
	}
}

// ---- 辅助 ----

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func newTestRequest(t *testing.T, url string) *http.Request {
	t.Helper()
	req, err := http.NewRequest("POST", url, strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	return req
}

// headerNames 从完整请求文本中按出现顺序提取头部名。
func headerNames(text string) []string {
	var names []string
	for _, l := range strings.Split(text, "\r\n")[1:] {
		if l == "" {
			break
		}
		if i := strings.Index(l, ":"); i > 0 {
			names = append(names, l[:i])
		}
	}
	return names
}

// testCertPEM / testKeyPEM 是本地自签证书（仅测试用，127.0.0.1，有效期 10 年）。
var (
	testCertPEM = []byte(testCertData)
	testKeyPEM  = []byte(testKeyData)
)

const testCertData = `-----BEGIN CERTIFICATE-----
MIIDGjCCAgKgAwIBAgIUB2Q7fmpHaH95tIWNSfFm2XqiGlIwDQYJKoZIhvcNAQEL
BQAwFDESMBAGA1UEAwwJMTI3LjAuMC4xMB4XDTI2MDkxNjExMjMyMVoXDTM2MDkx
MzExMjMyMVowFDESMBAGA1UEAwwJMTI3LjAuMC4xMIIBIjANBgkqhkiG9w0BAQEF
AAOCAQ8AMIIBCgKCAQEAxXrVTQCPJd62EYbzA0drAeee7B4XbsUXuNUmR8z8aOEG
7O7kERCvdsSTJu0yjNNREWeAUg868bZqSJ7j4211Pv9EUhE9iQCHRxjqT54CA2I9
1iYu0URB46nUIjv37KxRlTLsg1AWGwoKP3WYxxqvAYw16bdlfXM12drCqjWnQsE5
gCOrBDBvC+KXcyxaeNCxG3jAN7/Nb40run4CdibmOzrO0ocdyvBn6g5cf8LO3VHp
w4ugm3nGvaO1vMtR5HJswLCNXT7wZleeVZHC+JFrUZoKrDRnQdnYr2uXZG3lo54W
j1gwl0kj84iCtRfD5H1XOtCaAY7M+RLTZNvFGqxUfQIDAQABo2QwYjAdBgNVHQ4E
FgQU8wlAg7hC9mGTjoWAU7NN6DS7Y5kwHwYDVR0jBBgwFoAU8wlAg7hC9mGTjoWA
U7NN6DS7Y5kwDwYDVR0TAQH/BAUwAwEB/zAPBgNVHREECDAGhwR/AAABMA0GCSqG
SIb3DQEBCwUAA4IBAQBF/HykyZZdHrg4VzfRQpHYPvbvlKmxA4qRr0lnggi3/szD
a5KD2TOhUWUXS6/PYMc4ijHgs5y/HzBcIuvuXGSkrCqjR44H5lXNty/hh7rNXgu8
gvJgeWEGoKQzodTmAMPNFXF12FSMjk1yeYGaozYUycK9NcdKEZNJOcv+YUYIagoh
izaPlAJrLsIwDORDelCwNYMCcVutSAw1nrYOUIU8tvK2Ec3zomT6+msUoL49aNNO
CSlmq7KghWODaxm5o4V6Leuzcpp33hmVWKSO5hdgVOFvJfg9WB53uan1V0UZ73xh
G5hCnpATUKYnZcTAQ/6JeK70cOJPydVq1s1yoDGT
-----END CERTIFICATE-----
`
const testKeyData = `-----BEGIN PRIVATE KEY-----
MIIEvAIBADANBgkqhkiG9w0BAQEFAASCBKYwggSiAgEAAoIBAQDFetVNAI8l3rYR
hvMDR2sB557sHhduxRe41SZHzPxo4Qbs7uQREK92xJMm7TKM01ERZ4BSDzrxtmpI
nuPjbXU+/0RSET2JAIdHGOpPngIDYj3WJi7RREHjqdQiO/fsrFGVMuyDUBYbCgo/
dZjHGq8BjDXpt2V9czXZ2sKqNadCwTmAI6sEMG8L4pdzLFp40LEbeMA3v81vjSu6
fgJ2JuY7Os7Shx3K8GfqDlx/ws7dUenDi6Cbeca9o7W8y1HkcmzAsI1dPvBmV55V
kcL4kWtRmgqsNGdB2diva5dkbeWjnhaPWDCXSSPziIK1F8PkfVc60JoBjsz5EtNk
28UarFR9AgMBAAECggEABPzp76dN9k+TxbQ6OyD4q576vU1dRielm742g1BtfQYC
CMYCmLO/2tNy7A7IXL+Rp/Y3SttQ9SioXUE2JwPCwbPs49hvoiA99SZvSfzcYX1/
wQL3N8NDmXPWry6t6m/HzRRGM7g4IWK1iON4LRSYIFqMRLpPI/C0XCunCGmRL0xq
wf5pfpJYp5Q8hf0/Cfz3wMR6Lrqx5pergb+lZO5uEe4TclHO+aXOr5Lm0fNI10uJ
lAM2lzNQaAuQ2N04j45udk7pwM2kCuM3RKL0czsG2hkUpAgCLOn42s7ohO/E0xqS
eyKmL83xf+K5JblJWf4CkeNEeN7/+QY4JtdeIQMyOQKBgQDmwsWF1LZTCW893vh2
+EUrveGjNrdUN4hNV8KgrzpTYw58a/UfBAYR3pLgqCTJbSa4U4CRHvC+MjcNhxeb
h+fi9szizOymo8TwwUzDE1LifHAV5fkAZGFz/g/MyS7axnUtrgQx6tyBwJ5V4vV0
uG+M0HGGoYKzUjEVDZTFCEsS6wKBgQDbFDRDI4q5qaVWjdiWmqDnJXe6C+3hDHlB
JLFpHGwO0cIe/sJ1bWfclZk4E4C4pC0eviyuZ4R0YmT+ISyiwVLcXH4aKbn66Ltg
/3XN90PHp0tZidFHYTXtzLeNY2yMBIjboBi2o4mcCg1hBiIKNLXCLug/YL9dl31z
U1SqcGjMNwKBgG8P0u0kgVPZuJZ4l/D6cKAq1Uwua3G3AHzo/h1D+LhldnVfqCvz
TdCP5PUHOB1R0U7psXknAQspM+Ho4O3ULUDJM7b8lfFl5MVS41UIGd4zseZ4Nq1/
on+nCYewVEKrPX5swEweE17Hi+0ePLCei+Gj+N+pIDSaHFFbpfxmj2tdAoGAbQXi
9D5tvPNlqmswi9IrnJwStu1U1hgFB5whBbP1OnK8bfxN/W4Sr71q4HMkLb7WDWSK
i8hMLDcDF0yfD+exOqR0xMRbHzhOd3jpwTP58ROZ9dcV5LXFxq+H8L63t/5RtSo4
4jsEMjj2a4BH1Fhi013Qiim1UfgfoBKqIZ+LJ4UCgYA8FSaeBLRtjnwslkEXlYUP
C3VtXo+6c/ZVAhluUQHm/HzfDV2w8CkUrh+ZX7Tll+j1z54dlOOadU0cUXAWv909
mNkMgK6yDTbdE2vlaiHD/d0LZcgMiKubAfEl6+7m8OJ5JsFvR/dz7SlshKErPoCs
cDmoHOgqqtDt/nRvxVX+5w==
-----END PRIVATE KEY-----
`
