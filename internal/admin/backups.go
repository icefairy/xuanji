package admin

import (
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// 备份目录：DB 同目录下 backups/。备份文件为 db 文件名 + 时间戳 + .gz（gzip 压缩的 SQLite 快照）。
// 例：xuanji.db.20260806_161530.gz
const (
	backupMaxKeep = 10 // 自动保留最近 N 个备份
)

// backupInfo 是备份列表的单个元素。
type backupInfo struct {
	Name string `json:"name"` // 文件名（含 .gz）
	Size int64  `json:"size"` // 字节数
	Time string `json:"time"` // 备份时间（RFC3339，UTC）
	Age  string `json:"age"`  // 人类可读的相对时间，如 "3天前"
}

// BackupDir 返回备份目录路径（数据库同目录 backups/）。
func (h *Handler) backupDir() string {
	if h.store == nil {
		return ""
	}
	return h.store.BackupDir()
}

// Backups 列出所有备份（GET /admin/backups），按时间倒序。
func (h *Handler) Backups(w http.ResponseWriter, _ *http.Request) {
	dir := h.backupDir()
	if dir == "" {
		writeJSON(w, []backupInfo{})
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		writeJSON(w, []backupInfo{})
		return
	}
	var out []backupInfo
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".gz") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, backupInfo{
			Name: e.Name(),
			Size: info.Size(),
			Time: info.ModTime().UTC().Format(time.RFC3339),
			Age:  humanAge(time.Since(info.ModTime())),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time > out[j].Time })
	writeJSON(w, out)
}

// CreateBackup 手动备份（POST /admin/backups）。压缩存放，自动清理超量备份。
func (h *Handler) CreateBackup(w http.ResponseWriter, _ *http.Request) {
	if h.store == nil {
		writeJSON(w, map[string]string{"error": "store not available"})
		return
	}
	name, err := h.store.CreateBackup()
	if err != nil {
		writeJSON(w, map[string]string{"error": "backup failed: " + err.Error()})
		return
	}
	// 自动保留最近 10 个（手动备份也参与轮转，避免磁盘无限增长）
	pruned, _ := pruneBackups(h.backupDir(), backupMaxKeep)
	writeJSON(w, map[string]string{"status": "ok", "name": name, "pruned": fmt.Sprintf("%d", pruned)})
}

// DeleteBackup 删除单个备份（DELETE /admin/backups/{name}）。
func (h *Handler) DeleteBackup(w http.ResponseWriter, r *http.Request) {
	dir := h.backupDir()
	if dir == "" {
		writeJSON(w, map[string]string{"error": "store not available"})
		return
	}
	name := r.PathValue("name")
	// 防目录穿越：只允许文件名本身，不含路径分隔符
	if name == "" || strings.ContainsAny(name, "/\\") || !strings.HasSuffix(name, ".gz") {
		writeJSON(w, map[string]string{"error": "invalid backup name"})
		return
	}
	path := filepath.Join(dir, name)
	if err := os.Remove(path); err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

// pruneBackups 删除超出保留数量的旧备份，返回删除数量。
func pruneBackups(dir string, keep int) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	var gz []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".gz") {
			gz = append(gz, e.Name())
		}
	}
	if len(gz) <= keep {
		return 0, nil
	}
	sort.Strings(gz) // 文件名按时间戳字典序 = 时间升序，最旧在前
	pruned := 0
	for _, n := range gz[:len(gz)-keep] {
		if os.Remove(filepath.Join(dir, n)) == nil {
			pruned++
		}
	}
	return pruned, nil
}

// humanAge 返回人类可读的相对时间。
func humanAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "刚刚"
	case d < time.Hour:
		return fmt.Sprintf("%d分钟前", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d小时前", int(d.Hours()))
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%d天前", int(d.Hours()/24))
	default:
		return fmt.Sprintf("%d周前", int(d.Hours()/24/7))
	}
}

// gzipFile 将 src 压缩为 dst（gzip，保留原文件）。
func gzipFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	gz := gzip.NewWriter(out)
	if _, err := io.Copy(gz, in); err != nil {
		gz.Close()
		return err
	}
	return gz.Close()
}
