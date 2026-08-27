package proxy

import (
	"testing"

	"github.com/icefairy/xuanji/internal/config"
)

func TestStripUpstreamFields(t *testing.T) {
	up := &config.Upstream{StripFields: []string{"prompt_cache_retention", "reasoning"}}
	tests := []struct {
		name string
		body string
		want string
		ok   bool
	}{
		{
			name: "剥单个字段",
			body: `{"model":"m","prompt_cache_retention":"24h","messages":[]}`,
			want: `{"model":"m","messages":[]}`,
			ok:   true,
		},
		{
			name: "剥多个字段",
			body: `{"model":"m","prompt_cache_retention":"24h","reasoning":true,"messages":[]}`,
			want: `{"model":"m","messages":[]}`,
			ok:   true,
		},
		{
			name: "不含任何字段",
			body: `{"model":"m","messages":[]}`,
			want: `{"model":"m","messages":[]}`,
			ok:   false,
		},
		{
			name: "字段不存在不报错",
			body: `{"model":"m","prompt_cache_retention":"24h","messages":[]}`,
			want: `{"model":"m","messages":[]}`,
			ok:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := stripUpstreamFields([]byte(tt.body), up)
			if ok != tt.ok {
				t.Errorf("changed = %v, want %v", ok, tt.ok)
			}
			if string(got) != tt.want {
				t.Errorf("body = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestStripUpstreamFields_NoConfig(t *testing.T) {
	// 未配置 strip_fields 或 nil 上游：原样返回，changed=false
	up := &config.Upstream{}
	body := `{"model":"m","prompt_cache_retention":"24h"}`
	got, ok := stripUpstreamFields([]byte(body), up)
	if ok || string(got) != body {
		t.Errorf("expected no change, got changed=%v body=%s", ok, got)
	}
	got2, ok2 := stripUpstreamFields([]byte(body), nil)
	if ok2 || string(got2) != body {
		t.Errorf("nil upstream: expected no change, got changed=%v body=%s", ok2, got2)
	}
}

func TestIsArrearResponse(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{"中文余额不足", `{"error":{"message":"余额不足，请充值"}}`, true},
		{"英文insufficient balance", `{"error":{"message":"Insufficient balance. Manage your billing here: https://opencode.ai"}}`, true},
		{"quota exceeded", `{"error":{"message":"Allocated quota exceeded, please increase your quota limit"}}`, true},
		{"套餐用完了", `{"error":{"message":"套餐用完了"}}`, true},
		{"insufficient_quota码", `{"error":{"code":"insufficient_quota","message":"..."}},`, true},
		{"限流不算欠费", `{"error":{"message":"您的账户已达到速率限制"}}`, false},
		{"tpm耗尽不算欠费", `{"error":{"message":"inference tpm exhausted"}}`, false},
		{"rate limit不算", `{"error":{"message":"rate limit exceeded"}}`, false},
		{"空响应", ``, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isArrearResponse([]byte(tt.body)); got != tt.want {
				t.Errorf("isArrearResponse(%s) = %v, want %v", tt.body, got, tt.want)
			}
		})
	}
}
