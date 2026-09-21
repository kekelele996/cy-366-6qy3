package service

import (
	"errors"
	"fmt"
	"log/slog"
	"time"

	"gorm.io/gorm"

	"github.com/esportsbar/backend/internal/constants"
	"github.com/esportsbar/backend/internal/dto"
	"github.com/esportsbar/backend/internal/model"
	"github.com/esportsbar/backend/internal/repository"
	"github.com/esportsbar/backend/internal/util"
)

// SessionService 上机记录服务（含排行榜）。
type SessionService struct {
	sessionRepo        *repository.SessionRepository
	stationService     *StationService
	reservationService *ReservationService
	userPkgRepo        *repository.UserPackageRepository
	userRepo           *repository.UserRepository
	reservationRepo    *repository.ReservationRepository
	db                 *gorm.DB
	logger             *slog.Logger
}

// NewSessionService 构造上机记录服务。
func NewSessionService(
	sessionRepo *repository.SessionRepository,
	stationService *StationService,
	reservationService *ReservationService,
	userPkgRepo *repository.UserPackageRepository,
	userRepo *repository.UserRepository,
	reservationRepo *repository.ReservationRepository,
	db *gorm.DB,
	logger *slog.Logger,
) *SessionService {
	return &SessionService{
		sessionRepo:        sessionRepo,
		stationService:     stationService,
		reservationService: reservationService,
		userPkgRepo:        userPkgRepo,
		userRepo:           userRepo,
		reservationRepo:    reservationRepo,
		db:                 db,
		logger:             logger,
	}
}

// Start 会员上机开机：优先扣时长包，不足扣余额，事务内锁定机位。
func (s *SessionService) Start(userID uint, req *dto.StartSessionReq) (*model.Session, error) {
	station, err := s.stationService.GetByID(req.StationID)
	if err != nil {
		return nil, err
	}
	if station.Status != constants.StationIdle && station.Status != constants.StationReserved {
		return nil, util.NewAppError(constants.CodeStationBusy, "机位当前不可开机")
	}
	// 凭预约开机前先做逾期懒取消：逾期预约在独立事务中自动取消并释放机位。
	if req.ReservationID > 0 {
		if expired, err := s.reservationService.ExpireIfOverdue(req.ReservationID); err != nil {
			return nil, err
		} else if expired {
			return nil, util.NewAppError(constants.CodeResvExpired,
				fmt.Sprintf("预约 %d 已逾期未开机，系统已自动取消", req.ReservationID))
		}
	}
	sess := &model.Session{
		UserID:        userID,
		StationID:     req.StationID,
		ReservationID: req.ReservationID,
		StartTime:     time.Now(),
		GameType:      defaultGameType(req.GameType),
		Status:        constants.SessionActive,
	}
	err = s.db.Transaction(func(tx *gorm.DB) error {
		locked, err := s.stationService.LockForUpdate(tx, req.StationID)
		if err != nil {
			return err
		}
		if locked.Status == constants.StationUsing {
			return util.NewAppError(constants.CodeSessionOpen, "该机位已有进行中的上机记录")
		}
		if locked.Status == constants.StationFault {
			return util.NewAppError(constants.CodeStationFault, "机位当前故障，无法开机")
		}
		// 凭预约开机：校验归属、机位匹配与开机时间窗（起始前15分钟至结束前），逾期自动取消。
		if req.ReservationID > 0 {
			res, err := s.reservationRepo.LockByID(tx, req.ReservationID)
			if err != nil {
				return s.mapReservationFindErr(err)
			}
			if res.UserID != userID {
				return util.NewAppError(constants.CodeForbidden, "该预约不属于当前会员，越权开机已拒绝")
			}
			if res.StationID != req.StationID {
				return util.NewAppError(constants.CodeValidation, "预约机位与开机机位不一致")
			}
			if err := s.reservationService.CheckInForSession(tx, locked, res, time.Now()); err != nil {
				return err
			}
			locked.Status = constants.StationUsing
			if err := tx.Save(locked).Error; err != nil {
				return err
			}
		} else {
			// 无预约直开：当前时刻若该机位存在占用中的有效预约，则不允许抢占。
			now := time.Now()
			cnt, err := s.reservationRepo.CountStationConflictTx(tx, req.StationID, now, now.Add(time.Nanosecond), 0)
			if err != nil {
				return fmt.Errorf("session count station reservation: %w", err)
			}
			if cnt > 0 {
				return util.NewAppError(constants.CodeResvConflict, "该机位当前时段已被预约，请凭预约开机或选择其他机位")
			}
			locked.Status = constants.StationUsing
			if err := tx.Save(locked).Error; err != nil {
				return err
			}
		}
		return s.sessionRepo.CreateTx(tx, sess)
	})
	if err != nil {
		return nil, fmt.Errorf("session start tx: %w", err)
	}
	s.logger.Info(fmt.Sprintf(constants.LogTemplates["session_start_ok"], userID, req.StationID, sess.ID))
	return sess, nil
}

