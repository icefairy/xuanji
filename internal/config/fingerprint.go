package config

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/icefairy/xuanji/internal/config/piheaders"
)

// ===== pi 指纹：请求头集合 + 顺序伪装 =====
//
// 目的：璇玑是 Go 单二进制网关，而 one-api / new-api 等聚合网关同为 Go 实现，
// 上游可据此识别「这是聚合网关」。本文件把出站请求伪装成 pi agent 形态，
// 降低被按 Go 网关特征识别的显眼度。
//
// 两个层面：
//  1. 头部「集合」：ApplyPiFingerprintHeaders 补齐 pi 的固定头（缺什么补什么，
//     不覆盖调用方已设的值）。调用点与 ApplyUpstreamUserAgent 相同。
//  2. 头部「顺序」：Go 的 net/http 写入时会对头部字典序排序（仅有 Host /
//     User-Agent / Content-Length 三个特判位置），无法在 Request 层控制。
//     因此在 NewForwardTransport 的 DialTLSContext 返回的连接上做一次
//     「头部块收齐 → 按 pi 顺序重排 → 立刻写出」的改写（piFingerprintConn）。
//
// 不改动的内容：请求行、body、以及 host/connection/content-length 的位置
// （它们本就由 net/http 管理，且与 pi 实测顺序一致）。
//
// 局限：只对齐头部。请求体（系统提示词 / 工具定义 / 体积 / 用量形态）不在范围内，
// 作用仅为「降低显眼度」，不是「隐形」。

// DefaultUpstreamUserAgent 是转发时默认的 User-Agent。
// 取值为 pi 0.85.1 的真实 UA（与 pi 发行包 getPiUserAgent 模板一致，且与璇玑
// 请求日志中真实 pi 客户端的记录逐字相符）。
const DefaultUpstreamUserAgent = piheaders.UserAgent

var upstreamUserAgent atomic.Value // string；空 = 不设置（用 Go 默认 Go-http-client/1.1）

// SetUpstreamUserAgent 设置全局上游 UA（空串 = 不设置）。
func SetUpstreamUserAgent(ua string) { upstreamUserAgent.Store(ua) }

// UpstreamUserAgent 返回当前全局上游 UA。
func UpstreamUserAgent() string {
	if v, ok := upstreamUserAgent.Load().(string); ok {
		return v
	}
	return ""
}

// ApplyUpstreamUserAgent 是「上游伪装」的统一注入点：补齐 pi 指纹头（集合层面）
// 并设置 User-Agent；指纹头的「顺序」层面由 NewForwardTransport 的连接层重排完成。
//
// 之所以合并到一处：7 个转发调用点（proxy / anthropic / gemini / ollama）都只调它，
// 统一在此注入可避免遗漏，也不必改动各 handler 的签名。
func ApplyUpstreamUserAgent(req *http.Request) {
	ApplyPiFingerprintHeaders(req)
	if ua := UpstreamUserAgent(); ua != "" {
		req.Header.Set("User-Agent", ua)
	}
}

// piGzip 控制是否发送 pi 的 accept-encoding: gzip, deflate。
//
// 默认关闭，因为 Go 的「透明 gzip」解压要求请求未显式设置 Accept-Encoding
// （见 NewForwardTransport 说明）。开启时必须同时用 NewClientWithPiFingerprint
// 包装 client，否则响应体将是压缩字节而无法解析。
var piGzip atomic.Bool

// SetPiGzip 设置是否发送 accept-encoding 并启用解压链路。
func SetPiGzip(on bool) { piGzip.Store(on) }

// PiGzip 返回当前是否开启 accept-encoding 伪装。
func PiGzip() bool { return piGzip.Load() }

// piConflictingHeaders 是需要剥离的「他者身份」头，避免与 pi 伪装自相矛盾。
var piConflictingHeaders = []string{
	"X-Product", "X-IDE-Name", "X-IDE-Type", "X-IDE-Version",
	"X-Private-Data", "X-Conversation-Id", "X-Requested-With", "Traceparent",
	"X-Trace-Id", "B3", "X-B3-Traceid", "X-B3-Spanid", "X-B3-Parentspanid", "X-B3-Sampled",
}

// ApplyPiFingerprintHeaders 把 pi 的固定请求头补进 req。
//
// 规则：只填补缺失的头，不覆盖调用方已设置的值（authorization / content-type /
// User-Agent 等由上游配置与 ApplyUpstreamUserAgent 决定）；并剥离他者身份头。
func ApplyPiFingerprintHeaders(req *http.Request) {
	if req == nil {
		return
	}
	for _, k := range piConflictingHeaders {
		req.Header.Del(k)
	}
	for k, v := range piheaders.Default {
		if req.Header.Get(k) == "" {
			req.Header.Set(k, v)
		}
	}
	if piGzip.Load() && req.Header.Get("accept-encoding") == "" {
		req.Header.Set("accept-encoding", piheaders.AcceptEncoding)
	}
}

