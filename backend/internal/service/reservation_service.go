package service

import (
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

// CheckInLeadMinutes 已确认预约最早可在开始前 15 分钟开机。
const CheckInLeadMinutes = 15

// expiredSweepBatch 逾期自动取消单批最多处理的预约数。
const expiredSweepBatch = 100

// ReservationService 机位预约服务。
type ReservationService struct {
	reservationRepo *repository.ReservationRepository
	sessionRepo     *repository.SessionRepository
	userRepo        *repository.UserRepository
	stationService  *StationService
	db              *gorm.DB
	logger          *slog.Logger
}

// NewReservationService 构造预约服务。
func NewReservationService(
	reservationRepo *repository.ReservationRepository,
	sessionRepo *repository.SessionRepository,
	userRepo *repository.UserRepository,
	stationService *StationService,
	db *gorm.DB,
	logger *slog.Logger,
) *ReservationService {
	return &ReservationService{
		reservationRepo: reservationRepo,
		sessionRepo:     sessionRepo,
		userRepo:        userRepo,
		stationService:  stationService,
		db:              db,
		logger:          logger,
	}
}

// Create 创建预约：同一机位与同一会员的待确认/已确认时段均不可重叠；
// 待确认预约同样占用机位时段。全部校验与写入在同一事务内完成。
func (s *ReservationService) Create(userID uint, req *dto.CreateReservationReq) (*model.Reservation, error) {
	start := req.StartTime.Time()
	end := req.EndTime.Time()
	if !end.After(start) {
		return nil, util.NewAppError(constants.CodeValidation, "预约结束时间必须晚于开始时间")
	}
	if !start.After(time.Now()) {
		return nil, util.NewAppError(constants.CodeValidation, "预约开始时间必须晚于当前时间")
	}
	target, err := s.stationService.GetByID(req.StationID)
	if err != nil {
		return nil, err
	}
	if target.Status == constants.StationFault {
		return nil, util.NewAppError(constants.CodeStationFault, "机位当前故障，暂不可预约，请选择其他机位")
	}

	res := &model.Reservation{
		UserID:    userID,
		StationID: req.StationID,
		StartTime: start,
		EndTime:   end,
		Status:    constants.ReservationPending,
		Remark:    req.Remark,
	}
	err = s.db.Transaction(func(tx *gorm.DB) error {
		// 先锁会员行：同一会员的并发预约在此串行，避免占用重叠时段。
		if _, err := s.userRepo.LockByID(tx, userID); err != nil {
			return fmt.Errorf("reservation lock user: %w", err)
		}
		station, err := s.stationService.LockForUpdate(tx, req.StationID)
		if err != nil {
			return err
		}
		if station.Status == constants.StationFault {
			return util.NewAppError(constants.CodeStationFault, "机位当前故障，暂不可预约，请选择其他机位")
		}
		cnt, err := s.reservationRepo.CountStationConflictTx(tx, req.StationID, start, end, 0)
		if err != nil {
			return fmt.Errorf("reservation count station conflict: %w", err)
		}
		if cnt > 0 {
			return util.NewAppError(constants.CodeResvConflict, "该机位时段已存在待确认或已确认预约，请更换时段或机位")
		}
		userCnt, err := s.reservationRepo.CountUserConflictTx(tx, userID, start, end, 0)
		if err != nil {
			return fmt.Errorf("reservation count user conflict: %w", err)
		}
		if userCnt > 0 {
			return util.NewAppError(constants.CodeResvConflict, "您在该时段已有预约，请勿重复占用重叠时段")
		}
		if err := s.reservationRepo.CreateTx(tx, res); err != nil {
			return fmt.Errorf("reservation create: %w", err)
		}
		return reconcileStationReserved(tx, s.reservationRepo, s.sessionRepo, station)
	})
	if err != nil {
		return nil, err
	}
	s.logger.Info(fmt.Sprintf(constants.LogTemplates["reservation_create_ok"], userID, req.StationID, start.Format("2006-01-02 15:04")))
	return res, nil
}

// Confirm 店员确认预约：仅待确认预约可确认，行锁 + 状态守卫保证并发确认只生效一次。
func (s *ReservationService) Confirm(id uint) (*model.Reservation, error) {
	if expired, err := s.ExpireIfOverdue(id); err != nil {
		return nil, err
	} else if expired {
		return nil, util.NewAppError(constants.CodeResvExpired,
			fmt.Sprintf("预约 %d 已逾期，系统已自动取消", id))
	}
	err := s.db.Transaction(func(tx *gorm.DB) error {
		stationID, err := s.lockStationByReservation(tx, id)
		if err != nil {
			return err
		}
		res, err := s.reservationRepo.LockByID(tx, id)
		if err != nil {
			return s.mapReservationErr(err)
		}
		if res.Status != constants.ReservationPending {
			return util.NewAppError(constants.CodeReservation,
				fmt.Sprintf("预约 %d 当前为%s，仅待确认预约可以确认", id, util.StatusText(res.Status)))
		}
		if !res.EndTime.After(time.Now()) {
			// 与后台扫描/其他请求竞速：放弃本次确认，逾期清理由独立事务负责。
			return util.NewAppError(constants.CodeResvExpired,
				fmt.Sprintf("预约 %d 已逾期，系统将自动取消", id))
		}
		station, err := s.stationService.LockForUpdate(tx, stationID)
		if err != nil {
			return err
		}
		if station.Status == constants.StationFault {
			return util.NewAppError(constants.CodeStationFault, "机位当前故障，无法确认预约")
		}
		res.Status = constants.ReservationConfirmed
		if err := s.reservationRepo.UpdateTx(tx, res); err != nil {
			return fmt.Errorf("reservation confirm update: %w", err)
		}
		// 依据有效预约/上机记录重算机位态（可能店员此前手工流转过机位状态）。
		return reconcileStationReserved(tx, s.reservationRepo, s.sessionRepo, station)
	})
	if err != nil {
		return nil, err
	}
	res, _ := s.reservationRepo.FindByID(id)
	s.logger.Info(fmt.Sprintf(constants.LogTemplates["reservation_confirm_ok"], id))
	return res, nil
}

// Cancel 取消预约：会员仅可取消本人未开机的预约，店员可取消任意预约；
// 事务内释放机位占用，任何写入失败时原预约与机位状态不变。
func (s *ReservationService) Cancel(id uint, userID uint, role string) (*model.Reservation, error) {
	err := s.db.Transaction(func(tx *gorm.DB) error {
		stationID, err := s.lockStationByReservation(tx, id)
		if err != nil {
			return err
		}
		res, err := s.reservationRepo.LockByID(tx, id)
		if err != nil {
			return s.mapReservationErr(err)
		}
		isStaff := role == constants.RoleAdmin || role == constants.RoleStaff
		if !isStaff && res.UserID != userID {
			return util.NewAppError(constants.CodeForbidden, "会员仅可取消本人的预约，越权操作已拒绝")
		}
		if res.Status != constants.ReservationPending &&
			res.Status != constants.ReservationConfirmed &&
			res.Status != constants.ReservationCheckedIn {
			return util.NewAppError(constants.CodeReservation,
				fmt.Sprintf("预约 %d 当前为%s，不可取消", id, util.StatusText(res.Status)))
		}
		if !isStaff && res.Status == constants.ReservationCheckedIn {
			return util.NewAppError(constants.CodeForbidden, "已开机预约请联系店员取消")
		}
		res.Status = constants.ReservationCancelled
		if err := s.reservationRepo.UpdateTx(tx, res); err != nil {
			return fmt.Errorf("reservation cancel update: %w", err)
		}
		station, err := s.stationService.LockForUpdate(tx, stationID)
		if err != nil {
			return err
		}
		return reconcileStationReserved(tx, s.reservationRepo, s.sessionRepo, station)
	})
	if err != nil {
		return nil, err
	}
	res, _ := s.reservationRepo.FindByID(id)
	s.logger.Info(fmt.Sprintf(constants.LogTemplates["reservation_cancel_ok"], id))
	return res, nil
}

// CheckIn 到店开机：仅已确认预约可开机，且仅允许在起始前 15 分钟至结束前操作；
// 逾期未开机自动取消。行锁 + 状态守卫保证并发开机只生效一次。
func (s *ReservationService) CheckIn(id uint) (*model.Reservation, error) {
	if expired, err := s.ExpireIfOverdue(id); err != nil {
		return nil, err
	} else if expired {
		return nil, util.NewAppError(constants.CodeResvExpired,
			fmt.Sprintf("预约 %d 已逾期未开机，系统已自动取消", id))
	}
	err := s.db.Transaction(func(tx *gorm.DB) error {
		stationID, err := s.lockStationByReservation(tx, id)
		if err != nil {
			return err
		}
		res, err := s.reservationRepo.LockByID(tx, id)
		if err != nil {
			return s.mapReservationErr(err)
		}
		if res.Status != constants.ReservationConfirmed {
			return util.NewAppError(constants.CodeReservation,
				fmt.Sprintf("预约 %d 当前为%s，仅已确认预约可以开机", id, util.StatusText(res.Status)))
		}
		now := time.Now()
		if !now.Before(res.EndTime) {
			return util.NewAppError(constants.CodeResvExpired,
				fmt.Sprintf("预约 %d 已逾期，系统将自动取消", id))
		}
		if !checkInWindow(res, now) {
			return util.NewAppError(constants.CodeResvWindow,
				fmt.Sprintf("预约 %d 仅可在 %s 至 %s 之间开机", id,
					res.StartTime.Add(-CheckInLeadMinutes*time.Minute).Format("2006-01-02 15:04"),
					res.EndTime.Format("2006-01-02 15:04")))
		}
		station, err := s.stationService.LockForUpdate(tx, stationID)
		if err != nil {
			return err
		}
		if station.Status == constants.StationUsing {
			return util.NewAppError(constants.CodeStationBusy, "机位已有进行中的上机记录，无法重复开机")
		}
		if station.Status == constants.StationFault {
			return util.NewAppError(constants.CodeStationFault, "机位当前故障，无法开机")
		}
		station.Status = constants.StationUsing
		if err := tx.Save(station).Error; err != nil {
			return fmt.Errorf("reservation checkin station: %w", err)
		}
		res.Status = constants.ReservationCheckedIn
		if err := s.reservationRepo.UpdateTx(tx, res); err != nil {
			return fmt.Errorf("reservation checkin update: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	res, _ := s.reservationRepo.FindByID(id)
	s.logger.Info(fmt.Sprintf(constants.LogTemplates["reservation_checkin_ok"], id))
	return res, nil
}

// Reschedule 改约：会员仅可改本人预约，店员可代客改约。
// 事务内先以"排除自身"的冲突校验释放原时段占位，再校验并占用新时段；
// 冲突、越权或写入失败整体回滚，原预约与机位状态保持不变。
func (s *ReservationService) Reschedule(id uint, userID uint, role string, req *dto.RescheduleReservationReq) (*model.Reservation, error) {
	start := req.StartTime.Time()
	end := req.EndTime.Time()
	if !end.After(start) {
		return nil, util.NewAppError(constants.CodeValidation, "改约结束时间必须晚于开始时间")
	}
	if !start.After(time.Now()) {
		return nil, util.NewAppError(constants.CodeValidation, "改约开始时间必须晚于当前时间")
	}
	err := s.db.Transaction(func(tx *gorm.DB) error {
		// 按固定顺序（会员 -> 机位 -> 预约）加锁，避免跨事务死锁。
		// 先查询原预约拿到会员与原机位，再加锁。
		original, err := s.reservationRepo.FindByID(id)
		if err != nil {
			return s.mapReservationErr(err)
		}
		isStaff := role == constants.RoleAdmin || role == constants.RoleStaff
		if !isStaff && original.UserID != userID {
			return util.NewAppError(constants.CodeForbidden, "会员仅可修改本人的预约，越权操作已拒绝")
		}
		if _, err := s.userRepo.LockByID(tx, original.UserID); err != nil {
			return fmt.Errorf("reschedule lock user: %w", err)
		}
		newStationID := req.StationID
		if newStationID == 0 {
			newStationID = original.StationID
		}
		// 涉及换站时按机位 ID 升序加锁，避免两个改约 A→B / B→A 交叉等待形成死锁。
		stationIDsToLock := []uint{original.StationID}
		if newStationID != original.StationID {
			stationIDsToLock = append(stationIDsToLock, newStationID)
		}
		sort.Slice(stationIDsToLock, func(i, j int) bool { return stationIDsToLock[i] < stationIDsToLock[j] })
		lockedStations := make(map[uint]*model.Station, len(stationIDsToLock))
		for _, sid := range stationIDsToLock {
			st, err := s.stationService.LockForUpdate(tx, sid)
			if err != nil {
				return err
			}
			lockedStations[sid] = st
		}
		if targetStation := lockedStations[newStationID]; targetStation.Status == constants.StationFault {
			return util.NewAppError(constants.CodeStationFault, "新机位当前故障，暂不可改约")
		}
		oldStation := lockedStations[original.StationID]

		// 行锁原预约并做状态守卫。
		res, err := s.reservationRepo.LockByID(tx, id)
		if err != nil {
			return s.mapReservationErr(err)
		}
		if res.Status != constants.ReservationPending && res.Status != constants.ReservationConfirmed {
			return util.NewAppError(constants.CodeReservation,
				fmt.Sprintf("预约 %d 当前为%s，仅待确认或已确认预约可以改约", id, util.StatusText(res.Status)))
		}

		// 释放原时段：冲突统计排除自身，相当于先让出原时段；再占用新时段。
		stationCnt, err := s.reservationRepo.CountStationConflictTx(tx, newStationID, start, end, id)
		if err != nil {
			return fmt.Errorf("reschedule count station conflict: %w", err)
		}
		if stationCnt > 0 {
			return util.NewAppError(constants.CodeResvConflict, "新机位时段已存在待确认或已确认预约，请更换时段或机位")
		}
		userCnt, err := s.reservationRepo.CountUserConflictTx(tx, res.UserID, start, end, id)
		if err != nil {
			return fmt.Errorf("reschedule count user conflict: %w", err)
		}
		if userCnt > 0 {
			return util.NewAppError(constants.CodeResvConflict, "您在新时段已有预约，请勿占用重叠时段")
		}

		oldStationID := res.StationID
		res.StationID = newStationID
		res.StartTime = start
		res.EndTime = end
		// 改约后需店员重新确认，回到待确认。
		res.Status = constants.ReservationPending
		if req.Remark != nil {
			res.Remark = *req.Remark
		}
		if err := s.reservationRepo.UpdateTx(tx, res); err != nil {
			return fmt.Errorf("reschedule update: %w", err)
		}
		// 原时段释放：依据剩余有效预约重算原机位预约态。
		if err := reconcileStationReserved(tx, s.reservationRepo, s.sessionRepo, oldStation); err != nil {
			return err
		}
		if newStation, ok := lockedStations[newStationID]; newStationID != oldStationID && ok {
			if err := reconcileStationReserved(tx, s.reservationRepo, s.sessionRepo, newStation); err != nil {
				return err
			}
		}
		s.logger.Info(fmt.Sprintf(constants.LogTemplates["reservation_reschedule_ok"],
			id, oldStationID, newStationID, start.Format("2006-01-02 15:04"), end.Format("2006-01-02 15:04")))
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.reservationRepo.FindByID(id)
}

// SweepExpired 定时扫描逾期未开机的待确认/已确认预约，自动取消并释放机位。
func (s *ReservationService) SweepExpired() (int, error) {
	now := time.Now()
	ids, err := s.reservationRepo.FindExpiredIDs(now, expiredSweepBatch)
	if err != nil {
		return 0, fmt.Errorf("sweep expired find: %w", err)
	}
	cancelled := 0
	for _, id := range ids {
		done, err := s.expireOneOverdue(id)
		if err != nil {
			// 单条失败不影响其余逾期预约的清理。
			s.logger.Warn("sweep expired reservation failed", "reservationID", id, "err", err.Error())
			continue
		}
		if done {
			cancelled++
		}
	}
	if cancelled > 0 {
		s.logger.Info("expired reservations swept", "count", cancelled)
	}
	return cancelled, nil
}

// StartExpiredSweeper 启动逾期自动取消后台任务。
func (s *ReservationService) StartExpiredSweeper(interval time.Duration) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for range ticker.C {
			if _, err := s.SweepExpired(); err != nil {
				s.logger.Warn("sweep expired reservations failed", "err", err.Error())
			}
		}
	}()
}

// CheckInForSession 会员凭预约直接开机（SessionService.Start 复用）：
// 在已锁机位的事务内校验开机窗口并把预约流转为 checked_in；逾期自动取消。
// 返回的布尔值表示预约是否成功进入已开机状态。
func (s *ReservationService) CheckInForSession(tx *gorm.DB, station *model.Station, res *model.Reservation, now time.Time) error {
	locked, err := s.reservationRepo.LockByID(tx, res.ID)
	if err != nil {
		return s.mapReservationErr(err)
	}
	if locked.Status != constants.ReservationConfirmed {
		return util.NewAppError(constants.CodeReservation,
			fmt.Sprintf("预约 %d 当前为%s，仅已确认预约可以开机", locked.ID, util.StatusText(locked.Status)))
	}
	if !now.Before(locked.EndTime) {
		// 与后台扫描竞速时可能已由独立事务取消：返回逾期错误，当前开机事务回滚。
		return util.NewAppError(constants.CodeResvExpired,
			fmt.Sprintf("预约 %d 已逾期，系统将自动取消", locked.ID))
	}
	if !checkInWindow(locked, now) {
		return util.NewAppError(constants.CodeResvWindow,
			fmt.Sprintf("预约 %d 仅可在起始前%d分钟至结束前开机", locked.ID, CheckInLeadMinutes))
	}
	if locked.StationID != station.ID {
		return util.NewAppError(constants.CodeValidation, "预约机位与开机机位不一致")
	}
	locked.Status = constants.ReservationCheckedIn
	if err := s.reservationRepo.UpdateTx(tx, locked); err != nil {
		return fmt.Errorf("reservation session checkin update: %w", err)
	}
	*res = *locked
	return nil
}

// List 分页查询预约：会员端仅能看到本人预约（数据与店员端一致，只是按归属过滤）。
func (s *ReservationService) List(query *dto.ReservationQuery, currentUserID uint, role string) ([]model.Reservation, int64, error) {
	page := query.Page
	pageSize := query.PageSize
	if page <= 0 {
		page = constants.DefaultPage
	}
	if pageSize <= 0 {
		pageSize = constants.DefaultPageSize
	}
	userID := query.UserID
	if role != constants.RoleAdmin && role != constants.RoleStaff {
		userID = currentUserID
	}
	return s.reservationRepo.List(page, pageSize, query.Status, userID)
}

// lockStationByReservation 事务内先锁机位（固定加锁顺序机位 -> 预约，避免与创建/上机事务死锁）。
func (s *ReservationService) lockStationByReservation(tx *gorm.DB, id uint) (uint, error) {
	var res model.Reservation
	if err := tx.Select("station_id").First(&res, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return 0, util.NewAppError(constants.CodeNotFound, "预约记录不存在")
		}
		return 0, fmt.Errorf("reservation load station: %w", err)
	}
	if _, err := s.stationService.LockForUpdate(tx, res.StationID); err != nil {
		return 0, err
	}
	return res.StationID, nil
}

// ExpireIfOverdue 若预约已过结束时间仍处于待确认/已确认，则在独立事务中自动取消并释放机位。
// 返回 expired=true 表示本次调用完成了逾期取消。逾期取消必须独立提交，
// 不能嵌入调用方随后要回滚的业务事务。
func (s *ReservationService) ExpireIfOverdue(id uint) (bool, error) {
	return s.expireOneOverdue(id)
}

// expireOneOverdue 单条逾期自动取消：固定顺序锁机位 -> 预约，二次确认状态与时间后提交。
func (s *ReservationService) expireOneOverdue(id uint) (bool, error) {
	done := false
	err := s.db.Transaction(func(tx *gorm.DB) error {
		stationID, err := s.lockStationByReservation(tx, id)
		if err != nil {
			return err
		}
		res, err := s.reservationRepo.LockByID(tx, id)
		if err != nil {
			return s.mapReservationErr(err)
		}
		if res.Status != constants.ReservationPending && res.Status != constants.ReservationConfirmed {
			return nil
		}
		if res.EndTime.After(time.Now()) {
			return nil
		}
		res.Status = constants.ReservationCancelled
		if err := s.reservationRepo.UpdateTx(tx, res); err != nil {
			return fmt.Errorf("reservation expire cancel update: %w", err)
		}
		station, err := s.stationService.LockForUpdate(tx, stationID)
		if err != nil {
			return err
		}
		if err := reconcileStationReserved(tx, s.reservationRepo, s.sessionRepo, station); err != nil {
			return err
		}
		done = true
		s.logger.Info(fmt.Sprintf(constants.LogTemplates["reservation_expire_cancel"], res.ID, stationID))
		return nil
	})
	if err != nil {
		return false, err
	}
	return done, nil
}

// mapReservationErr 统一映射仓储错误。
func (s *ReservationService) mapReservationErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, repository.ErrNotFound) {
		return util.NewAppError(constants.CodeNotFound, "预约记录不存在")
	}
	return fmt.Errorf("reservation find: %w", err)
}

// checkInWindow 判断当前时间是否处于开机窗口：[start-15min, end)。
func checkInWindow(res *model.Reservation, now time.Time) bool {
	earliest := res.StartTime.Add(-CheckInLeadMinutes * time.Minute)
	return !now.Before(earliest) && now.Before(res.EndTime)
}

// reconcileStationReserved 依据机位剩余有效预约/上机记录重算机位预约态：
// 有进行中上机 -> using；无上机但有有效预约 -> reserved；都没有 -> idle；故障态保持故障态。
func reconcileStationReserved(
	tx *gorm.DB,
	reservationRepo *repository.ReservationRepository,
	sessionRepo *repository.SessionRepository,
	station *model.Station,
) error {
	if station.Status == constants.StationFault {
		return nil
	}
	activeSessions, err := sessionRepo.CountActiveByStationTx(tx, station.ID)
	if err != nil {
		return fmt.Errorf("reconcile station count sessions: %w", err)
	}
	if activeSessions > 0 {
		station.Status = constants.StationUsing
		return tx.Save(station).Error
	}
	activeReservations, err := reservationRepo.CountActiveByStationTx(tx, station.ID)
	if err != nil {
		return fmt.Errorf("reconcile station count reservations: %w", err)
	}
	if activeReservations > 0 {
		station.Status = constants.StationReserved
	} else {
		station.Status = constants.StationIdle
	}
	return tx.Save(station).Error
}