// Renew 续费：延长上机结束时间，并从时长包/余额扣除费用。
func (s *SessionService) Renew(userID, sessionID uint, req *dto.RenewSessionReq) (*model.Session, error) {
	sess, err := s.sessionRepo.FindByID(sessionID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, util.NewAppError(constants.CodeNotFound, "上机记录不存在")
		}
		return nil, fmt.Errorf("session renew find: %w", err)
	}
	if sess.Status != constants.SessionActive {
		return nil, util.NewAppError(constants.CodeConflict, "仅进行中的上机可以续费")
	}
	station, err := s.stationService.GetByID(sess.StationID)
	if err != nil {
		return nil, err
	}
	// 先扣时长包，不足部分按机位时价扣余额。
	neededHours := float64(req.AddMinutes) / 60
	if err := s.consumeHoursAndCharge(userID, neededHours, station.PricePerHour); err != nil {
		return nil, err
	}
	now := time.Now()
	if sess.EndTime == nil {
		sess.EndTime = &now
	}
	end := sess.EndTime.Add(time.Duration(req.AddMinutes) * time.Minute)
	sess.EndTime = &end
	if err := s.sessionRepo.Update(sess); err != nil {
		return nil, fmt.Errorf("session renew update: %w", err)
	}
	s.logger.Info(fmt.Sprintf(constants.LogTemplates["session_renew_ok"], sessionID, req.AddMinutes))
	return sess, nil
}

// End 会员下机：结算时长与金额，释放机位。
func (s *SessionService) End(userID, sessionID uint, req *dto.EndSessionReq) (*model.Session, error) {
	sess, err := s.sessionRepo.FindByID(sessionID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, util.NewAppError(constants.CodeNotFound, "上机记录不存在")
		}
		return nil, fmt.Errorf("session end find: %w", err)
	}
	if sess.Status != constants.SessionActive {
		return nil, util.NewAppError(constants.CodeConflict, "上机记录已结束")
	}
	now := time.Now()
	end := now
	if sess.EndTime != nil && sess.EndTime.After(now) {
		end = *sess.EndTime
	}
	duration := int(end.Sub(sess.StartTime).Minutes())
	station, err := s.stationService.GetByID(sess.StationID)
	if err != nil {
		return nil, err
	}
	amount := station.PricePerHour * float64(duration) / 60
	// 结算：先扣时长包，不足部分扣余额（结束上机时结算本机时长费用）。
	neededHours := float64(duration) / 60
	if err := s.consumeHoursAndCharge(userID, neededHours, station.PricePerHour); err != nil {
		return nil, err
	}
	err = s.db.Transaction(func(tx *gorm.DB) error {
		// 与预约确认/开机/改约保持相同加锁顺序：先锁机位，再锁预约，避免死锁。
		locked, err := s.stationService.LockForUpdate(tx, sess.StationID)
		if err != nil {
			return err
		}
		// 预约开机产生的上机记录结束后，预约流转为已完成。
		if sess.ReservationID > 0 {
			if res, err := s.reservationRepo.LockByID(tx, sess.ReservationID); err == nil {
				if res.Status == constants.ReservationCheckedIn {
					res.Status = constants.ReservationCompleted
					if err := s.reservationRepo.UpdateTx(tx, res); err != nil {
						return fmt.Errorf("session end complete reservation: %w", err)
					}
				}
			} else if !errors.Is(err, repository.ErrNotFound) {
				return fmt.Errorf("session end reservation find: %w", err)
			}
		}
		sess.EndTime = &end
		sess.DurationMinutes = duration
		sess.Amount = amount
		sess.GameType = defaultGameType(req.GameType)
		sess.Status = constants.SessionCompleted
		if err := s.sessionRepo.UpdateTx(tx, sess); err != nil {
			return fmt.Errorf("session end update: %w", err)
		}
		// 下机后若该机位仍有待确认/已确认预约，机位回到预约态，否则空闲。
		return reconcileStationReserved(tx, s.reservationRepo, s.sessionRepo, locked)
	})
	if err != nil {
		return nil, fmt.Errorf("session end tx: %w", err)
	}
	s.logger.Info(fmt.Sprintf(constants.LogTemplates["session_end_ok"], sessionID, duration, amount))
	return sess, nil
}

