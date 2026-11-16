package proxy

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/icefairy/xuanji/internal/config"
)

// 时间线注入：近似 session 识别 + 「距上轮」计算。
//
// 设计要点（见 time_context.go 顶部注释）：
//   - chainKey = 前 N-1 条消息指纹（识别"同一个会话"）
//   - tailKey  = 第 N 条消息指纹（区分同前缀下的不同轮次）
//   - 只改最后一条 user 消息（保住上游前缀缓存）

func msg(role, content string) map[string]interface{} {
	return map[string]interface{}{"role": role, "content": content}
}

func bodyWith(t *testing.T, msgs ...map[string]interface{}) []byte {
	t.Helper()
	return mustJSON(t, map[string]interface{}{"model": "m", "messages": msgs})
}

// inOrder 按顺序拼消息（map 无序，必须用有序结构构造请求体）。
func bodyOf(t *testing.T, pairs ...string) []byte {
	t.Helper()
	var sb strings.Builder
	sb.WriteString(`{"model":"m","messages":[`)
	for i := 0; i+1 < len(pairs); i += 2 {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(`{"role":"` + pairs[i] + `","content":"` + pairs[i+1] + `"}`)
	}
	sb.WriteString(`]}`)
	return []byte(sb.String())
}

func TestTimeContext_Disabled_NoStore(t *testing.T) {
	// enabled=false 时不注入，也不碰存储
	st := NewTimeContextStore(10)
	cfg := &config.TimeContextConfig{Enabled: false}
	body := bodyOf(t, "user", "hi")
	got, changed := InjectTimeLine(body, cfg, "key1", st, time.Now())
	if changed {
		t.Fatal("未开启时不应改动")
	}
	if string(got) != string(body) {
		t.Fatal("未开启时应原样返回")
	}
	if st.Len() != 0 {
		t.Fatalf("未开启时不应写会话记录, len=%d", st.Len())
	}
}

func TestTimeContext_FirstTurn_NoGap(t *testing.T) {
	// 首次会话：只有时间，没有「距上轮」
	st := NewTimeContextStore(10)
	cfg := &config.TimeContextConfig{
		Enabled: true, TZ: "Asia/Shanghai", Format: "%Y-%m-%d %H:%M", MinGapMinutes: 5,
	}
	now := time.Date(2026, 9, 10, 14, 5, 0, 0, time.UTC)
	body := bodyOf(t, "user", "第一句")
	got, changed := InjectTimeLine(body, cfg, "key1", st, now)
	if !changed {
		t.Fatal("应注入时间")
	}
	content := firstContent(t, got)
	if !strings.Contains(content, "【时间】") {
		t.Fatalf("缺少时间行: %q", content)
	}
	if strings.Contains(content, "距上轮") {
		t.Fatalf("首轮不应有距上轮: %q", content)
	}
	if !strings.HasSuffix(content, "\n第一句") {
		t.Fatalf("原文丢失: %q", content)
	}
}

func TestTimeContext_SecondTurn_ShowsGap(t *testing.T) {
	// 第二轮：同一会话（同 chain），显示真实间隔
	st := NewTimeContextStore(10)
	cfg := &config.TimeContextConfig{
		Enabled: true, TZ: "Asia/Shanghai", Format: "%Y-%m-%d %H:%M", MinGapMinutes: 5,
	}
	t0 := time.Date(2026, 9, 10, 14, 0, 0, 0, time.UTC)
	// 第一轮：单条 user
	first := bodyOf(t, "user", "第一句")
	InjectTimeLine(first, cfg, "key1", st, t0)

	// 第二轮：history 含第一轮（原文，无时间戳——模拟客户端真实行为）+ 新的 user
	second := bodyOf(t,
		"user", "第一句",
		"assistant", "收到",
		"user", "三小时后我问",
	)
	later := t0.Add(3*time.Hour + 22*time.Minute)
	got, changed := InjectTimeLine(second, cfg, "key1", st, later)
	if !changed {
		t.Fatal("应注入")
	}
	content := lastContent(t, got)
	if !strings.Contains(content, "【时间】") {
		t.Fatalf("缺少时间行: %q", content)
	}
	if !strings.Contains(content, "距上轮") {
		t.Fatalf("第二轮应有距上轮: %q", content)
	}
	// 3 小时 22 分应渲染为可读文本
	if !strings.Contains(content, "3") || !strings.Contains(content, "小时") {
		t.Fatalf("间隔渲染不对: %q", content)
	}
	// 历史消息必须保持原样（前缀缓存）
	if c := firstContent(t, got); c != "第一句" {
		t.Fatalf("历史消息被改动: %q", c)
	}
}

