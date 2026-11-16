package proxy

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/icefairy/xuanji/internal/config"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// 时间线注入：让模型对「两条消息之间过了多久」有感知。
//
// # 为什么不能直接给历史消息打时间戳
//
// 网关是无状态的，客户端的 history 在客户端本地。网关这次给最后一条消息加的时间戳，
// 客户端不会写回本地历史；下次客户端带着原样 history 再发请求时，那条消息又是没时间的。
// 所以"给每条历史消息补时间"在网关层根本做不到。
//
// # 网关能做的是什么
//
// 记住「上一次请求是什么时候」，在本次请求的最后一条 user 消息前附上：
//
//	【时间】2026-09-10 14:05 周四（Asia/Shanghai）
//	【距上轮】3 小时 22 分钟
//
// 只改最后一条消息是关键：历史消息保持字节稳定，上游前缀缓存不受影响
// （OpenAI/Anthropic 的前缀缓存逐 token 匹配，改历史等于让缓存全部失效）。
//
// # 近似 session 识别（双指纹）
//
// 网关没有真正的 session id，用请求体里的消息序列做指纹：
//
//	chainKey = hash(APIKey + 全部消息去掉最后一条)
//	tailKey  = hash(最后一条消息完整内容)
//
// - chainKey 命中 → 是同一个会话的历史前缀
// - tailKey 也命中 → 同一条消息被重发（重试/双击），不算新一轮，不显示"距上轮 0 分钟"
// - tailKey 没命中 → 新一轮，间隔 = 本次时间 - 该 chainKey 上次请求时间
//
// 用户明确要求把最后一条消息也纳入指纹，这样两个内容相同的会话不会被误判为一个。
//
// # 已知边界
//
//   - 客户端编辑/删除早期历史 → chainKey 变化 → 判为新会话，丢一次「距上轮」（不出错）
//   - 不同客户端持有完全相同的 history → 会共享 interval 记录（实际极少）
//   - 网关重启不影响（会话已持久化到 SQLite）

// 会话记录的默认参数（可被系统设置覆盖）。
const (
	// defaultMinGapMinutes 是「距上轮」显示阈值：低于该值不显示，避免刷屏噪音。
	defaultMinGapMinutes = 5
	// maxFingerprintMessages 是参与指纹的消息条数上限（防止超长 history 每次算几百 KB）。
	// 取「首 4 条 + 末 6 条」代表会话：首部锁定会话身份，末部锁定轮次位置。
	//
	// 末部取 6 而不是 3：agent 类客户端一轮里会插入多条 tool_call / tool_result，
	// 增量常超过 3 条；窗口太窄会让前后两轮的“末几条”毫无交集，
	// 从而每一轮都被当成新会话（表现为「距上轮」永不显示）。
	maxFingerprintMessages = 10
	// maxCallbackCuts 是 callbackKeys 的回退深度：截掉尾部 1..N 条去匹配上一轮前缀。
	// 需覆盖“一轮增量消息数”的典型值（agent 多步工具调用可达 10 条上下）。
	maxCallbackCuts = 12
	// maxFingerprintBytes 是参与指纹的单个消息内容字节上限，超出截断。
	maxFingerprintBytes = 2048
)

// defaultTimeLineTemplate 是默认注入模板。
const defaultTimeLineTemplate = "【时间】{{datetime}} {{weekday}}（{{tz}}）"

// weekdayCN 是 Go Weekday 到中文星期的映射（周日=0）。
var weekdayCN = [...]string{"周日", "周一", "周二", "周三", "周四", "周五", "周六"}

// TimeContextEntry 是一个近似会话的状态。
type TimeContextEntry struct {
	TailKey  string    // 上次请求最后一条消息的指纹
	LastSeen time.Time // 上次请求时间
}

// TimeContextStore 是近似会话表：LRU + 过期 + 异步持久化。
//
// 为什么带持久化：会话跨网关重启要保留「距上轮」（用户明确要求 30 天）。
// 内存是热路径只读的缓存，写库在后台合并批量落盘，不阻塞转发。
type TimeContextStore struct {
	mu       sync.Mutex
	capacity int
	ttl      time.Duration
	ll       *list.List               // 最近使用在队首
	items    map[string]*list.Element // chainKey → element{*tcItem}
	persist  func(entries map[string]TimeContextEntry, removals []string)
	dirty    map[string]TimeContextEntry
	removed  map[string]struct{}
}