// consumeHoursAndCharge 消费扣款：优先消耗时长包小时数，不足部分按机位时价从余额扣除。
func (s *SessionService) consumeHoursAndCharge(userID uint, neededHours, pricePerHour float64) error {
	consumedHours, err := s.userPkgRepo.ConsumeHours(userID, neededHours)
	if err != nil {
		return fmt.Errorf("session consume package: %w", err)
	}
	remainingHours := neededHours - consumedHours
	if remainingHours > 0.0001 {
		cost := remainingHours * pricePerHour
		if err := s.userRepo.UpdateBalance(userID, -cost); err != nil {
			if errors.Is(err, repository.ErrConflict) {
				return util.NewAppError(constants.CodeInsufficient, "会员余额不足，请先充值")
			}
			return fmt.Errorf("session consume balance: %w", err)
		}
	}
	return nil
}

// List 分页查询上机记录。
func (s *SessionService) List(page, pageSize int, userID uint, status string) ([]model.Session, int64, error) {
	return s.sessionRepo.List(page, pageSize, userID, status)
}

// Rank 上机时长排行榜。
func (s *SessionService) Rank(query *dto.RankQuery) ([]RankItem, error) {
	period := query.Period
	if period == "" {
		period = "week"
	}
	limit := query.Limit
	if limit <= 0 {
		limit = 10
	}
	rows, _, err := s.sessionRepo.Rank(period, query.GameType, limit)
	if err != nil {
		return nil, fmt.Errorf("session rank: %w", err)
	}
	s.logger.Info(fmt.Sprintf(constants.LogTemplates["session_rank_query"], period, query.GameType))
	items := make([]RankItem, 0, len(rows))
	for i, row := range rows {
		user, err := s.userRepo.FindByID(row.UserID)
		if err != nil {
			continue
		}
		items = append(items, RankItem{
			Rank:         i + 1,
			UserID:       row.UserID,
			Username:     user.Username,
			Nickname:     user.Nickname,
			TotalMinutes: row.DurationMinutes,
		})
	}
	return items, nil
}

// RankItem 排行榜条目。
type RankItem struct {
	Rank         int    `json:"rank"`
	UserID       uint   `json:"user_id"`
	Username     string `json:"username"`
	Nickname     string `json:"nickname"`
	TotalMinutes int    `json:"total_minutes"`
}

// defaultGameType 默认游戏类型。
func defaultGameType(gameType string) string {
	if gameType == "" {
		return constants.GameOther
	}
	return gameType
}

// mapReservationFindErr 统一映射预约查询错误。
func (s *SessionService) mapReservationFindErr(err error) error {
	if errors.Is(err, repository.ErrNotFound) {
		return util.NewAppError(constants.CodeNotFound, "预约记录不存在")
	}
	return fmt.Errorf("session reservation find: %w", err)
}
