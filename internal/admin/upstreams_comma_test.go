package admin

import (
	"encoding/json"
	"testing"
)

// TestSortUpstreamsJSON 覆盖 sortUpstreamsJSON 的两种输入格式：
// JSON 数组字符串与逗号分隔字符串，以及空串/空白输入。
func TestSortUpstreamsJSON(t *testing.T) {
	h := &Handler{}

	cases := []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "JSON 数组字符串原样解析",
			in:   `["minimax","deepseek"]`,
			want: []string{"minimax", "deepseek"},
		},
		{
			name: "逗号分隔字符串兼容（issue 复现场景）",
			in:   `minimax,deepseek`,
			want: []string{"minimax", "deepseek"},
		},
		{
			name: "逗号串带空白",
			in:   ` minimax , deepseek `,
			want: []string{"minimax", "deepseek"},
		},
		{
			name: "单元素逗号串",
			in:   `deepseek`,
			want: []string{"deepseek"},
		},
		{
			name: "空串原样返回",
			in:   ``,
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := h.sortUpstreamsJSON(tc.in)
			if tc.want == nil {
				if got != tc.in {
					t.Fatalf("empty input: got %q want %q", got, tc.in)
				}
				return
			}
			var gotArr []string
			if err := json.Unmarshal([]byte(got), &gotArr); err != nil {
				t.Fatalf("output %q is not JSON array: %v", got, err)
			}
			if len(gotArr) != len(tc.want) {
				t.Fatalf("got %v want %v", gotArr, tc.want)
			}
			for i := range tc.want {
				if gotArr[i] != tc.want[i] {
					t.Fatalf("got %v want %v", gotArr, tc.want)
				}
			}
		})
	}
}