// ===== 头部顺序重排（连接层） =====

// piFingerprintConn 包装一条已是 TLS 的 net.Conn，在首个 HTTP 请求的头部块
// 写满时，把头部按 pi 顺序重排后写出；此后透传。
//
// 为什么必须"收齐再写"：Go 的 Transport 是流式写出（writeSubset 逐个头写），
// 顺序由它决定；只有在连接层拦下整块头部自行改写，才能改变线上字节顺序。
type piFingerprintConn struct {
	net.Conn
	mu      sync.Mutex
	buf     bytes.Buffer
	scanned int  // buf 中已扫描到的位置（避免重复扫描）
	done    bool // 头部已处理（已重排或已放弃），后续写入透传
}

// maxHeaderBuffer 是等待头部收齐的缓冲上限。超出即放弃重排并原样透传，
// 避免异常情况下的无界缓冲。
const maxHeaderBuffer = 1 << 20

func newPiFingerprintConn(c net.Conn) *piFingerprintConn {
	return &piFingerprintConn{Conn: c}
}

func (c *piFingerprintConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.done {
		return c.Conn.Write(p)
	}
	c.buf.Write(p)
	if c.buf.Len() > maxHeaderBuffer {
		c.done = true
		_, err := c.Conn.Write(c.buf.Bytes())
		c.buf.Reset()
		return len(p), err
	}
	raw := c.buf.Bytes()
	end := findHeaderEnd(raw, c.scanned)
	if end < 0 {
		if n := len(raw) - 3; n > c.scanned {
			c.scanned = n
		}
		return len(p), nil // 头部未收齐，先缓冲，不写出
	}
	head := raw[:end]
	rest := raw[end+4:]
	out := reorderHeaderBlock(head)
	out = append(out, rest...)
	c.done = true
	c.buf.Reset()
	_, err := c.Conn.Write(out)
	return len(p), err
}

// hasHeaderNamed 判断头部列表中是否已存在指定名（大小写不敏感）。
func hasHeaderNamed(hdrs []headerLine, name string) bool {
	for _, h := range hdrs {
		if strings.EqualFold(h.name, name) {
			return true
		}
	}
	return false
}

// findHeaderEnd 返回 raw 中头部结束标记（\r\n\r\n）的起始下标，未找到返回 -1。
// from 是本次扫描的起点（此前已确认无匹配，用于避免 O(n²) 重复扫描）。
func findHeaderEnd(raw []byte, from int) int {
	if from < 0 {
		from = 0
	}
	idx := bytes.Index(raw[from:], []byte("\r\n\r\n"))
	if idx < 0 {
		return -1
	}
	return from + idx
}

// headerLine 是一条原始头部行及其名字。
type headerLine struct {
	name string
	line string
}

// reorderHeaderBlock 把头部块按 pi 顺序重排，并返回完整的块（含结尾空行）。
//
// 入参 head 是请求行与头部行、不含结尾空行；返回值始终是可直接写出的完整字节。
// 解析失败时原样还原（补回空行），保证绝不破坏请求。
func reorderHeaderBlock(head []byte) []byte {
	lines := bytes.Split(head, []byte("\r\n"))
	if len(lines) == 0 || !bytes.Contains(lines[0], []byte("HTTP/1.")) {
		return append(append([]byte{}, head...), "\r\n\r\n"...)
	}
	type kv = headerLine
	reqLine := string(lines[0])
	hdrs := make([]kv, 0, len(lines)-1)
	for _, l := range lines[1:] {
		if len(l) == 0 {
			continue
		}
		i := bytes.IndexByte(l, ':')
		if i <= 0 {
			return append(append([]byte{}, head...), "\r\n\r\n"...)
		}
		hdrs = append(hdrs, kv{name: string(l[:i]), line: string(l)})
	}
	// Go 对 HTTP/1.1 默认不发 Connection（仅关闭时写 Connection: close），
	// 而 pi 显式发送 keep-alive。此处仅在缺失时补上，保持与 pi 一致；
	// 若 Go 已写 Connection: close 则不介入，避免语义矛盾。
	if !hasHeaderNamed(hdrs, "connection") {
		hdrs = append(hdrs, kv{name: "connection", line: "connection: keep-alive"})
	}
	// 稳定排序：pi 已登记的头按名次排；未登记的头保持原相对顺序排在最后。
	rank := make(map[int]int, len(hdrs))
	for i := range hdrs {
		rank[i] = piheaders.Rank(hdrs[i].name)
	}
	idx := make([]int, len(hdrs))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool {
		return rank[idx[a]] < rank[idx[b]]
	})
	var out bytes.Buffer
	out.WriteString(reqLine)
	out.WriteString("\r\n")
	for _, i := range idx {
		out.WriteString(hdrs[i].line)
		out.WriteString("\r\n")
	}
	out.WriteString("\r\n")
	return out.Bytes()
}

