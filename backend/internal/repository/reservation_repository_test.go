package repository

import (
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestReservationRepositoryFindExpiredIDs(t *testing.T) {
	gdb, mock := newMockDB(t)
	repo := NewReservationRepository(gdb)
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.Local)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT `id` FROM `reservations` WHERE status IN (?,?) AND end_time <= ? ORDER BY end_time ASC LIMIT ?")).
		WithArgs("pending", "confirmed", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(7).AddRow(9))
	ids, err := repo.FindExpiredIDs(now, 100)
	if err != nil {
		t.Fatalf("FindExpiredIDs error: %v", err)
	}
	if len(ids) != 2 || ids[0] != 7 || ids[1] != 9 {
		t.Fatalf("unexpected ids: %v", ids)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations not met: %v", err)
	}
}

func TestReservationRepositoryCountUserConflict(t *testing.T) {
	gdb, mock := newMockDB(t)
	repo := NewReservationRepository(gdb)
	start := time.Date(2026, 9, 21, 14, 0, 0, 0, time.Local)
	end := start.Add(2 * time.Hour)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT count(*) FROM `reservations` WHERE (user_id = ? AND status IN (?,?,?)) AND (start_time < ? AND end_time > ?) AND id <> ?")).
		WithArgs(uint(3), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), uint(5)).
		WillReturnRows(sqlmockRowsCount(0))
	cnt, err := repo.CountUserConflictTx(gdb, 3, start, end, 5)
	if err != nil {
		t.Fatalf("CountUserConflictTx error: %v", err)
	}
	if cnt != 0 {
		t.Fatalf("unexpected cnt = %d", cnt)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations not met: %v", err)
	}
}
