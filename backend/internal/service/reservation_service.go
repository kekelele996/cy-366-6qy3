package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"gorm.io/gorm"

	"github.com/esportsbar/backend/internal/constants"
	"github.com/esportsbar/backend/internal/dto"
	"github.com/esportsbar/backend/internal/model"
	"github.com/esportsbar/backend/internal/repository"
	"github.com/esportsbar/backend/internal/util"
)

// ReservationService 机位预约服务。
type ReservationService struct {
	reservationRepo *repository.ReservationRepository
	userRepo        *repository.UserRepository
	stationService  *StationService
	db              *gorm.DB
	logger          *slog.Logger
}

// NewReservationService 构造预约服务。
func NewReservationService(
	reservationRepo *repository.ReservationRepository,
	userRepo *repository.UserRepository,
	stationService *StationService,
	db *gorm.DB,
	logger *slog.Logger,
) *ReservationService {
	return &ReservationService{
		reservationRepo: reservationRepo,
		userRepo:        userRepo,
		stationService:  stationService,
		db:              db,
		logger:          logger,
	}
}

// validateWindow 校验预约时段本身的合法性。
func validateWindow(start, end time.Time) error {
	if !end.After(start) {
		return util.NewAppError(constants.CodeValidation, "预约结束时间必须晚于开始时间")
	}
	if !start.After(time.Now()) {
		return util.NewAppError(constants.CodeValidation, "预约开始时间必须晚于当前时间，无法预约已开始的时段")
	}
	return nil
}

// checkInWindow 返回预约可开机时间窗 [start-lead, end)。
func checkInWindow(res *model.Reservation, now time.Time) (time.Time, time.Time, error) {
	open := res.StartTime.Add(-time.Duration(constants.CheckInLeadMinutes) * time.Minute)
	if now.Before(open) {
		return open, res.EndTime, util.NewAppError(constants.CodeReservationTime,
			fmt.Sprintf("未到开机时间：仅可在起始前 %d 分钟（%s）至结束前开机",
				constants.CheckInLeadMinutes, util.FormatTime(open)))
	}
	if !now.Before(res.EndTime) {
		return open, res.EndTime, util.NewAppError(constants.CodeReservationExpired, "预约已过结束时间，逾期未开机将自动取消")
	}
	return open, res.EndTime, nil
}

// Create 创建预约：会员提交后为待确认状态；同一机位与同一会员的占用时段均不允许重叠。
func (s *ReservationService) Create(userID uint, req *dto.CreateReservationReq) (*model.Reservation, error) {
	if err := validateWindow(req.StartTime, req.EndTime); err != nil {
		return nil, err
	}
	station, err := s.stationService.GetByID(req.StationID)
	if err != nil {
		return nil, err
	}
	if station.Status == constants.StationFault {
		return nil, util.NewAppError(constants.CodeStationFault, "机位处于故障状态，无法预约，请选择其他机位")
	}
	if station.Status == constants.StationUsing {
		return nil, util.NewAppError(constants.CodeStationBusy, "机位正在使用中，请选择其他机位或更换时段")
	}
	res := &model.Reservation{
		UserID:    userID,
		StationID: req.StationID,
		StartTime: req.StartTime,
		EndTime:   req.EndTime,
		Status:    constants.ReservationPending,
		Remark:    req.Remark,
	}
	err = s.db.Transaction(func(tx *gorm.DB) error {
		// 固定锁顺序（会员 → 机位），串行化同一会员/同一机位的并发预约。
		if _, err := s.userRepo.LockByID(tx, userID); err != nil {
			return err
		}
		locked, err := s.stationService.LockForUpdate(tx, req.StationID)
		if err != nil {
			return err
		}
		if locked.Status != constants.StationIdle && locked.Status != constants.StationReserved {
			return util.NewAppError(constants.CodeStationBusy, "机位当前不可预约，请选择其他机位")
		}
		if err := ensureNoConflictTx(s.reservationRepo, tx, userID, req.StationID, req.StartTime, req.EndTime, 0); err != nil {
			return err
		}
		if locked.Status == constants.StationIdle {
			locked.Status = constants.StationReserved
			if err := tx.Save(locked).Error; err != nil {
				return err
			}
		}
		return s.reservationRepo.CreateTx(tx, res)
	})
	if err != nil {
		var appErr *util.AppError
		if errors.As(err, &appErr) {
			return nil, err
		}
		return nil, fmt.Errorf("reservation create tx: %w", err)
	}
	s.logger.Info(fmt.Sprintf(constants.LogTemplates["reservation_create_ok"], userID, req.StationID, req.StartTime.Format("2006-01-02 15:04")))
	return res, nil
}