// ===== 透明解压（accept-encoding 伪装的配套） =====
//
// Go 的透明 gzip 仅在请求未显式设置 Accept-Encoding 时生效。一旦我们按 pi 发送
// accept-encoding: gzip, deflate，上游可能返回压缩响应，而璇玑现有代码不做解压
// （JSON 解析、SSE 解析、token 计数、透传都直接读明文），会全部拿到压缩字节。
// 因此在 Transport 外层补一层透明解压，保持上层代码零改动。

type readCloser struct {
	r    io.Reader
	orig io.Closer
}

func (c *readCloser) Read(p []byte) (int, error) { return c.r.Read(p) }

func (c *readCloser) Close() error {
	var err error
	if rc, ok := c.r.(io.Closer); ok {
		err = rc.Close()
	}
	if c.orig != nil {
		if e := c.orig.Close(); err == nil {
			err = e
		}
	}
	return err
}

// decompressRoundTripper 对带 Content-Encoding: gzip/deflate 的响应体做透明解压。
// 解压是惰性的（真正读取时才解），以保持 SSE 流式语义。
type decompressRoundTripper struct {
	base http.RoundTripper
}

func (d *decompressRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := d.base.RoundTrip(req)
	if err != nil || resp == nil || resp.Body == nil {
		return resp, err
	}
	enc := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding")))
	if enc != "gzip" && enc != "deflate" {
		return resp, nil
	}
	// 用 bufio 预读，解压构造失败时仍能把已预读字节交还给上层，避免丢数据。
	br := bufio.NewReader(resp.Body)
	orig := resp.Body
	var dec io.ReadCloser
	if enc == "gzip" {
		if zr, zerr := gzip.NewReader(br); zerr == nil {
			dec = zr
		}
	} else {
		if dr, derr := newDeflateReader(br); derr == nil {
			dec = dr
		}
	}
	if dec == nil {
		// 解压不可用：原样透传（保留 bufio 已预读的字节）
		resp.Body = &readCloser{r: br, orig: orig}
		return resp, nil
	}
	resp.Body = &readCloser{r: dec, orig: orig}
	resp.Header.Del("Content-Encoding")
	resp.Header.Del("Content-Length")
	resp.ContentLength = -1
	resp.Uncompressed = true
	return resp, nil
}

// newDeflateReader 处理 deflate 的两种现实形态：zlib 包装（RFC1950，Node zlib 默认）
// 与裸 deflate。用前两字节判断，避免解压失败。
func newDeflateReader(r io.Reader) (io.ReadCloser, error) {
	br := bufio.NewReader(r)
	if hdr, err := br.Peek(2); err == nil && len(hdr) == 2 {
		// CM=8(deflate) 且 (CMF<<8|FLG) % 31 == 0 → zlib 包装
		if hdr[0]&0x0f == 8 && (int(hdr[0])<<8|int(hdr[1]))%31 == 0 {
			return zlib.NewReader(br)
		}
	}
	return flate.NewReader(br), nil
}

// NewClientWithPiFingerprint 用转发 Transport 构造带 pi 伪装的 HTTP client。
// 与 NewForwardTransport 配对使用，确保 accept-encoding 开启时响应被透明解压。
func NewClientWithPiFingerprint(tr *http.Transport) *http.Client {
	return &http.Client{Transport: &decompressRoundTripper{base: tr}}
}

// piTLSConfig 构造 pi 指纹链路使用的 TLS 配置。
//
// base 是调用方在 Transport 上设置的 TLSClientConfig（可能为 nil）：本函数克隆它，
// 保持调用方的自定义（如 InsecureSkipVerify、证书池）生效，避免自建配置造成行为回归。
// ServerName 缺省取目标 host；NextProtos 缺省为 http/1.1 —— pi 实测协商的正是
// http/1.1（非 h2），显式声明可确保两端一致，同时不影响连接复用。
func piTLSConfig(base *tls.Config, serverName string) *tls.Config {
	var cfg *tls.Config
	if base != nil {
		cfg = base.Clone()
	} else {
		cfg = &tls.Config{}
	}
	if cfg.ServerName == "" {
		cfg.ServerName = serverName
	}
	if len(cfg.NextProtos) == 0 {
		cfg.NextProtos = []string{"http/1.1"}
	}
	return cfg
}

// dialTLSWithPiFingerprint 建立 TLS 连接并套上头部顺序重排。
func dialTLSWithPiFingerprint(ctx context.Context, network, addr string, dialTimeout time.Duration, base *tls.Config) (net.Conn, error) {
	d := &net.Dialer{Timeout: dialTimeout}
	raw, err := d.DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	host, _, splitErr := net.SplitHostPort(addr)
	if splitErr != nil {
		host = addr
	}
	tlsConn := tls.Client(raw, piTLSConfig(base, host))
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		raw.Close()
		return nil, err
	}
	return newPiFingerprintConn(tlsConn), nil
}