func TestTimeContext_GapBelowThreshold_Hidden(t *testing.T) {
	// 间隔小于阈值不显示，避免噪音
	st := NewTimeContextStore(10)
	cfg := &config.TimeContextConfig{
		Enabled: true, TZ: "Asia/Shanghai", Format: "%Y-%m-%d %H:%M", MinGapMinutes: 30,
	}
	t0 := time.Date(2026, 9, 10, 14, 0, 0, 0, time.UTC)
	InjectTimeLine(bodyOf(t, "user", "第一句"), cfg, "k", st, t0)
	got, _ := InjectTimeLine(
		bodyOf(t, "user", "第一句", "assistant", "ok", "user", "马上又问"),
		cfg, "k", st, t0.Add(2*time.Minute),
	)
	content := lastContent(t, got)
	if strings.Contains(content, "距上轮") {
		t.Fatalf("低于阈值不应显示间隔: %q", content)
	}
}

func TestTimeContext_DifferentSession_NoGap(t *testing.T) {
	// 不同 chainKey = 不同会话，不应串味
	st := NewTimeContextStore(10)
	cfg := &config.TimeContextConfig{
		Enabled: true, TZ: "Asia/Shanghai", Format: "%Y-%m-%d %H:%M", MinGapMinutes: 5,
	}
	t0 := time.Date(2026, 9, 10, 14, 0, 0, 0, time.UTC)
	InjectTimeLine(bodyOf(t, "user", "会话A第一句"), cfg, "k", st, t0)
	got, _ := InjectTimeLine(bodyOf(t, "user", "会话B第一句"), cfg, "k", st, t0.Add(time.Hour))
	if strings.Contains(lastContent(t, got), "距上轮") {
		t.Fatal("不同会话不应显示距上轮")
	}
}

func TestTimeContext_DifferentAPIKey_Isolated(t *testing.T) {
	// API key 参与隔离：key1 的记录不能被 key2 命中
	st := NewTimeContextStore(10)
	cfg := &config.TimeContextConfig{
		Enabled: true, TZ: "Asia/Shanghai", Format: "%Y-%m-%d %H:%M", MinGapMinutes: 5,
	}
	t0 := time.Date(2026, 9, 10, 14, 0, 0, 0, time.UTC)
	InjectTimeLine(bodyOf(t, "user", "同一句话"), cfg, "key1", st, t0)
	got, _ := InjectTimeLine(bodyOf(t, "user", "同一句话"), cfg, "key2", st, t0.Add(time.Hour))
	if strings.Contains(lastContent(t, got), "距上轮") {
		t.Fatal("不同 API key 不应共享会话")
	}
}

func TestTimeContext_RetrySameTurn_NoGap(t *testing.T) {
	// 同一条消息重发（重试 / 双击）：tailKey 命中，不算新一轮，
	// 但也不应显示"距上轮 0 分钟"
	st := NewTimeContextStore(10)
	cfg := &config.TimeContextConfig{
		Enabled: true, TZ: "Asia/Shanghai", Format: "%Y-%m-%d %H:%M", MinGapMinutes: 5,
	}
	t0 := time.Date(2026, 9, 10, 14, 0, 0, 0, time.UTC)
	body := bodyOf(t, "user", "同一条")
	InjectTimeLine(body, cfg, "k", st, t0)
	got, _ := InjectTimeLine(body, cfg, "k", st, t0.Add(10*time.Second))
	if strings.Contains(lastContent(t, got), "距上轮") {
		t.Fatal("重试同一轮不应显示距上轮")
	}
}

func TestTimeContext_SkipToolResultTurn(t *testing.T) {
	// tool 结果回填轮次：agent 同一次推理的延续，不注入
	st := NewTimeContextStore(10)
	cfg := &config.TimeContextConfig{Enabled: true, TZ: "UTC", Format: "%H:%M"}
	body := []byte(`{"model":"m","messages":[` +
		`{"role":"user","content":"查一下"},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"f","input":{}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"r"}]}]}`)
	if _, changed := InjectTimeLine(body, cfg, "k", st, time.Now()); changed {
		t.Fatal("tool 结果轮次不应注入")
	}
}

