package dto

// CreateReservationReq 创建预约请求。
type CreateReservationReq struct {
	StationID uint      `json:"station_id" binding:"required"`
	StartTime LocalTime `json:"start_time" binding:"required"`
	EndTime   LocalTime `json:"end_time" binding:"required"`
	Remark    string    `json:"remark" binding:"omitempty,max=255"`
}

// RescheduleReservationReq 改约请求：先释放原时段再占用新时段。
type RescheduleReservationReq struct {
	StationID uint      `json:"station_id" binding:"omitempty"`
	StartTime LocalTime `json:"start_time" binding:"required"`
	EndTime   LocalTime `json:"end_time" binding:"required"`
	Remark    *string   `json:"remark" binding:"omitempty,max=255"`
}

// ReservationQuery 预约查询参数。
type ReservationQuery struct {
	Page     int    `form:"page" binding:"omitempty,min=1"`
	PageSize int    `form:"page_size" binding:"omitempty,min=1,max=100"`
	Status   string `form:"status" binding:"omitempty,oneof=pending confirmed checked_in completed cancelled"`
	UserID   uint   `form:"user_id"`
}
