package dto

import (
	"encoding/json"
	"testing"
	"time"
)

func TestLocalTimeUnmarshal(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{name: "rfc3339", raw: `"2026-08-17T10:00:00+08:00"`, want: "2026-08-17 10:00:00"},
		{name: "local_datetime", raw: `"2026-08-17 10:00:00"`, want: "2026-08-17 10:00:00"},
		{name: "local_minute", raw: `"2026-08-17 10:00"`, want: "2026-08-17 10:00:00"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var lt LocalTime
			if err := json.Unmarshal([]byte(tc.raw), &lt); err != nil {
				t.Fatalf("unmarshal error: %v", err)
			}
			if got := lt.Time().Format("2006-01-02 15:04:05"); got != tc.want {
				t.Fatalf("parsed = %s, want %s", got, tc.want)
			}
		})
	}

	var lt LocalTime
	if err := json.Unmarshal([]byte(`"not-a-time"`), &lt); err == nil {
		t.Fatal("expected error for invalid time, got nil")
	}
	var empty LocalTime
	if err := json.Unmarshal([]byte(`""`), &empty); err != nil {
		t.Fatalf("empty string should be allowed: %v", err)
	}
	if !empty.Time().IsZero() {
		t.Fatalf("empty value should map to zero time, got %v", empty.Time())
	}
	_ = time.Local
}