type tcItem struct {
	key string
	ent TimeContextEntry
}

// NewTimeContextStore 创建会话表。capacity<=0 表示不启用（Len 恒为 0）。
// persist 为 nil 时纯内存。
func NewTimeContextStore(capacity int) *TimeContextStore {
	if capacity < 0 {
		capacity = 0
	}
	return &TimeContextStore{
		capacity: capacity,
		ll:       list.New(),
		items:    make(map[string]*list.Element),
		dirty:    make(map[string]TimeContextEntry),
		removed:  make(map[string]struct{}),
	}
}

// SetPersist 注入持久化回调（后台批量写库）。
func (s *TimeContextStore) SetPersist(fn func(entries map[string]TimeContextEntry, removals []string)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.persist = fn
}

// Len 返回当前会话数。
func (s *TimeContextStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.items)
}

// Keys 返回当前所有会话键（测试与诊断用，不保证顺序）。
func (s *TimeContextStore) Keys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.items))
	for k := range s.items {
		out = append(out, k)
	}
	return out
}

// Get 查会话（未命中返回 false）。
func (s *TimeContextStore) Get(key string) (TimeContextEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	el, ok := s.items[key]
	if !ok {
		return TimeContextEntry{}, false
	}
	s.ll.MoveToFront(el)
	return el.Value.(*tcItem).ent, true
}

// Touch 写入/更新会话（同时刷新 LRU 位置并标记为待持久化）。
func (s *TimeContextStore) Touch(key, tailKey string, at time.Time, ttl time.Duration) {
	if s.capacity <= 0 || key == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ent := TimeContextEntry{TailKey: tailKey, LastSeen: at}
	if el, ok := s.items[key]; ok {
		item := el.Value.(*tcItem)
		item.ent = ent
		s.ll.MoveToFront(el)
	} else {
		el = s.ll.PushFront(&tcItem{key: key, ent: ent})
		s.items[key] = el
		for s.ll.Len() > s.capacity {
			s.evictOldest()
		}
	}
	if s.ttl > 0 {
		s.pruneLocked(at, s.ttl)
	}
	s.dirty[key] = ent
	delete(s.removed, key)
}

// Load 从持久化层预载记录（启动时调用；不标记 dirty）。
func (s *TimeContextStore) Load(entries map[string]TimeContextEntry) {
	if s.capacity <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, ent := range entries {
		if _, ok := s.items[key]; ok {
			continue
		}
		s.items[key] = s.ll.PushBack(&tcItem{key: key, ent: ent})
		for s.ll.Len() > s.capacity {
			s.evictOldest()
		}
	}
}

// Prune 删除 LastSeen 早于 now-ttl 的会话。
func (s *TimeContextStore) Prune(now time.Time, ttl time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(now, ttl)
}

// pruneLocked 清理过期项（调用方持锁）。ttl<=0 表示不清理。
func (s *TimeContextStore) pruneLocked(now time.Time, ttl time.Duration) {
	if ttl <= 0 {
		return
	}
	cutoff := now.Add(-ttl)
	for el := s.ll.Back(); el != nil; {
		prev := el.Prev()
		item := el.Value.(*tcItem)
		if item.ent.LastSeen.Before(cutoff) {
			s.removeElement(el)
		}
		el = prev
	}
}

// evictOldest 淘汰最久未使用的会话（调用方持锁）。
func (s *TimeContextStore) evictOldest() {
	if el := s.ll.Back(); el != nil {
		s.removeElement(el)
	}
}

// removeElement 摘除一项并登记删除（调用方持锁）。
func (s *TimeContextStore) removeElement(el *list.Element) {
	item := el.Value.(*tcItem)
	s.ll.Remove(el)
	delete(s.items, item.key)
	delete(s.dirty, item.key)
	s.removed[item.key] = struct{}{}
}

