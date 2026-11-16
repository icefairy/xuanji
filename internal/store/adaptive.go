package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// AdaptiveWeight 是 adaptive_weights 表的行映射：按真实请求成功率算出的上游惩罚系数。
//
// 设计约束（见 internal/proxy/adaptive.go）：
//   - 只存「惩罚系数」，upstreams.weight 永远是用户手设的原值（有效权重 = Weight * Factor）；
//   - 主键 (Upstream, Model)，Model 为客户端模型名，空串表示该上游所有模型；
//   - 不参与路由热路径查询——启动时加载进内存，后台 ticker 周期刷新并回写。
type AdaptiveWeight struct {
	Upstream    string  `json:"upstream"`
	Model       string  `json:"model"`
	Factor      float64 `json:"factor"`       // 惩罚系数 0.0~1.0，1.0=无惩罚
	Rate        float64 `json:"rate"`         // 最近一次判定时的成功率快照
	Samples     int     `json:"samples"`      // 样本数
	WindowHours int     `json:"window_hours"` // 统计窗口小时数
	ProbeAt     string  `json:"probe_at"`     // 下次可试探时间（RFC3339）
	// ProbeStart 是本次试探观察期的起点（RFC3339，空串=不在试探期）。
	// 试探期判定只看这个时刻之后的流量，避免被历史 429 淹没（见 TASK-probe-fix.md）。
	ProbeStart string `json:"probe_start"`
	Backoff    int    `json:"backoff"` // 连续失败次数（指数退避）
	UpdatedAt  string `json:"updated_at"`
}

// adaptiveTimeLayout 是 adaptive_weights 表里时间列的存储格式。
// 统一用 UTC RFC3339：SQLite 字符串比较等价于时间比较，便于按 probe_at 排序/比对。
const adaptiveTimeLayout = time.RFC3339

// logTimeLayout 是 request_log.ts 的精确存储格式（Insert 用 time.RFC3339 写入，秒精度 UTC）。
//
// 统计窗口的下界必须与 ts 同格式：SQLite 里 ts 是 TEXT，`ts >= ?` 是字符串比较，
// 而 datetime('now','-5 hours') 产出的是「空格分隔、无 Z」的 '2026-11-04 10:16:37'。
// 由于 'T'(0x54) > ' '(0x20)，任意 '2026-11-04T...' 都大于它 → 窗口下界恒为真，
// 「5 小时窗口」退化成「全部历史」（实测 374 行 vs 正确的 21 行）。
const logTimeLayout = "2006-01-02T15:04:05Z"

// sqlNowOffset 拼 SQLite strftime 表达式，产出与 ts 完全一致的 'T...Z' 格式。
// modifiers 按 SQLite 语义依次生效；参数只接受内部常量（小时为整数，无注入面）。
//
//	sqlNowOffset("-5 hours")  →  strftime('%Y-%m-%dT%H:%M:%SZ','now','-5 hours')
func sqlNowOffset(modifiers ...string) string {
	expr := "strftime('%Y-%m-%dT%H:%M:%SZ','now'"
	for _, m := range modifiers {
		expr += ",'" + m + "'"
	}
	return expr + ")"
}

// AdaptiveWindowSinceSQL 返回「now 往前 hours 小时」的窗口下界表达式（hours < 0 视为 0）。
// 产出格式与 request_log.ts 一致（'2006-01-02T15:04:05Z'），可直接做字符串比较。
func AdaptiveWindowSinceSQL(hours int) string {
	if hours < 0 {
		hours = 0
	}
	return sqlNowOffset(fmt.Sprintf("-%d hours", hours))
}

