// 电竞馆上机管理系统入口。
package main

import (
	"context"
	"encoding/json"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/esportsbar/backend/internal/config"
	"github.com/esportsbar/backend/internal/database"
	"github.com/esportsbar/backend/internal/handler"
	"github.com/esportsbar/backend/internal/middleware"
	"github.com/esportsbar/backend/internal/model"
	"github.com/esportsbar/backend/internal/repository"
	"github.com/esportsbar/backend/internal/router"
	"github.com/esportsbar/backend/internal/service"
	"github.com/esportsbar/backend/internal/util"
	"github.com/esportsbar/backend/pkg/response"
)

func main() {
	cfg := config.Load()
	logger := util.NewLogger(cfg.LogLevel)
	rdb := database.ConnectRedis(cfg, logger)

	db, err := database.Connect(cfg)
	if err != nil {
		log.Fatalf("connect database: %v", err)
	}
	if err := database.Seed(db, logger); err != nil {
		log.Fatalf("seed database: %v", err)
	}

	// 仓储层
	userRepo := repository.NewUserRepository(db)
	stationRepo := repository.NewStationRepository(db)
	packageRepo := repository.NewTimePackageRepository(db)
	userPkgRepo := repository.NewUserPackageRepository(db)
	rechargeRepo := repository.NewRechargeRepository(db)
	orderRepo := repository.NewPackageOrderRepository(db)
	reservationRepo := repository.NewReservationRepository(db)
	sessionRepo := repository.NewSessionRepository(db)
	tournamentRepo := repository.NewTournamentRepository(db)
	teamRepo := repository.NewTeamRepository(db)
	regRepo := repository.NewRegistrationRepository(db)
	matchRepo := repository.NewMatchRepository(db)
	auditRepo := repository.NewAuditRepository(db)

	// 服务层
	authService := service.NewAuthService(userRepo, logger, cfg.JWTSecret, cfg.JWTExpireSec)
	userService := service.NewUserService(userRepo, logger)
	stationService := service.NewStationService(stationRepo, logger)
	packageService := service.NewTimePackageService(packageRepo, logger)
	rechargeService := service.NewRechargeService(userRepo, rechargeRepo, packageRepo, userPkgRepo, orderRepo, logger)
	reservationService := service.NewReservationService(reservationRepo, userRepo, stationService, db, logger)
	sessionService := service.NewSessionService(sessionRepo, stationService, userPkgRepo, userRepo, reservationRepo, db, logger)
	tournamentService := service.NewTournamentService(tournamentRepo, teamRepo, regRepo, matchRepo, db, logger)
	auditService := service.NewAuditService(auditRepo, logger)
	dashboardService := service.NewDashboardService(db, logger)

	// 处理器层
	authHandler := handler.NewAuthHandler(authService, userService, logger)
	userHandler := handler.NewUserHandler(userService, logger)
	stationHandler := handler.NewStationHandler(stationService, logger)
	packageHandler := handler.NewTimePackageHandler(packageService, logger)
	rechargeHandler := handler.NewRechargeHandler(rechargeService, logger)
	reservationHandler := handler.NewReservationHandler(reservationService, logger)
	sessionHandler := handler.NewSessionHandler(sessionService, logger)
	tournamentHandler := handler.NewTournamentHandler(tournamentService, logger)
	auditHandler := handler.NewAuditHandler(auditService, logger)
	dashboardHandler := handler.NewDashboardHandler(dashboardService, logger)

	hub := handler.NewStationHub(logger)
	go hub.Run()
	go broadcastStations(hub, db, logger)
	wsHandler := handler.NewWSHandler(hub, logger)

	// 预约逾期自动取消：周期性把已过结束时间仍未开机的预约置为取消并释放机位。
	expireCtx, stopExpireSweeper := context.WithCancel(context.Background())
	defer stopExpireSweeper()
	go runReservationExpireSweeper(expireCtx, reservationService, logger)

	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()
	engine.Use(
		middleware.RequestID(),
		middleware.ErrorHandler(logger),
		middleware.Logger(logger),
		middleware.RateLimit(600, rdb),
		cors.New(cors.Config{
			AllowAllOrigins: true,
			AllowMethods:    []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
			AllowHeaders:    []string{"Origin", "Content-Type", "Authorization", "X-Request-ID"},
		}),
	)

	engine.GET("/healthz", func(c *gin.Context) {
		response.OK(c, gin.H{"status": "up", "time": time.Now().Format(time.RFC3339)})
	})

	api := engine.Group("/api/v1")
	api.Use(middleware.Audit(auditService))
	router.RegisterAuth(api, authHandler, cfg.JWTSecret)
	router.RegisterUser(api, userHandler, cfg.JWTSecret)
	router.RegisterStation(api, stationHandler, cfg.JWTSecret)
	router.RegisterRecharge(api, rechargeHandler, cfg.JWTSecret)
	router.RegisterTimePackage(api, packageHandler, cfg.JWTSecret)
	router.RegisterReservation(api, reservationHandler, cfg.JWTSecret)
	router.RegisterSession(api, sessionHandler, cfg.JWTSecret)
	router.RegisterTournament(api, tournamentHandler, cfg.JWTSecret)
	router.RegisterAudit(api, auditHandler, cfg.JWTSecret)
	router.RegisterDashboard(api, dashboardHandler, cfg.JWTSecret)
	router.RegisterWS(api, wsHandler)

	srv := &http.Server{
		Addr:    ":" + cfg.ServerPort,
		Handler: engine,
	}

	go func() {
		logger.Info("server started", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("server listen failed", "err", err)
			os.Exit(1)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	logger.Info("shutting down server")
	stopExpireSweeper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

// broadcastStations 周期性向 WebSocket 客户端推送机位状态快照。
func broadcastStations(hub *handler.StationHub, db *gorm.DB, logger *slog.Logger) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		var stations []model.Station
		if err := db.Order("area, id").Find(&stations).Error; err != nil {
			logger.Warn("broadcast stations query failed", "err", err)
			continue
		}
		payload, err := json.Marshal(map[string]any{
			"type":     "station_status",
			"time":     time.Now().Format(time.RFC3339),
			"stations": stations,
		})
		if err != nil {
			continue
		}
		hub.Publish(payload)
	}
}

// runReservationExpireSweeper 预约逾期自动取消定时任务。
func runReservationExpireSweeper(ctx context.Context, svc *service.ReservationService, logger *slog.Logger) {
	// 启动后先等半个周期，避免与启动流量叠加；随后每分钟扫描一次。
	initial := time.NewTimer(30 * time.Second)
	select {
	case <-ctx.Done():
		initial.Stop()
		return
	case <-initial.C:
	}
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	sweep := func() {
		cancelled, err := svc.AutoCancelOverdue(ctx)
		if err != nil {
			logger.Warn("reservation expire sweep failed", "err", err)
			return
		}
		if cancelled > 0 {
			logger.Info("reservation expire sweep released", "cancelled", cancelled)
		}
	}
	sweep()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweep()
		}
	}
}
