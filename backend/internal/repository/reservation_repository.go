package repository

import (
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/esportsbar/backend/internal/model"
)

// ActiveReservationStatuses 占用机位/会员时段的预约状态：
// 待确认与已确认预约均占用时段；已开机的预约在其时段内同样阻塞冲突预约。
var ActiveReservationStatuses = []string{"pending", "confirmed", "checked_in"}

// ReservationRepository 预约仓储。
type ReservationRepository struct {
	db *gorm.DB
}

// NewReservationRepository 构造预约仓储。
func NewReservationRepository(db *gorm.DB) *ReservationRepository {
	return &ReservationRepository{db: db}
}

// FindByID 查询预约。
func (r *ReservationRepository) FindByID(id uint) (*model.Reservation, error) {
	var res model.Reservation
	err := r.db.First(&res, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	return &res, err
}

// LockByID 事务内行锁查询预约（并发确认/开机/改约时使用 SELECT ... FOR UPDATE）。
func (r *ReservationRepository) LockByID(tx *gorm.DB, id uint) (*model.Reservation, error) {
	var res model.Reservation
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&res, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	return &res, err
}

// CreateTx 事务内创建预约。
func (r *ReservationRepository) CreateTx(tx *gorm.DB, res *model.Reservation) error {
	return tx.Create(res).Error
}

// UpdateTx 事务内更新预约。
func (r *ReservationRepository) UpdateTx(tx *gorm.DB, res *model.Reservation) error {
	return tx.Save(res).Error
}

// List 分页查询预约。
func (r *ReservationRepository) List(page, pageSize int, status string, userID uint) ([]model.Reservation, int64, error) {
	var list []model.Reservation
	var total int64
	query := r.db.Model(&model.Reservation{})
	if status != "" {
		query = query.Where("status = ?", status)
	}
	if userID > 0 {
		query = query.Where("user_id = ?", userID)
	}
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	err := query.Order("start_time DESC").Offset((page - 1) * pageSize).Limit(pageSize).Find(&list).Error
	return list, total, err
}

// CountStationConflictTx 事务内统计同一机位时段重叠的有效预约数（excludeID 用于改约排除自身）。
func (r *ReservationRepository) CountStationConflictTx(tx *gorm.DB, stationID uint, start, end time.Time, excludeID uint) (int64, error) {
	var cnt int64
	query := tx.Model(&model.Reservation{}).
		Where("station_id = ? AND status IN ?", stationID, ActiveReservationStatuses).
		Where("start_time < ? AND end_time > ?", end, start)
	if excludeID > 0 {
		query = query.Where("id <> ?", excludeID)
	}
	if err := query.Count(&cnt).Error; err != nil {
		return 0, err
	}
	return cnt, nil
}

// CountUserConflictTx 事务内统计同一会员时段重叠的有效预约数（excludeID 用于改约排除自身）。
func (r *ReservationRepository) CountUserConflictTx(tx *gorm.DB, userID uint, start, end time.Time, excludeID uint) (int64, error) {
	var cnt int64
	query := tx.Model(&model.Reservation{}).
		Where("user_id = ? AND status IN ?", userID, ActiveReservationStatuses).
		Where("start_time < ? AND end_time > ?", end, start)
	if excludeID > 0 {
		query = query.Where("id <> ?", excludeID)
	}
	if err := query.Count(&cnt).Error; err != nil {
		return 0, err
	}
	return cnt, nil
}

// CountActiveByStationTx 事务内统计机位当前仍占用时段的有效预约数（用于释放/恢复机位预约态）。
func (r *ReservationRepository) CountActiveByStationTx(tx *gorm.DB, stationID uint) (int64, error) {
	var cnt int64
	err := tx.Model(&model.Reservation{}).
		Where("station_id = ? AND status IN ?", stationID, ActiveReservationStatuses).
		Count(&cnt).Error
	return cnt, err
}

// FindExpiredIDs 查询已过结束时间仍处于待确认/已确认的预约 ID（逾期自动取消定时清理）。
func (r *ReservationRepository) FindExpiredIDs(now time.Time, limit int) ([]uint, error) {
	var ids []uint
	err := r.db.Model(&model.Reservation{}).
		Where("status IN ? AND end_time <= ?", []string{"pending", "confirmed"}, now).
		Order("end_time ASC").
		Limit(limit).
		Pluck("id", &ids).Error
	return ids, err
}
