// Package piheaders 定义 pi agent 的真实上游请求头指纹。
//
// 用途：璇玑网关转发到上游时，把出站请求头伪装成 pi agent 的形态，
// 避免被上游按「Go 聚合网关」特征（one-api / new-api 均为 Go 实现）识别。
//
// 数据来源（实测，2026-09-16）：用本地 mock 上游抓取真实 pi 0.85.1 的
// 出站请求头集合与顺序，逐字段记录。pi 自身的 UA 模板见其发行包
// dist/utils/pi-user-agent.js：
//
//	pi/${version} (${platform}; ${runtime}; ${arch})   runtime = node/${process.version}
//
// 与之对应的真实观测值为 "pi/0.85.1 (linux; node/v22.22.1; x64)"（与璇玑请求日志中
// 真实 pi 客户端记录一致）。
//
// 局限（务必知悉）：只对齐「请求头」。请求体（系统提示词、工具定义、体积与用量形态）
// 不在范围内，也无法用头部掩盖。作用是「降低显眼度」，不是「隐形」。
package piheaders

import "strings"

// Order 是 pi 0.85.1 发往上游的头部顺序（逐字段实测）。
//
// 其中的 host / connection / content-length 由 Go 的 net/http 自行管理
// （Host 与 User-Agent 特殊处理，Content-Length 由 body 推导），
// 因此它们不出现在 Default 中，但仍需在此登记以保证排序名次与 pi 一致。
var Order = []string{
	"host",
	"connection",
	"Accept",
	"X-Stainless-Retry-Count",
	"X-Stainless-Timeout",
	"X-Stainless-Lang",
	"X-Stainless-Package-Version",
	"X-Stainless-OS",
	"X-Stainless-Arch",
	"X-Stainless-Runtime",
	"X-Stainless-Runtime-Version",
	"authorization",
	"User-Agent",
	"content-type",
	"accept-language",
	"sec-fetch-mode",
	"accept-encoding",
	"content-length",
}

// Default 是需要补齐的固定头部（值按实测固定，不随机器变化）。
//
// 不含以下三类：
//   - host / connection / content-length：由 Go net/http 管理；
//   - User-Agent / authorization / content-type：由调用方按上游设置；
//   - accept-encoding：默认不发送（Go 的透明 gzip 依赖它为空，见 config 包说明）。
var Default = map[string]string{
	"Accept":                      "application/json",
	"X-Stainless-Retry-Count":     "0",
	"X-Stainless-Timeout":         "300",
	"X-Stainless-Lang":            "js",
	"X-Stainless-Package-Version": "6.40.0",
	"X-Stainless-OS":              "Linux",
	"X-Stainless-Arch":            "x64",
	"X-Stainless-Runtime":         "node",
	"X-Stainless-Runtime-Version": "v22.22.1",
	"accept-language":             "*",
	"sec-fetch-mode":              "cors",
}

// UserAgent 是 pi 的真实 User-Agent 形态（固定值）。
// 与 pi 发行包的 getPiUserAgent 模板一致：pi/<version> (<platform>; node/<nodeVer>; <arch>)
const UserAgent = "pi/0.85.1 (linux; node/v22.22.1; x64)"

// AcceptEncoding 是 pi 实际发送的值；仅在开启 gzip 透明解压时使用。
const AcceptEncoding = "gzip, deflate"

// Rank 返回头部 name 在 pi 顺序中的名次（大小写不敏感）。
// 未登记的头部返回 len(Order)，即排在已知头部之后。
func Rank(name string) int {
	for i, h := range Order {
		if strings.EqualFold(h, name) {
			return i
		}
	}
	return len(Order)
}

// IsManaged 判断该头部是否由 Go net/http 管理，指纹注入时应跳过。
func IsManaged(name string) bool {
	switch strings.ToLower(name) {
	case "host", "connection", "content-length", "user-agent", "authorization", "content-type":
		return true
	}
	return false
}