// FlushWith 把累积的变更交给 fn（无变更时不调用）。
// 在锁外执行回调：写库较慢，不能阻塞转发路径。
func (s *TimeContextStore) FlushWith(fn func(dirty map[string]TimeContextEntry, removals []string)) {
	if fn == nil {
		return
	}
	s.mu.Lock()
	if len(s.dirty) == 0 && len(s.removed) == 0 {
		s.mu.Unlock()
		return
	}
	dirty := s.dirty
	removed := s.removed
	s.dirty = make(map[string]TimeContextEntry, len(dirty))
	s.removed = make(map[string]struct{}, len(removed))
	s.mu.Unlock()

	rm := make([]string, 0, len(removed))
	for k := range removed {
		rm = append(rm, k)
	}
	fn(dirty, rm)
}

// Flush 把累积的变更交给 SetPersist 注入的回调（兼容入口）。
func (s *TimeContextStore) Flush() {
	s.mu.Lock()
	fn := s.persist
	s.mu.Unlock()
	if fn == nil {
		return
	}
	s.FlushWith(fn)
}

// 编译期断言：避免误删哈希工具。
var _ = sha256.Sum256

// ─────────────────────────── 注入主流程 ───────────────────────────

// InjectTimeLine 给请求体最后一条 user 消息注入时间线。
//
// cfg 为 nil 或未启用、store 为 nil 时原样返回（零开销）。
// apiKey 参与指纹，隔离不同下游客户端的会话。
// now 由调用方传入便于测试；生产传 time.Now()。
func InjectTimeLine(body []byte, cfg *config.TimeContextConfig, apiKey string, store *TimeContextStore, now time.Time) ([]byte, bool) {
	if cfg == nil || !cfg.Enabled || len(body) == 0 || !gjson.ValidBytes(body) {
		return body, false
	}
	msgs := gjson.GetBytes(body, "messages")
	if !msgs.IsArray() {
		return body, false
	}
	list := msgs.Array()
	if len(list) == 0 {
		return body, false
	}
	// 最后一条 user 消息的下标（跳过 assistant/system 尾随）。
	idx := -1
	for i := len(list) - 1; i >= 0; i-- {
		if list[i].Get("role").String() == "user" {
			idx = i
			break
		}
	}
	if idx < 0 {
		return body, false
	}
	// tool 结果回填轮次：同一次推理的延续，不注入（也不更新会话时间，
	// 否则 agent 反复调工具会把"上次对话"时间不断刷新，真实的用户间隔被吃掉）。
	if isToolResultTurn(list[idx]) {
		return body, false
	}

	chainKey, tailKey := fingerprint(apiKey, list)

	// 查会话：优先直揢 chainKey（同 history 重发），否则用 callbackKeys
	// （本轮可能对应的「上一轮」指纹）逐一查找。
	//
	// 为什么需要多个候选：客户端每轮会往 history 里追加 assistant + 新 user，
	// 上一轮请求的指纹与本轮的任何单一前缀都不一定相等（比如第一轮只有 1 条
	// 消息，第二轮 history 是 [user, assistant, user] 三条），只能把"截掉尾部的
	// 若干中可能都算一遍。只算 3 个尾部长度，开销可忽。
	var gap time.Duration
	haveGap := false
	if store != nil {
		ent, ok := store.Get(chainKey)
		if !ok {
			for _, k := range callbackKeys(apiKey, list) {
				if ent, ok = store.Get(k); ok {
					break
				}
			}
		}
		if ok && ent.TailKey != tailKey {
			// tailKey 不同 = 新一轮；相同 = 同一条消息重发，不算间隔
			gap = now.Sub(ent.LastSeen)
			haveGap = gap > 0
		}
		store.Touch(chainKey, tailKey, now, cfg.SessionTTL())
	}

	block := renderTimeLine(cfg, now, gap, haveGap)
	if block == "" {
		return body, false
	}
	return prependToMessage(body, idx, list[idx].Get("content"), block)
}

// fingerprint 计算 (chainKey, tailKey)。
//
// chainKey 覆盖「APIKey + 全部消息（含最后一条）的 role+content」。
// 包含最后一条是关键：网关没有真正的 session id，只能靠历史前缀识别会话。
// 若把最后一条排除在 chainKey 外，单轮会话的第二轮（history 多了 assistant 回复）
// 就找不到上次记录，间隔永远算不出来。
//
// tailKey 是最后一条消息的单独指纹，用来区分「同一个 history 前缀下的不同轮次」：
//   - tailKey 相同 → 同一条消息被重发（重试/双击），不算新一轮
//   - tailKey 不同 → 新一轮
func fingerprint(apiKey string, list []gjson.Result) (chainKey, tailKey string) {
	chainKey = hashSequence(apiKey, list, len(list))
	th := sha256.New()
	writeMsgToHash(th, list[len(list)-1])
	tailKey = hex.EncodeToString(th.Sum(nil))[:32]
	return chainKey, tailKey
}