func TestTimeContext_ContentArray(t *testing.T) {
	// 多模态数组：在首个 text 块前插入时间行
	st := NewTimeContextStore(10)
	cfg := &config.TimeContextConfig{Enabled: true, TZ: "UTC", Format: "%H:%M"}
	body := []byte(`{"model":"m","messages":[{"role":"user","content":[` +
		`{"type":"text","text":"看图"},{"type":"image_url","image_url":{"url":"data:x"}}]}]}`)
	got, changed := InjectTimeLine(body, cfg, "k", st, time.Now())
	if !changed {
		t.Fatal("应注入")
	}
	blocks := contentBlocks(t, got)
	if len(blocks) != 2 {
		t.Fatalf("块数 = %d, want 2", len(blocks))
	}
	if got := blocks[0]["text"].(string); !strings.HasSuffix(got, "\n看图") {
		t.Fatalf("首块 = %q", got)
	}
	if blocks[1]["type"] != "image_url" {
		t.Fatal("图片块丢失")
	}
}

func TestTimeContext_NoUserMessage(t *testing.T) {
	st := NewTimeContextStore(10)
	cfg := &config.TimeContextConfig{Enabled: true, TZ: "UTC", Format: "%H:%M"}
	body := bodyOf(t, "system", "sys")
	if _, changed := InjectTimeLine(body, cfg, "k", st, time.Now()); changed {
		t.Fatal("无 user 消息不应改动")
	}
}

func TestTimeContext_InvalidBody(t *testing.T) {
	st := NewTimeContextStore(10)
	cfg := &config.TimeContextConfig{Enabled: true, TZ: "UTC", Format: "%H:%M"}
	for _, b := range [][]byte{nil, []byte("not json"), []byte(`{"messages":"x"}`)} {
		if _, changed := InjectTimeLine(b, cfg, "k", st, time.Now()); changed {
			t.Fatalf("非法请求体 %q 不应改动", b)
		}
	}
}