// ListAdaptiveWeights 返回全部自适应权重行（启动时加载进内存）。
func (s *Store) ListAdaptiveWeights() ([]AdaptiveWeight, error) {
	rows, err := s.db.Query(
		`SELECT upstream, model, factor, rate, samples, window_hours, probe_at, probe_start, backoff, updated_at
		   FROM adaptive_weights ORDER BY upstream, model`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AdaptiveWeight
	for rows.Next() {
		var a AdaptiveWeight
		if err := rows.Scan(&a.Upstream, &a.Model, &a.Factor, &a.Rate, &a.Samples,
			&a.WindowHours, &a.ProbeAt, &a.ProbeStart, &a.Backoff, &a.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// UpsertAdaptiveWeight 写入/更新一行自适应权重（主键 upstream+model）。
func (s *Store) UpsertAdaptiveWeight(a AdaptiveWeight) error {
	if a.UpdatedAt == "" {
		a.UpdatedAt = time.Now().UTC().Format(adaptiveTimeLayout)
	}
	_, err := s.db.Exec(
		`INSERT INTO adaptive_weights (upstream, model, factor, rate, samples, window_hours, probe_at, probe_start, backoff, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(upstream, model) DO UPDATE SET
		   factor=excluded.factor, rate=excluded.rate, samples=excluded.samples,
		   window_hours=excluded.window_hours, probe_at=excluded.probe_at,
		   probe_start=excluded.probe_start,
		   backoff=excluded.backoff, updated_at=excluded.updated_at`,
		a.Upstream, a.Model, a.Factor, a.Rate, a.Samples, a.WindowHours,
		a.ProbeAt, a.ProbeStart, a.Backoff, a.UpdatedAt,
	)
	return err
}

// DeleteAdaptiveWeight 删除一行（完全恢复时调用，不留垃圾行）。
func (s *Store) DeleteAdaptiveWeight(upstream, model string) error {
	_, err := s.db.Exec(`DELETE FROM adaptive_weights WHERE upstream = ? AND model = ?`, upstream, model)
	return err
}

// AdaptiveSample 是某 (upstream, model) 在统计窗口内的成功/失败样本数。
// Total 只统计「成功 + 真失败」两类，客户端错误（400/401/403/404/422/499）已从分母排除。
type AdaptiveSample struct {
	Upstream string
	Model    string
	Total    int
	OK       int
}

// AdaptiveSamples 统计 since（RFC3339，含边界）之后每个 (upstream, model) 的成功率样本。
//
// 判定口径（需求书第二节，直接决定降权结果对错）：
//   - success = status BETWEEN 200 AND 299
//   - 真失败 = status = 0（连接失败/超时）、429（账号池耗尽/限流）、>=500（服务端故障）
//   - 客户端错误（400/401/403/404/422）与 499（客户端主动断开）从分母剔除
func (s *Store) AdaptiveSamples(since string) ([]AdaptiveSample, error) {
	return s.AdaptiveSamplesSince(since, "")
}

// goLogTime 把 Go 侧算出的时刻规整成 request_log.ts 的同格式串（'2006-01-02T15:04:05Z'）。
// 解析失败时返回空串，由调用方回退到 SQL 侧生成（见 logWindowBound）。
func goLogTime(v string) string {
	if v = strings.TrimSpace(v); v == "" {
		return ""
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t.UTC().Format(logTimeLayout)
	}
	if t, err := time.Parse("2006-01-02 15:04:05", v); err == nil {
		return t.UTC().Format(logTimeLayout)
	}
	return ""
}

// logTimeLowerBound 是三条统计 SQL 共用的「窗口下界占位符」。
//
// 下界一律取 max(Go 侧算出的 since, DB 侧当前时刻往前 hours 小时)：
// 前者的格式由 goLogTime 规整，后者用 strftime 产出与 ts 完全一致的 'T...Z' 格式，
// 二者取 MAX 保证边界永不被放大（这正是「5 小时窗口 = 全部历史」的修复点）。
const logTimeLowerBound = `max(?, strftime('%Y-%m-%dT%H:%M:%SZ','now', ?))`

// logWindowDefaultOffset 是 Go 侧没算出 since（空串/不可解析）时的回退偏移，
// 与默认统计窗口一致（5 小时）。显式传 since 的调用方不受影响。
const logWindowDefaultOffset = "-5 hours"

// windowQueryOffset 从 Go 侧的 RFC3339 串推算「now 到 since 的整小时偏移」，
// 让 Go 与 SQL 两边的窗口起底天然同步（改配置窗口不用再改这里）。
func windowQueryOffset(since string) string {
	t, err := time.Parse(time.RFC3339, since)
	if err != nil {
		return logWindowDefaultOffset
	}
	h := int(time.Since(t).Hours() + 0.5) // 四舍五入到小时；向下取整会把窗口放大
	if h <= 0 {
		return "-0 hours"
	}
	return fmt.Sprintf("-%d hours", h)
}

// logWindowArgs 规整 since，并给出 SQL 需要的 (下界, 偏移修饰符)。
//
// 规整很重要：一旦 since 被传成 '2026-11-04 10:16:37' 这种空格格式，
// 字符串比较就会把窗口放大成「全部历史」；goLogTime 统一改成 'T...Z'。
func logWindowArgs(since string) (string, string) {
	if g := goLogTime(since); g != "" {
		return g, windowQueryOffset(g)
	}
	return time.Now().UTC().Format(logTimeLayout), logWindowDefaultOffset
}

// AdaptiveSamplesSince 同 AdaptiveSamples，但额外限定 after（RFC3339，含边界，空串=不限定）。
//
// 试探期判定必须用它：after = 本次试探起点，否则窗口里的历史 429 会把
// 「试探期新流量全成功」判成失败（TASK-probe-fix.md 的核心缺陷）。
func (s *Store) AdaptiveSamplesSince(since, after string) ([]AdaptiveSample, error) {
	since, offset := logWindowArgs(since)
	q := `SELECT upstream, model,
	             COUNT(*) AS n,
	             SUM(CASE WHEN status BETWEEN 200 AND 299 THEN 1 ELSE 0 END) AS ok
	        FROM request_log
	       WHERE ts >= ` + logTimeLowerBound + `
	         AND (status BETWEEN 200 AND 299 OR status = 0 OR status = 429 OR status >= 500)`
	args := []any{since, offset}
	if after = goLogTime(after); after != "" {
		q += ` AND ts >= ?`
		args = append(args, after)
	}
	q += ` GROUP BY upstream, model`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AdaptiveSample
	for rows.Next() {
		var a AdaptiveSample
		var ok sql.NullInt64
		if err := rows.Scan(&a.Upstream, &a.Model, &a.Total, &ok); err != nil {
			return nil, err
		}
		a.OK = int(ok.Int64)
		out = append(out, a)
	}
	return out, rows.Err()
}

// AdaptiveProbeRow 是对 (upstream, model) 做故障段分析时用到的一行请求记录。
type AdaptiveProbeRow struct {
	Status  int
	AtMS    int64  // 记录时间（Unix 毫秒）
	ErrText string // error_detail，用于识别「账号池无可用账号」类额度耗尽
}

// AdaptiveProbeRows 返回 since（RFC3339）之后某 (upstream, model) 的明细，按时间升序。
// 只返回 200-299 / 429 / 0 三类——故障段判定只关心「成功」与「限流」的交替。
// limit 为 0 表示不限制。
func (s *Store) AdaptiveProbeRows(upstream, model, since string, limit int) ([]AdaptiveProbeRow, error) {
	q := `SELECT status, ts, COALESCE(error_detail, '')
	        FROM request_log
	       WHERE upstream = ? AND model = ? AND ts >= ` + logTimeLowerBound + `
	         AND (status BETWEEN 200 AND 299 OR status = 429 OR status = 0)
	       ORDER BY ts ASC, id ASC`
	if limit > 0 {
		q += ` LIMIT ` + itoa(limit)
	}
	since, offset := logWindowArgs(since)
	rows, err := s.db.Query(q, upstream, model, since, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AdaptiveProbeRow
	for rows.Next() {
		var r AdaptiveProbeRow
		var ts string
		if err := rows.Scan(&r.Status, &ts, &r.ErrText); err != nil {
			return nil, err
		}
		r.AtMS = parseLogTimeMS(ts)
		out = append(out, r)
	}
	return out, rows.Err()
}

// itoa 是 strconv.Itoa 的极小替身，避免为一个 LIMIT 引入额外语义（limit 由调用方保证 > 0）。
func itoa(n int) string {
	if n <= 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// parseLogTimeMS 解析 request_log.ts 为 Unix 毫秒。
// 历史行都是 RFC3339；解析失败返回 0（调用方按「无有效时间」跳过）。
func parseLogTimeMS(ts string) int64 {
	if t, err := time.Parse(time.RFC3339, ts); err == nil {
		return t.UnixNano() / int64(time.Millisecond)
	}
	if t, err := time.Parse("2006-01-02 15:04:05", ts); err == nil {
		return t.UnixNano() / int64(time.Millisecond)
	}
	return 0
}
