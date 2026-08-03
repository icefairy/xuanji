package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// TestWebEmbed_NoDiskWeb 验证无磁盘 web/ 目录时（单二进制发布/Docker 场景），
// go:embed 嵌入的资源能正常提供管理页。
func TestWebEmbed_NoDiskWeb(t *testing.T) {
	// 临时目录模拟无 web 的 CWD
	tmp, err := os.MkdirTemp("", "xuanji-no-web-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmp)

	oldWD, _ := os.Getwd()
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldWD)

	fsys := webFileSystem()
	if fsys == nil {
		t.Fatal("webFileSystem() 返回 nil")
	}

	// 1. 首页 HTML 可从嵌入 FS 打开
	f, err := fsys.Open("admin_vue.html")
	if err != nil {
		t.Fatalf("嵌入的 admin_vue.html 无法打开: %v", err)
	}
	b, _ := io.ReadAll(f)
	f.Close()
	if !strings.Contains(string(b), "<html") && !strings.Contains(string(b), "html") {
		t.Errorf("admin_vue.html 内容异常，长度=%d", len(b))
	}
	t.Logf("admin_vue.html 嵌入 OK，大小=%d", len(b))

	// 2. /vue/ 静态资源（vue2.min.js）可访问
	req := httptest.NewRequest(http.MethodGet, "/vue/vue2.min.js", nil)
	rec := httptest.NewRecorder()
	http.StripPrefix("/vue/", http.FileServer(webVueFileSystem())).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /vue/vue2.min.js status=%d", rec.Code)
	}
	if rec.Body.Len() < 1000 {
		t.Errorf("vue2.min.js 内容过短: %d", rec.Body.Len())
	}
	t.Logf("/vue/vue2.min.js 嵌入 OK，大小=%d", rec.Body.Len())
}

// TestWebEmbed_DiskPriority 验证磁盘 web/ 目录存在时优先走磁盘（开发模式）。
func TestWebEmbed_DiskPriority(t *testing.T) {
	tmp, err := os.MkdirTemp("", "xuanji-with-web-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmp)

	// 创建磁盘 web/admin_vue.html
	if err := os.MkdirAll(tmp+"/web", 0o755); err != nil {
		t.Fatal(err)
	}
	diskMarker := "<html>disk-version</html>"
	if err := os.WriteFile(tmp+"/web/admin_vue.html", []byte(diskMarker), 0o644); err != nil {
		t.Fatal(err)
	}

	oldWD, _ := os.Getwd()
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldWD)

	fsys := webFileSystem()
	f, err := fsys.Open("admin_vue.html")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(f)
	f.Close()
	if string(b) != diskMarker {
		t.Errorf("应优先返回磁盘 web 版本, got=%q", string(b))
	}
	t.Log("磁盘 web 优先 OK")
}
