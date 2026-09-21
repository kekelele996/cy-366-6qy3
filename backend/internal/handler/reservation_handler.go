package handler

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/gin-gonic/gin"

	"github.com/esportsbar/backend/internal/constants"
	"github.com/esportsbar/backend/internal/dto"
	"github.com/esportsbar/backend/internal/service"
	"github.com/esportsbar/backend/internal/util"
	"github.com/esportsbar/backend/pkg/response"
)

// ReservationHandler 预约接口处理器。
type ReservationHandler struct {
	reservationService *service.ReservationService
	logger             *slog.Logger
}

// NewReservationHandler 构造预约接口处理器。
func NewReservationHandler(reservationService *service.ReservationService, logger *slog.Logger) *ReservationHandler {
	return &ReservationHandler{reservationService: reservationService, logger: logger}
}

// Create 创建预约。
func (h *ReservationHandler) Create(c *gin.Context) {
	var req dto.CreateReservationReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, 400, constants.CodeValidation, "预约参数校验失败："+err.Error())
		return
	}
	userID, _ := c.Get("user_id")
	uid, _ := userID.(uint)
	res, err := h.reservationService.Create(uid, &req)
	if err != nil {
		h.abort(c, err)
		return
	}
	response.OKMessage(c, constants.MsgReserveOK, res)
}

// Confirm 确认预约。
func (h *ReservationHandler) Confirm(c *gin.Context) {
	var idReq dto.IDReq
	if err := c.ShouldBindUri(&idReq); err != nil {
		response.Fail(c, 400, constants.CodeValidation, "预约 ID 无效")
		return
	}
	res, err := h.reservationService.Confirm(idReq.ID)
	if err != nil {
		h.abort(c, err)
		return
	}
	response.OKMessage(c, constants.MsgUpdateSuccess, res)
}

// Cancel 取消预约（会员仅可取消本人未开机预约）。
func (h *ReservationHandler) Cancel(c *gin.Context) {
	var idReq dto.IDReq
	if err := c.ShouldBindUri(&idReq); err != nil {
		response.Fail(c, 400, constants.CodeValidation, "预约 ID 无效")
		return
	}
	uid, role := currentUser(c)
	res, err := h.reservationService.Cancel(idReq.ID, uid, role)
	if err != nil {
		h.abort(c, err)
		return
	}
	response.OKMessage(c, constants.MsgUpdateSuccess, res)
}

// Reschedule 改约（先释放原时段再占用新时段）。
func (h *ReservationHandler) Reschedule(c *gin.Context) {
	var idReq dto.IDReq
	if err := c.ShouldBindUri(&idReq); err != nil {
		response.Fail(c, 400, constants.CodeValidation, "预约 ID 无效")
		return
	}
	var req dto.RescheduleReservationReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, 400, constants.CodeValidation, "改约参数校验失败："+err.Error())
		return
	}
	uid, role := currentUser(c)
	res, err := h.reservationService.Reschedule(idReq.ID, uid, role, &req)
	if err != nil {
		h.abort(c, err)
		return
	}
	response.OKMessage(c, constants.MsgRescheduleOK, res)
}

// CheckIn 到店开机。
func (h *ReservationHandler) CheckIn(c *gin.Context) {
	var idReq dto.IDReq
	if err := c.ShouldBindUri(&idReq); err != nil {
		response.Fail(c, 400, constants.CodeValidation, "预约 ID 无效")
		return
	}
	res, err := h.reservationService.CheckIn(idReq.ID)
	if err != nil {
		h.abort(c, err)
		return
	}
	response.OKMessage(c, constants.MsgCheckInOK, res)
}

// List 分页查询预约（会员端自动按本人过滤，店员端可看全部，展示数据一致）。
func (h *ReservationHandler) List(c *gin.Context) {
	var query dto.ReservationQuery
	if err := c.ShouldBindQuery(&query); err != nil {
		response.Fail(c, 400, constants.CodeValidation, "预约查询参数校验失败："+err.Error())
		return
	}
	uid, role := currentUser(c)
	list, total, err := h.reservationService.List(&query, uid, role)
	if err != nil {
		h.abort(c, err)
		return
	}
	page := query.Page
	if page <= 0 {
		page = constants.DefaultPage
	}
	pageSize := query.PageSize
	if pageSize <= 0 {
		pageSize = constants.DefaultPageSize
	}
	response.OK(c, dto.PageResult{List: list, Total: total, Page: page, PageSize: pageSize})
}

// currentUser 从上下文读取当前会员 ID 与角色。
func currentUser(c *gin.Context) (uint, string) {
	userID, _ := c.Get("user_id")
	uid, _ := userID.(uint)
	role, _ := c.Get("role")
	roleStr, _ := role.(string)
	return uid, roleStr
}

// abort 统一错误处理。
func (h *ReservationHandler) abort(c *gin.Context, err error) {
	var appErr *util.AppError
	if errors.As(err, &appErr) {
		response.Fail(c, httpStatusFor(appErr.Code), appErr.Code, appErr.Message)
		return
	}
	h.logger.Error(fmt.Sprintf("reservation handler error: %v", err))
	response.Fail(c, 500, constants.CodeInternal, "服务器内部错误，请稍后重试")
}
