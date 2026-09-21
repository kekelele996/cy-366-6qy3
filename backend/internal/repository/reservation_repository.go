package repository

import (
	"errors"
	"time"

	"gorm.io/gorm"

	"github.com/esportsbar/backend/internal/constants"
	"github.com/esportsbar/backend/internal/model"
)

// ReservationRepository 预约仓储。
type ReservationRepository struct {
	db *gorm.DB
}

// NewReservationRepository 构造预约仓储。
func NewReservationRepository(db *gorm.DB) *ReservationRepository {
	return &ReservationRepository{db: db}
}

// Create 创建预约。
func (r *ReservationRepository) Create(res *model.Reservation) error {
	return r.db.Create(res).Error
}

// CreateTx 在事务内创建预约。
func (r *ReservationRepository) CreateTx(tx *gorm.DB, res *model.Reservation) error {
	return tx.Create(res).Error
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

// LockByID 事务内行锁查询预约（并发确认/开机/改约使用 SELECT ... FOR UPDATE）。
func (r *ReservationRepository) LockByID(tx *gorm.DB, id uint) (*model.Reservation, error) {
	var res model.Reservation
	err := tx.Clauses(clauseLocking()).First(&res, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	return &res, err
}

// Update 更新预约。
func (r *ReservationRepository) Update(res *model.Reservation) error {
	return r.db.Save(res).Error
}

// UpdateTx 在事务内更新预约。
func (r *ReservationRepository) UpdateTx(tx *gorm.DB, res *model.Reservation) error {
	return tx.Save(res).Error
}

// UpdateStatusIfCurrentTx 条件更新：仅当当前状态属于 fromStatuses 时改为 to，返回受影响行数。
// 并发确认/开机/改约场景下配合行锁使用，保证同一时刻只允许一次状态流转生效。
func (r *ReservationRepository) UpdateStatusIfCurrentTx(tx *gorm.DB, id uint, fromStatuses []string, to string) (int64, error) {
	res := tx.Model(&model.Reservation{}).
		Where("id = ? AND status IN ?", id, fromStatuses).
		Update("status", to)
	return res.RowsAffected, res.Error
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

// overlapWhere 追加时段重叠条件：半开区间 [start,end)，首尾相接不算重叠。
func overlapWhere(query *gorm.DB, start, end time.Time) *gorm.DB {
	return query.Where("start_time < ? AND end_time > ?", end, start)
}

// CountConflict 统计机位在时段内的冲突预约数（待确认/已确认/已开机均不可与新时段共存）。
func (r *ReservationRepository) CountConflict(stationID uint, start, end time.Time, excludeID uint) (int64, error) {
	return r.CountConflictTx(r.db, stationID, start, end, excludeID)
}

// CountConflictTx 事务内统计同机位时段冲突数。
func (r *ReservationRepository) CountConflictTx(tx *gorm.DB, stationID uint, start, end time.Time, excludeID uint) (int64, error) {
	var cnt int64
	query := tx.Model(&model.Reservation{}).
		Where("station_id = ? AND status IN ?", stationID, constants.ReservationBlockingStatus)
	query = overlapWhere(query, start, end)
	if excludeID > 0 {
		query = query.Where("id <> ?", excludeID)
	}
	err := query.Count(&cnt).Error
	return cnt, err
}

// CountUserConflictTx 事务内统计同一会员跨机位的时段冲突数（会员不能同时占用重叠时段）。
func (r *ReservationRepository) CountUserConflictTx(tx *gorm.DB, userID uint, start, end time.Time, excludeID uint) (int64, error) {
	var cnt int64
	query := tx.Model(&model.Reservation{}).
		Where("user_id = ? AND status IN ?", userID, constants.ReservationBlockingStatus)
	query = overlapWhere(query, start, end)
	if excludeID > 0 {
		query = query.Where("id <> ?", excludeID)
	}
	err := query.Count(&cnt).Error
	return cnt, err
}

// CountBlockingOnStationTx 事务内统计该机位仍占有时段的预约数（取消后决定机位是否可释放）。
func (r *ReservationRepository) CountBlockingOnStationTx(tx *gorm.DB, stationID uint, excludeID uint) (int64, error) {
	var cnt int64
	query := tx.Model(&model.Reservation{}).
		Where("station_id = ? AND status IN ?", stationID, constants.ReservationBlockingStatus)
	if excludeID > 0 {
		query = query.Where("id <> ?", excludeID)
	}
	err := query.Count(&cnt).Error
	return cnt, err
}

// ListExpiredBlocking 查询已过结束时间、尚未开机的待确认/已确认预约（逾期未到店）。
// 已开机（checked_in）的预约由上机结算流程处理，不在逾期自动取消范围内。
func (r *ReservationRepository) ListExpiredBlocking(now time.Time, limit int) ([]model.Reservation, error) {
	var list []model.Reservation
	err := r.db.Where("status IN ? AND end_time <= ?",
		[]string{constants.ReservationPending, constants.ReservationConfirmed}, now).
		Order("end_time ASC").
		Limit(limit).
		Find(&list).Error
	return list, err
}