// callbackKeys 返回本轮请求可能对应的「上一轮指纹」候选，按优先级排序。
//
// 客户端每轮都会往 history 里追加 assistant 回复与新的 user 消息，所以本轮序列
// 与上一轮序列的公共部分是「本轮截掉若干条尾部」。上一轮实际有几条消息不知道，
// 所以从截 1 条开始向前试几个（最多 3 个，兼顾命中率与开销）。
//
// 为什么不直接用固定 offset：第一轮可能只有 1 条消息，而第二轮 history 是 3 条，
// 二者不是简单的一前一后关系，必须逐个候选试。
func callbackKeys(apiKey string, list []gjson.Result) []string {
	n := len(list)
	if n <= 1 {
		return nil
	}
	out := make([]string, 0, 3)
	for cut := 1; cut <= maxCallbackCuts && n-cut >= 1; cut++ {
		out = append(out, hashSequence(apiKey, list, n-cut))
	}
	return out
}

// hashSequence 对 list[0:n] 生成指纹（n 为参与计算的消息条数）。
//
// 三处调用（chainKey / parentKey）必须共用本函数，否则哈希输入不一致，
// parentKey 永远对不上上一轮的 chainKey——会话接续就会静默失效。
//
// ⚠ 采样策略必须只依赖「前缀长度 n」，不能依赖总长：否则同一前缀在不同长度的
// 序列里会 hash 出不同结果，“上一轮的 chainKey == 本轮的 parentKey”就成立不了。
func hashSequence(apiKey string, list []gjson.Result, n int) string {
	h := sha256.New()
	h.Write([]byte("v2|"))
	h.Write([]byte(apiKey))
	h.Write([]byte{'|'})
	for i := 0; i < n; i++ {
		if !shouldHashMessage(i, n) {
			continue
		}
		writeMsgToHash(h, list[i])
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// shouldHashMessage 判断序列前 n 条中的第 i 条是否参与指纹。
//
// ⚠ 只能依赖 (i, n)，不能依赖调用方序列的总长：chainKey 传当前总长，
// parentKey 传「总长-1」，两者算同一个前缀时结果必须一致。
// 策略：n 较小时全量；n 较大时取「首 4 + 末 3」，兼顾会话身份与计算开销。
func shouldHashMessage(i, n int) bool {
	if n <= maxFingerprintMessages {
		return true
	}
	if i < 4 {
		return true
	}
	return i >= n-6
}

// writeMsgToHash 把一条消息的 role + content 规范化后写进 hash。
func writeMsgToHash(h interface{ Write([]byte) (int, error) }, m gjson.Result) {
	h.Write([]byte(m.Get("role").String()))
	h.Write([]byte{0x1f})
	c := m.Get("content")
	switch {
	case c.Type == gjson.String:
		s := c.String()
		if len(s) > maxFingerprintBytes {
			s = s[:maxFingerprintBytes]
		}
		h.Write([]byte(s))
	case c.IsArray():
		// 多模态：只取 text 块，图片 URL 可能很长且对识别会话帮助不大
		for _, b := range c.Array() {
			if b.Get("type").String() == "text" {
				s := b.Get("text").String()
				if len(s) > maxFingerprintBytes {
					s = s[:maxFingerprintBytes]
				}
				h.Write([]byte(s))
			}
		}
	default:
		h.Write([]byte(c.Raw))
	}
	h.Write([]byte{0x1e})
}

// isToolResultTurn 判断一条 user 消息是否其实是工具结果回填。
// OpenAI 协议用独立 role=tool（不在 user 候选里），Anthropic 折进 user 的
// 是 content 数组里的 tool_result 块。
func isToolResultTurn(msg gjson.Result) bool {
	content := msg.Get("content")
	if !content.IsArray() {
		return false
	}
	for _, b := range content.Array() {
		switch b.Get("type").String() {
		case "tool_result", "tool_use":
			return true
		}
	}
	return false
}

// renderTimeLine 渲染注入块。间隔低于阈值时只输出时间行。
// 返回空串表示不注入。
func renderTimeLine(cfg *config.TimeContextConfig, now time.Time, gap time.Duration, haveGap bool) string {
	tz := cfg.TZ
	loc, err := time.LoadLocation(tz)
	if err != nil || tz == "" {
		if err != nil {
			slog.Warn("time_context 时区非法，回退 UTC", "tz", tz, "error", err)
		}
		loc = time.UTC
		tz = "UTC"
	}
	t := now.In(loc)
	layout := strftimeToGoLayout(cfg.Format)
	if strings.TrimSpace(cfg.Format) == "" {
		layout = "2006-01-02 15:04"
	}
	tpl := cfg.Template
	if strings.TrimSpace(tpl) == "" {
		tpl = defaultTimeLineTemplate
	}
	line := tpl
	line = strings.ReplaceAll(line, "{{datetime}}", t.Format(layout))
	line = strings.ReplaceAll(line, "{{weekday}}", weekdayCN[int(t.Weekday())])
	line = strings.ReplaceAll(line, "{{tz}}", tz)

	if cfg.HideGap || !haveGap || gap < cfg.MinGap() {
		return line
	}
	return line + "\n" + "【距上轮】" + FormatGap(gap)
}

// FormatGap 把间隔渲染成人能读的中文文本。
func FormatGap(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	mins := int(d.Minutes())
	if mins < 1 {
		mins = 1 // 已过阈值时不足 1 分钟按 1 分钟显示
	}
	days := mins / (24 * 60)
	hours := (mins % (24 * 60)) / 60
	mins = mins % 60

	var parts []string
	if days > 0 {
		parts = append(parts, fmt.Sprintf("%d 天", days))
	}
	if hours > 0 {
		parts = append(parts, fmt.Sprintf("%d 小时", hours))
	}
	if mins > 0 && days == 0 { // 超过 1 天时省略分钟，读数更清爽
		parts = append(parts, fmt.Sprintf("%d 分钟", mins))
	}
	if len(parts) == 0 {
		return "不到 1 分钟"
	}
	return strings.Join(parts, " ")
}

// prependToMessage 把注入块拼到第 idx 条消息的正文前。
// content 是字符串时直接前拼；是数组时拼进首个 text 块（无 text 块则补一个）。
func prependToMessage(body []byte, idx int, content gjson.Result, block string) ([]byte, bool) {
	path := "messages." + itoa(idx) + ".content"
	switch {
	case content.Type == gjson.String:
		nb, err := sjson.SetBytes(body, path, block+"\n"+content.String())
		if err != nil {
			return body, false
		}
		return nb, true
	case content.IsArray():
		blocks := content.Array()
		for bi, b := range blocks {
			if b.Get("type").String() != "text" {
				continue
			}
			nb, err := sjson.SetBytes(body, path+"."+itoa(bi)+".text", block+"\n"+b.Get("text").String())
			if err != nil {
				return body, false
			}
			return nb, true
		}
		// 没有 text 块：先拼新数组再整体写回
		// （sjson.Set 到已有下标是覆盖而非插入，直接写 .0 会吞掉首块）
		arr := make([]interface{}, 0, len(blocks)+1)
		arr = append(arr, map[string]interface{}{"type": "text", "text": block})
		for _, b := range blocks {
			arr = append(arr, json.RawMessage(b.Raw))
		}
		nb, err := sjson.SetBytes(body, path, arr)
		if err != nil {
			return body, false
		}
		return nb, true
	default:
		return body, false
	}
}

// itoa 是 strconv.Itoa 的零依赖局部实现（热路径小整数）。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// strftimeToGoLayout 把 strftime 占位符替换成 Go 参考时间片段。
func strftimeToGoLayout(f string) string {
	return strings.NewReplacer(
		"%Y", "2006", "%m", "01", "%d", "02",
		"%H", "15", "%M", "04", "%S", "05", "%p", "PM", "%%", "%",
	).Replace(f)
}

// DefaultTimeLineTemplate 是管理页「默认模板」展示用。
const DefaultTimeLineTemplate = defaultTimeLineTemplate

// DefaultMinGapMinutes 供系统设置默认值引用。
const DefaultMinGapMinutes = defaultMinGapMinutes
