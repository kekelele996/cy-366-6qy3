package dto

import (
	"errors"
	"strings"
	"time"
)

// LocalTime 兼容前端 RFC3339（2026-08-17T10:00:00+08:00）
// 与本地时间（2026-08-17 10:00:00）两种格式的时间类型。
type LocalTime time.Time

// UnmarshalJSON 反序列化时间，兼容多种常见格式。
func (t *LocalTime) UnmarshalJSON(data []byte) error {
	value := strings.Trim(string(data), `"`)
	if value == "" || value == "null" {
		return nil
	}
	layouts := []string{
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
		"2006-01-02 15:04",
	}
	for _, layout := range layouts {
		if parsed, err := time.ParseInLocation(layout, value, time.Local); err == nil {
			*t = LocalTime(parsed)
			return nil
		}
	}
	return errors.New("时间格式无效，支持 2006-01-02 15:04:05 或 RFC3339")
}

// Time 转回标准库 time.Time。
func (t LocalTime) Time() time.Time { return time.Time(t) }

// MarshalJSON 序列化为 RFC3339。
func (t LocalTime) MarshalJSON() ([]byte, error) {
	return []byte(`"` + time.Time(t).Format(time.RFC3339) + `"`), nil
}
