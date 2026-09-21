package repository

import (
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestReservationCountConflict 同机位时段冲突只统计占用态（pending/confirmed/checked_in）。
func TestReservationCountConflict(t *testing.T) {
	gdb, mock := newMockDB(t)
	repo := NewReservationRepository(gdb)
	start := time.Date(2026, 9, 21, 10, 0, 0, 0, time.Local)
	end := start.Add(2 * time.Hour)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT count(*) FROM `reservations` WHERE (station_id = ? AND status IN (?,?,?)) AND (start_time < ? AND end_time > ?) AND id <> ?")).
		WithArgs(uint(7), "pending", "confirmed", "checked_in", end, start, uint(9)).
		WillReturnRows(sqlmockRowsCount(1))

	cnt, err := repo.CountConflict(7, start, end, 9)
	if err != nil {
		t.Fatalf("CountConflict error: %v", err)
	}
	if cnt != 1 {
		t.Fatalf("CountConflict = %d, want 1", cnt)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations not met: %v", err)
	}
}

// TestReservationCountUserConflict 同一会员跨机位的时段冲突判定。
func TestReservationCountUserConflict(t *testing.T) {
	gdb, mock := newMockDB(t)
	repo := NewReservationRepository(gdb)
	start := time.Date(2026, 9, 21, 10, 0, 0, 0, time.Local)
	end := start.Add(time.Hour)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT count(*) FROM `reservations` WHERE (user_id = ? AND status IN (?,?,?)) AND (start_time < ? AND end_time > ?)")).
		WithArgs(uint(3), "pending", "confirmed", "checked_in", end, start).
		WillReturnRows(sqlmockRowsCount(2))

	cnt, err := repo.CountUserConflictTx(gdb, 3, start, end, 0)
	if err != nil {
		t.Fatalf("CountUserConflictTx error: %v", err)
	}
	if cnt != 2 {
		t.Fatalf("CountUserConflictTx = %d, want 2", cnt)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations not met: %v", err)
	}
}

// TestReservationUpdateStatusIfCurrent 条件更新：仅占用态可流转为目标态，返回受影响行数。
func TestReservationUpdateStatusIfCurrent(t *testing.T) {
	gdb, mock := newMockDB(t)
	repo := NewReservationRepository(gdb)
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `reservations` SET `status`=?,`updated_at`=? WHERE id = ? AND status IN (?,?,?)")).
		WithArgs("checked_in", sqlmock.AnyArg(), uint(11), "pending", "confirmed", "checked_in").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	affected, err := repo.UpdateStatusIfCurrentTx(gdb, 11, []string{"pending", "confirmed", "checked_in"}, "checked_in")
	if err != nil {
		t.Fatalf("UpdateStatusIfCurrentTx error: %v", err)
	}
	if affected != 1 {
		t.Fatalf("affected = %d, want 1", affected)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations not met: %v", err)
	}
}

// TestReservationListExpiredBlocking 逾期扫描只捞 pending/confirmed 且已过结束时间的预约。
func TestReservationListExpiredBlocking(t *testing.T) {
	gdb, mock := newMockDB(t)
	repo := NewReservationRepository(gdb)
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.Local)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT * FROM `reservations` WHERE status IN (?,?) AND end_time <= ? ORDER BY end_time ASC LIMIT ?")).
		WithArgs("pending", "confirmed", now, 100).
		WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "station_id", "status", "start_time", "end_time"}).
			AddRow(1, 2, 3, "confirmed", now.Add(-2*time.Hour), now.Add(-time.Hour)))

	list, err := repo.ListExpiredBlocking(now, 100)
	if err != nil {
		t.Fatalf("ListExpiredBlocking error: %v", err)
	}
	if len(list) != 1 || list[0].ID != 1 {
		t.Fatalf("unexpected list: %+v", list)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations not met: %v", err)
	}
}