// ensureNoConflictTx 事务内同时校验同机位与同一会员的时段重叠。
func ensureNoConflictTx(repo *repository.ReservationRepository, tx *gorm.DB, userID, stationID uint, start, end time.Time, excludeID uint) error {
	stationCnt, err := repo.CountConflictTx(tx, stationID, start, end, excludeID)
	if err != nil {
		return fmt.Errorf("reservation station conflict count: %w", err)
	}
	if stationCnt > 0 {
		return util.NewAppError(constants.CodeReservation, "该机位时段存在待确认或已确认的预约，请更换机位或时段")
	}
	userCnt, err := repo.CountUserConflictTx(tx, userID, start, end, excludeID)
	if err != nil {
		return fmt.Errorf("reservation user conflict count: %w", err)
	}
	if userCnt > 0 {
		return util.NewAppError(constants.CodeReservation, "该会员在此时段已有预约，不能占用重叠时段")
	}
	return nil
}

// Confirm 店员/管理员确认预约：并发确认只有一次生效。
func (s *ReservationService) Confirm(id uint) (*model.Reservation, error) {
	pre, err := s.getReservation(id)
	if err != nil {
		return nil, err
	}
	if pre.Status == constants.ReservationPending && !pre.EndTime.After(time.Now()) {
		if _, cancelErr := s.AutoCancelOverdue(context.Background()); cancelErr != nil {
			s.logger.Warn(fmt.Sprintf("reservation %d overdue auto cancel failed: %v", id, cancelErr))
		}
		return nil, util.NewAppError(constants.CodeReservationExpired, "预约已过结束时间，逾期未确认已自动取消")
	}
	var res *model.Reservation
	err = s.db.Transaction(func(tx *gorm.DB) error {
		locked, err := s.reservationRepo.LockByID(tx, id)
		if err != nil {
			return mapReservationNotFound(err)
		}
		res = locked
		switch res.Status {
		case constants.ReservationConfirmed:
			// 幂等：重复确认直接返回当前预约，不重复产生状态流转。
			return nil
		case constants.ReservationPending:
			if !res.EndTime.After(time.Now()) {
				return util.NewAppError(constants.CodeReservationExpired, "预约已过结束时间，逾期未确认已自动取消")
			}
		default:
			return util.NewAppError(constants.CodeReservation, "仅待确认或已确认的预约可以确认")
		}
		affected, err := s.reservationRepo.UpdateStatusIfCurrentTx(tx, id,
			[]string{constants.ReservationPending}, constants.ReservationConfirmed)
		if err != nil {
			return err
		}
		if affected == 0 {
			s.logger.Warn(fmt.Sprintf(constants.LogTemplates["reservation_confirm_race"], id, res.Status, "rows-affected-zero"))
			return util.NewAppError(constants.CodeConflict, "预约状态已变更，确认未生效，请刷新后重试")
		}
		res.Status = constants.ReservationConfirmed
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("reservation confirm tx: %w", err)
	}
	s.logger.Info(fmt.Sprintf(constants.LogTemplates["reservation_confirm_ok"], id))
	return res, nil
}

// Cancel 取消预约：会员仅可取消本人预约，店员/管理员可取消任意预约；取消后释放机位占用。
func (s *ReservationService) Cancel(id, actorID uint, actorStaff bool) (*model.Reservation, error) {
	var res *model.Reservation
	err := s.db.Transaction(func(tx *gorm.DB) error {
		locked, err := s.reservationRepo.LockByID(tx, id)
		if err != nil {
			return mapReservationNotFound(err)
		}
		res = locked
		if !actorStaff && res.UserID != actorID {
			return util.NewAppError(constants.CodeForbidden, "仅可取消本人的预约")
		}
		switch res.Status {
		case constants.ReservationPending, constants.ReservationConfirmed:
		case constants.ReservationCancelled:
			return nil // 幂等
		default:
			return util.NewAppError(constants.CodeReservation, "当前状态不可取消")
		}
		affected, err := s.reservationRepo.UpdateStatusIfCurrentTx(tx, id,
			constants.ReservationBlockingStatus, constants.ReservationCancelled)
		if err != nil {
			return err
		}
		if affected == 0 {
			return util.NewAppError(constants.CodeConflict, "预约状态已变更，取消未生效，请刷新后重试")
		}
		res.Status = constants.ReservationCancelled
		return releaseStationIfFree(tx, s.stationService, s.reservationRepo, res.StationID, 0)
	})
	if err != nil {
		var appErr *util.AppError
		if errors.As(err, &appErr) {
			return nil, err
		}
		return nil, fmt.Errorf("reservation cancel tx: %w", err)
	}
	s.logger.Info(fmt.Sprintf(constants.LogTemplates["reservation_cancel_ok"], id))
	return res, nil
}

// CheckIn 到店开机：仅已确认预约可开机，且只允许在起始前十五分钟至结束前的时间窗内操作。
// 已过结束时间的预约在拒绝开机的同时立即自动取消并释放机位，无需等待定时扫描。
func (s *ReservationService) CheckIn(id uint) (*model.Reservation, error) {
	pre, err := s.getReservation(id)
	if err != nil {
		return nil, err
	}
	if pre.Status == constants.ReservationConfirmed && !time.Now().Before(pre.EndTime) {
		if _, cancelErr := s.AutoCancelOverdue(context.Background()); cancelErr != nil {
			s.logger.Warn(fmt.Sprintf("reservation %d overdue auto cancel failed: %v", id, cancelErr))
		}
		return nil, util.NewAppError(constants.CodeReservationExpired, "预约已过结束时间，逾期未开机已自动取消")
	}
	var res *model.Reservation
	err = s.db.Transaction(func(tx *gorm.DB) error {
		locked, err := s.reservationRepo.LockByID(tx, id)
		if err != nil {
			return mapReservationNotFound(err)
		}
		res = locked
		switch res.Status {
		case constants.ReservationCheckedIn:
			return util.NewAppError(constants.CodeReservation, "该预约已开机，请勿重复开机")
		case constants.ReservationConfirmed:
		case constants.ReservationPending:
			return util.NewAppError(constants.CodeReservation, "预约尚未确认，请联系店员确认后再开机")
		case constants.ReservationCancelled:
			return util.NewAppError(constants.CodeReservation, "预约已取消，无法开机")
		default:
			return util.NewAppError(constants.CodeReservation, "当前状态不可开机")
		}
		if _, _, err := checkInWindow(res, time.Now()); err != nil {
			return err
		}
		affected, err := s.reservationRepo.UpdateStatusIfCurrentTx(tx, id,
			[]string{constants.ReservationConfirmed}, constants.ReservationCheckedIn)
		if err != nil {
			return err
		}
		if affected == 0 {
			s.logger.Warn(fmt.Sprintf(constants.LogTemplates["reservation_confirm_race"], id, res.Status, "checkin-rows-zero"))
			return util.NewAppError(constants.CodeConflict, "预约状态已变更，开机未生效，请刷新后重试")
		}
		res.Status = constants.ReservationCheckedIn
		station, err := s.stationService.LockForUpdate(tx, res.StationID)
		if err != nil {
			return err
		}
		if station.Status != constants.StationUsing {
			station.Status = constants.StationUsing
			if err := tx.Save(station).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		var appErr *util.AppError
		if errors.As(err, &appErr) {
			return nil, err
		}
		return nil, fmt.Errorf("reservation checkin tx: %w", err)
	}
	s.logger.Info(fmt.Sprintf(constants.LogTemplates["reservation_checkin_ok"], id))
	return res, nil
}

// Reschedule 改约：先释放原时段再占用新时段；冲突、越权或任一步写入失败时整体回滚，原预约与机位状态不变。
func (s *ReservationService) Reschedule(id, actorID uint, actorStaff bool, req *dto.RescheduleReservationReq) (*model.Reservation, error) {
	if err := validateWindow(req.StartTime, req.EndTime); err != nil {
		return nil, err
	}
	newStationID := req.StationID
	var res *model.Reservation
	err := s.db.Transaction(func(tx *gorm.DB) error {
		locked, err := s.reservationRepo.LockByID(tx, id)
		if err != nil {
			return mapReservationNotFound(err)
		}
		res = locked
		if !actorStaff && res.UserID != actorID {
			return util.NewAppError(constants.CodeForbidden, "仅可改约本人的预约")
		}
		if res.Status != constants.ReservationPending && res.Status != constants.ReservationConfirmed {
			return util.NewAppError(constants.CodeReservation, "仅待确认或已确认的预约可以改约")
		}
		if newStationID == 0 {
			newStationID = res.StationID
		}
		if _, err := s.userRepo.LockByID(tx, res.UserID); err != nil {
			return err
		}
		// 固定顺序锁定涉及的机位，避免与创建/改约并发时发生死锁。
		stationIDs := []uint{res.StationID}
		if newStationID != res.StationID {
			stationIDs = append(stationIDs, newStationID)
		}
		sort.Slice(stationIDs, func(i, j int) bool { return stationIDs[i] < stationIDs[j] })
		lockedStations := make(map[uint]*model.Station, len(stationIDs))
		for _, sid := range stationIDs {
			st, err := s.stationService.LockForUpdate(tx, sid)
			if err != nil {
				if errors.Is(err, repository.ErrNotFound) {
					return util.NewAppError(constants.CodeNotFound, "改约目标机位不存在")
				}
				return err
			}
			lockedStations[sid] = st
		}
		target := lockedStations[newStationID]
		if target.Status != constants.StationIdle && target.Status != constants.StationReserved {
			return util.NewAppError(constants.CodeStationBusy, "目标机位当前不可预约，请选择其他机位")
		}

		// 第一步：先释放原时段——将原预约临时置为取消并尝试释放原机位；
		// 全部写操作处于同一事务，任一步失败整体回滚，原预约与机位状态保持不变。
		originalStation := res.StationID
		originalStatus := res.Status
		res.Status = constants.ReservationCancelled
		if err := s.reservationRepo.UpdateTx(tx, res); err != nil {
			return fmt.Errorf("reservation reschedule release: %w", err)
		}
		if err := releaseStationIfFree(tx, s.stationService, s.reservationRepo, originalStation, id); err != nil {
			return err
		}

		// 第二步：再占用新时段，按新时段重新做同机位 + 同会员冲突判定。
		if err := ensureNoConflictTx(s.reservationRepo, tx, res.UserID, newStationID, req.StartTime, req.EndTime, id); err != nil {
			return err
		}
		targetNow := lockedStations[newStationID]
		if targetNow.Status == constants.StationIdle {
			targetNow.Status = constants.StationReserved
			if err := tx.Save(targetNow).Error; err != nil {
				return err
			}
		}
		res.StationID = newStationID
		res.StartTime = req.StartTime
		res.EndTime = req.EndTime
		res.Status = originalStatus
		res.Remark = req.Remark
		if err := s.reservationRepo.UpdateTx(tx, res); err != nil {
			return fmt.Errorf("reservation reschedule update: %w", err)
		}
		s.logger.Info(fmt.Sprintf(constants.LogTemplates["reservation_reschedule_ok"],
			id, originalStation, newStationID,
			req.StartTime.Format("2006-01-02 15:04"), req.EndTime.Format("2006-01-02 15:04")))
		return nil
	})
	if err != nil {
		var appErr *util.AppError
		if errors.As(err, &appErr) {
			return nil, err
		}
		return nil, fmt.Errorf("reservation reschedule tx: %w", err)
	}
	return res, nil
}

// AutoCancelOverdue 将已过结束时间仍未开机的待确认/已确认预约置为取消并释放机位（逾期自动取消）。
// 返回本次实际取消的预约数。
func (s *ReservationService) AutoCancelOverdue(ctx context.Context) (int, error) {
	now := time.Now()
	pending, err := s.reservationRepo.ListExpiredBlocking(now, 100)
	if err != nil {
		return 0, fmt.Errorf("reservation expire list: %w", err)
	}
	cancelled := 0
	for _, item := range pending {
		id := item.ID
		end := item.EndTime
		done := false
		err := s.db.Transaction(func(tx *gorm.DB) error {
			locked, err := s.reservationRepo.LockByID(tx, id)
			if err != nil {
				if errors.Is(err, repository.ErrNotFound) {
					done = true
					return nil
				}
				return err
			}
			if locked.Status != constants.ReservationPending && locked.Status != constants.ReservationConfirmed {
				done = true // 已被并发确认/开机/取消流程处理
				return nil
			}
			if locked.EndTime.After(now) {
				done = true // 行内最新数据尚未逾期
				return nil
			}
			affected, err := s.reservationRepo.UpdateStatusIfCurrentTx(tx, id,
				[]string{constants.ReservationPending, constants.ReservationConfirmed},
				constants.ReservationCancelled)
			if err != nil {
				return err
			}
			if affected == 0 {
				done = true
				return nil
			}
			if err := releaseStationIfFree(tx, s.stationService, s.reservationRepo, locked.StationID, 0); err != nil {
				return err
			}
			cancelled++
			return nil
		})
		if err != nil {
			return cancelled, fmt.Errorf("reservation auto cancel tx id=%d: %w", id, err)
		}
		if !done {
			s.logger.Info(fmt.Sprintf(constants.LogTemplates["reservation_autocancel_ok"], id, util.FormatTime(end)))
		}
		if ctx.Err() != nil {
			break
		}
	}
	s.logger.Info(fmt.Sprintf(constants.LogTemplates["reservation_expire_scan"], len(pending), cancelled))
	return cancelled, nil
}

// List 分页查询预约。
func (s *ReservationService) List(query *dto.ReservationQuery) ([]model.Reservation, int64, error) {
	page := query.Page
	pageSize := query.PageSize
	if page <= 0 {
		page = constants.DefaultPage
	}
	if pageSize <= 0 {
		pageSize = constants.DefaultPageSize
	}
	return s.reservationRepo.List(page, pageSize, query.Status, query.UserID)
}

// getReservation 查询预约并统一处理错误。
func (s *ReservationService) getReservation(id uint) (*model.Reservation, error) {
	res, err := s.reservationRepo.FindByID(id)
	if err != nil {
		return nil, mapReservationNotFound(err)
	}
	return res, nil
}

// mapReservationNotFound 统一映射预约不存在错误。
func mapReservationNotFound(err error) error {
	if errors.Is(err, repository.ErrNotFound) {
		return util.NewAppError(constants.CodeNotFound, "预约记录不存在")
	}
	return err
}

// releaseStationIfFree 释放机位预约占用：仅当该机位已无任何占用时段的预约时才回到空闲。
// excludeID 为改约流程中“先释放的原预约”：它的旧时段不再算在占用集合内，整体回滚时由事务自动恢复。
func releaseStationIfFree(tx *gorm.DB, svc *StationService, repo *repository.ReservationRepository, stationID uint, excludeID uint) error {
	station, err := svc.LockForUpdate(tx, stationID)
	if err != nil {
		return err
	}
	if station.Status != constants.StationReserved {
		return nil
	}
	cnt, err := repo.CountBlockingOnStationTx(tx, stationID, excludeID)
	if err != nil {
		return err
	}
	if cnt == 0 {
		station.Status = constants.StationIdle
		return tx.Save(station).Error
	}
	return nil
}
