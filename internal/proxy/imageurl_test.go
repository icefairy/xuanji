package proxy

import (
	"strings"
	"testing"
)

func TestNormalizeImageURLFlat(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantHTTP bool // 是否应发生修改
		wantFlat string
	}{
		{
			name: "嵌套对象拍平",
			body: `{"messages":[{"role":"user","content":[
				{"type":"text","text":"hi"},
				{"type":"image_url","image_url":{"url":"data:image/jpeg;base64,AAAA"}}
			]}]}`,
			wantHTTP: true,
			wantFlat: `"image_url":"data:image/jpeg;base64,AAAA"`,
		},
		{
			name: "已是平铺字符串不动",
			body: `{"messages":[{"role":"user","content":[
				{"type":"image_url","image_url":"data:image/jpeg;base64,BBBB"}
			]}]}`,
			wantHTTP: false,
		},
		{
			name: "image类型同样处理",
			body: `{"messages":[{"role":"user","content":[
				{"type":"image","image_url":{"url":"data:image/png;base64,CCCC"}}
			]}]}`,
			wantHTTP: true,
			wantFlat: `"image_url":"data:image/png;base64,CCCC"`,
		},
		{
			name: "纯文本零开销",
			body: `{"messages":[{"role":"user","content":"你好"}]}`,
			wantHTTP: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			nb, changed := normalizeImageURLFlat([]byte(tc.body))
			if changed != tc.wantHTTP {
				t.Fatalf("changed=%v want %v", changed, tc.wantHTTP)
			}
			if tc.wantFlat != "" && !strings.Contains(string(nb), tc.wantFlat) {
				t.Fatalf("result missing %q:\n%s", tc.wantFlat, nb)
			}
			if tc.wantFlat == "" && !changed {
				// 未修改时应原样
				if string(nb) != tc.body {
					t.Fatalf("no-change case modified body:\n%s", nb)
				}
			}
		})
	}
}