func TestTimeContext_FormatGap(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{3*time.Hour + 22*time.Minute, "3 小时 22 分钟"},
		{45 * time.Minute, "45 分钟"},
		{26 * time.Hour, "1 天 2 小时"},
		{2 * 24 * time.Hour, "2 天"},
		{90 * time.Second, "1 分钟"}, // 不足 1 分钟按 1 分钟显示（已过阈值时）
	}
	for _, c := range cases {
		if got := FormatGap(c.d); got != c.want {
			t.Errorf("FormatGap(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

func TestTimeContextStore_LRUAndExpiry(t *testing.T) {
	st := NewTimeContextStore(2)
	now := time.Now()
	st.Touch("a", "t1", now, time.Hour)
	st.Touch("b", "t1", now, time.Hour)
	st.Touch("c", "t1", now, time.Hour) // 超过上限 2 → 淘汰最旧的 a
	if _, ok := st.Get("a"); ok {
		t.Fatal("超上限应淘汰最旧会话")
	}
	if _, ok := st.Get("c"); !ok {
		t.Fatal("新会话应存在")
	}
	// 过期清理
	st.Prune(now.Add(2*time.Hour), time.Hour)
	if st.Len() != 0 {
		t.Fatalf("过期后应清空, len=%d", st.Len())
	}
}

func TestTimeContextStore_Disabled(t *testing.T) {
	st := NewTimeContextStore(0) // 0 = 不启用
	st.Touch("a", "t1", time.Now(), time.Hour)
	if st.Len() != 0 {
		t.Fatal("容量 0 时不应记录")
	}
}

func TestTimeContext_CustomTemplate(t *testing.T) {
	st := NewTimeContextStore(10)
	cfg := &config.TimeContextConfig{
		Enabled: true, TZ: "UTC", Format: "%H:%M",
		Template: "⏰ {{datetime}}",
	}
	got, _ := InjectTimeLine(bodyOf(t, "user", "x"), cfg, "k", st, time.Now())
	if !strings.HasPrefix(lastContent(t, got), "⏰ ") {
		t.Fatalf("自定义模板未生效: %q", lastContent(t, got))
	}
	if strings.Contains(lastContent(t, got), "【时间】") {
		t.Fatal("自定义模板时应替换默认格式")
	}
}

func TestTimeContext_InvalidTZ_FallbackUTC(t *testing.T) {
	st := NewTimeContextStore(10)
	cfg := &config.TimeContextConfig{Enabled: true, TZ: "Not/AZone", Format: "%Y"}
	got, changed := InjectTimeLine(bodyOf(t, "user", "x"), cfg, "k", st, time.Now())
	if !changed {
		t.Fatal("非法时区不应阻断注入")
	}
	if !strings.Contains(lastContent(t, got), "【时间】") {
		t.Fatal("应正常注入")
	}
}

// ─────────────────────────── 测试辅助 ───────────────────────────

func mustJSON(t *testing.T, v interface{}) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// firstContent 取 messages[0].content（字符串）。
func firstContent(t *testing.T, body []byte) string {
	t.Helper()
	var m struct {
		Messages []struct {
			Content interface{} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("body not json: %s", body)
	}
	if len(m.Messages) == 0 {
		t.Fatal("no messages")
	}
	s, _ := m.Messages[0].Content.(string)
	return s
}

// lastContent 取最后一条消息的 content（字符串）。
func lastContent(t *testing.T, body []byte) string {
	t.Helper()
	var m struct {
		Messages []struct {
			Content interface{} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("body not json: %s", body)
	}
	if len(m.Messages) == 0 {
		t.Fatal("no messages")
	}
	s, _ := m.Messages[len(m.Messages)-1].Content.(string)
	return s
}

// contentBlocks 取最后一条消息的 content 数组块。
func contentBlocks(t *testing.T, body []byte) []map[string]interface{} {
	t.Helper()
	var m struct {
		Messages []struct {
			Content []map[string]interface{} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("body not json: %s", body)
	}
	if len(m.Messages) == 0 {
		t.Fatal("no messages")
	}
	return m.Messages[len(m.Messages)-1].Content
}

// TestTimeContext_AgentToolBurst 覆盖 agent 客户端的真实形态：
// 一轮对话里插入多条 tool_call / tool_result，历史增量远超 3 条。
//
// 回归背景：采样窗口曾只有「首 4 + 末 3」且 callbackKeys 只回退 3 条，
// 当一轮增量 > 3 条时，前后两轮的末几条毫无交集，导致每轮都被当成新会话，
// 「距上轮」永不显示（生产上表现为只有【时间】没有【距上轮】）。
func TestTimeContext_AgentToolBurst(t *testing.T) {
	st := NewTimeContextStore(10)
	cfg := &config.TimeContextConfig{
		Enabled: true, TZ: "Asia/Shanghai", Format: "%Y-%m-%d %H:%M", MinGapMinutes: 5,
	}
	t0 := time.Date(2026, 9, 10, 14, 0, 0, 0, time.UTC)

	// 第一轮：用户提问
	first := bodyOf(t, "user", "帮我查一下明天的天气")
	if _, changed := InjectTimeLine(first, cfg, "key1", st, t0); !changed {
		t.Fatal("第一轮应注入")
	}

	// 第二轮：history = 第一轮 + 一轮内插入的多条工具往返 + 新的 user
	// （真实 agent 客户端就是这样把 tool 消息留在 history 里的）
	msgs := []string{
		"user", "帮我查一下明天的天气",
		"assistant", "我需要调用天气工具",
		"tool", "city=深圳 date=2026-09-11",
		"assistant", "查到结果了，我再确认一下",
		"tool", "city=深圳 date=2026-09-11 confirmed=true",
		"assistant", "明天深圳多云",
		"user", "那后天呢",
	}
	second := bodyOf(t, msgs...)
	later := t0.Add(3*time.Hour + 22*time.Minute)
	got, changed := InjectTimeLine(second, cfg, "key1", st, later)
	if !changed {
		t.Fatal("应注入")
	}
	content := lastContent(t, got)
	if !strings.Contains(content, "距上轮") {
		t.Fatalf("一轮内新增 5 条消息后仍应识别为同一会话，实际: %q", content)
	}
	if !strings.Contains(content, "3 小时") {
		t.Fatalf("间隔渲染不对: %q", content)
	}
}

// TestTimeContext_ShouldHashMessageStable 采样策略必须只依赖 (i, n)，
// 保证对同一前缀在不同总长下算出的指纹一致。
func TestTimeContext_ShouldHashMessageStable(t *testing.T) {
	// 前 4 条在任何长度下都参与
	for n := 1; n <= 30; n++ {
		for i := 0; i < 4 && i < n; i++ {
			if !shouldHashMessage(i, n) {
				t.Fatalf("n=%d i=%d 应参与指纹", n, i)
			}
		}
	}
	// 末尾 6 条参与
	if !shouldHashMessage(9, 10) || !shouldHashMessage(4, 10) {
		t.Fatal("n=10 时末 6 条应全部参与")
	}
	if shouldHashMessage(4, 11) {
		t.Fatal("n=11 i=4 中部不应参与")
	}
}
