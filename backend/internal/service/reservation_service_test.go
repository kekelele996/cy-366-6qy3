package service

import (
	"testing"
	"time"

	"github.com/esportsbar/backend/internal/constants"
	"github.com/esportsbar/backend/internal/dto"
	"github.com/esportsbar/backend/internal/model"
	"github.com/esportsbar/backend/internal/util"
)

// TestValidateWindow 预约时段合法性白盒测试。
func TestValidateWindow(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name    string
		start   time.Time
		end     time.Time
		wantErr bool
	}{
		{name: "valid_future", start: now.Add(time.Hour), end: now.Add(2 * time.Hour), wantErr: false},
		{name: "end_before_start", start: now.Add(2 * time.Hour), end: now.Add(time.Hour), wantErr: true},
		{name: "end_equal_start", start: now.Add(time.Hour), end: now.Add(time.Hour), wantErr: true},
		{name: "start_in_past", start: now.Add(-time.Minute), end: now.Add(time.Hour), wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateWindow(tc.start, tc.end)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateWindow() err=%v, wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

// TestCheckInWindow 开机时间窗 [起始前15分钟, 结束) 白盒测试。
func TestCheckInWindow(t *testing.T) {
	base := time.Date(2026, 9, 21, 10, 0, 0, 0, time.Local)
	res := &model.Reservation{StartTime: base, EndTime: base.Add(2 * time.Hour)}
	open := base.Add(-time.Duration(constants.CheckInLeadMinutes) * time.Minute)

	cases := []struct {
		name     string
		now      time.Time
		wantCode int
		wantOK   bool
	}{
		{name: "too_early_30m", now: base.Add(-30 * time.Minute), wantCode: constants.CodeReservationTime, wantOK: false},
		{name: "exactly_open_boundary", now: open, wantOK: true},
		{name: "within_window_before_start", now: base.Add(-5 * time.Minute), wantOK: true},
		{name: "during_reservation", now: base.Add(30 * time.Minute), wantOK: true},
		{name: "just_before_end", now: res.EndTime.Add(-time.Second), wantOK: true},
		{name: "exactly_end_expired", now: res.EndTime, wantCode: constants.CodeReservationExpired, wantOK: false},
		{name: "after_end_expired", now: res.EndTime.Add(10 * time.Minute), wantCode: constants.CodeReservationExpired, wantOK: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotOpen, gotEnd, err := checkInWindow(res, tc.now)
			if tc.wantOK {
				if err != nil {
					t.Fatalf("checkInWindow() unexpected err=%v", err)
				}
				if !gotOpen.Equal(open) || !gotEnd.Equal(res.EndTime) {
					t.Fatalf("checkInWindow() window=[%s,%s), want [%s,%s)",
						util.FormatTime(gotOpen), util.FormatTime(gotEnd), util.FormatTime(open), util.FormatTime(res.EndTime))
				}
				return
			}
			appErr, ok := err.(*util.AppError)
			if !ok {
				t.Fatalf("checkInWindow() err type=%T, want *AppError", err)
			}
			if appErr.Code != tc.wantCode {
				t.Fatalf("checkInWindow() code=%d, want %d", appErr.Code, tc.wantCode)
			}
		})
	}
}

// TestRescheduleRequestShape 改约 DTO 必须承载新机位与新时段。
func TestRescheduleRequestShape(t *testing.T) {
	req := dto.RescheduleReservationReq{
		StartTime: time.Now().Add(time.Hour),
		EndTime:   time.Now().Add(2 * time.Hour),
	}
	if req.StationID != 0 {
		t.Fatalf("station_id 缺省时应为 0，由服务层沿用原机位")
	}
}
