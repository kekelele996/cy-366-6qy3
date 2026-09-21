package service

import (
	"testing"
	"time"

	"github.com/esportsbar/backend/internal/constants"
	"github.com/esportsbar/backend/internal/model"
)

// TestCheckInWindow 开机窗口：仅 [start-15min, end) 内允许开机。
func TestCheckInWindow(t *testing.T) {
	loc := time.Local
	start := time.Date(2026, 9, 21, 14, 0, 0, 0, loc)
	end := time.Date(2026, 9, 21, 16, 0, 0, 0, loc)
	res := &model.Reservation{StartTime: start, EndTime: end, Status: constants.ReservationConfirmed}
	cases := []struct {
		name string
		now  time.Time
		want bool
	}{
		{name: "before_window", now: start.Add(-16 * time.Minute), want: false},
		{name: "boundary_minus_15m", now: start.Add(-15 * time.Minute), want: true},
		{name: "at_start", now: start, want: true},
		{name: "mid_window", now: start.Add(time.Hour), want: true},
		{name: "boundary_end_exclusive", now: end, want: false},
		{name: "after_end", now: end.Add(time.Minute), want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := checkInWindow(res, tc.now); got != tc.want {
				t.Fatalf("checkInWindow at %s = %v, want %v", tc.now.Format(time.RFC3339), got, tc.want)
			}
		})
	}
}
