package store

import "testing"

// 组模型配额：0 = 该窗口不限；三个窗口全 0 = 无限制行，不应落库。
// 历史 bug：全 0 时后端静默 DELETE 且返回 ok，前端表格里该行随之消失，
// 用户看到「点新增后输入的东西没了」，误以为保存失败/页面不显示。
func TestGroupQuotaZeroSemantics(t *testing.T) {
	s := openTestStore(t)
	g, err := s.CreateGroup("组A", "[]", "")
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}

	rows := func() []GroupModelQuotaRow {
		t.Helper()
		rs, err := s.ListGroupQuotas(g.ID)
		if err != nil {
			t.Fatalf("ListGroupQuotas: %v", err)
		}
		return rs
	}

	// 只设 5h 窗口 → 落库一行
	if err := s.UpsertGroupQuota(g.ID, "m1", 1000, 0, 0); err != nil {
		t.Fatalf("UpsertGroupQuota: %v", err)
	}
	if got := rows(); len(got) != 1 || got[0].Model != "m1" || got[0].Token5H != 1000 {
		t.Fatalf("5h 窗口写入结果异常: %+v", got)
	}

	// 全 0 → 删除该行（无限制语义）
	if err := s.UpsertGroupQuota(g.ID, "m1", 0, 0, 0); err != nil {
		t.Fatalf("UpsertGroupQuota 全 0: %v", err)
	}
	if got := rows(); len(got) != 0 {
		t.Fatalf("全 0 应删除该行，仍剩: %+v", got)
	}

	// 三窗口分别单设都成立（0 表示该窗口不限，不影响其它窗口）
	if err := s.UpsertGroupQuota(g.ID, "m2", 0, 5000, 0); err != nil {
		t.Fatalf("UpsertGroupQuota m2: %v", err)
	}
	if err := s.UpsertGroupQuota(g.ID, "m3", 0, 0, 9000); err != nil {
		t.Fatalf("UpsertGroupQuota m3: %v", err)
	}
	got := rows()
	if len(got) != 2 {
		t.Fatalf("期望 2 行，得到 %+v", got)
	}
	byModel := map[string]GroupModelQuotaRow{}
	for _, r := range got {
		byModel[r.Model] = r
	}
	if byModel["m2"].TokenWeek != 5000 || byModel["m2"].Token5H != 0 {
		t.Errorf("m2 周窗口异常: %+v", byModel["m2"])
	}
	if byModel["m3"].TokenMonth != 9000 {
		t.Errorf("m3 月窗口异常: %+v", byModel["m3"])
	}
}
