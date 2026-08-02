package config

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"
)

// loadDotEnv 读取 path 指定的 .env 文件（KEY=VALUE 每行一条），把变量注入
// 进程环境。文件不存在时静默跳过；已存在的环境变量不会被覆盖。
func loadDotEnv(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}

	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	line := 0
	for scanner.Scan() {
		line++
		s := strings.TrimSpace(scanner.Text())
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		key, val, ok := strings.Cut(s, "=")
		if !ok {
			return fmt.Errorf("%s:%d: expected KEY=VALUE", path, line)
		}
		key = strings.TrimSpace(key)
		val = strings.Trim(strings.TrimSpace(val), `"'`)
		if key == "" {
			continue
		}
		if _, exists := os.LookupEnv(key); !exists {
			if err := os.Setenv(key, val); err != nil {
				return fmt.Errorf("%s:%d: setenv %s: %w", path, line, key, err)
			}
		}
	}
	return scanner.Err()
}
