package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"recallo/db"
	"recallo/internals/cache"
	"recallo/internals/configs"
	"recallo/internals/handlers"
	"recallo/internals/insights"
	"recallo/internals/jobs"
	"recallo/internals/livekit"
	"recallo/internals/logger"
	"recallo/internals/middleware"
	"recallo/internals/realtime"
	"recallo/internals/rooms"
	"recallo/internals/routes"
	"recallo/internals/summaries"
	"recallo/internals/transcripts"
	"recallo/internals/utils"
	"recallo/internals/webhooks"
)

func main() {
	cfg := configs.LoadConfig()

	closeLog, err := logger.Init()
	if err != nil {
		log.Fatalf("[startup] failed to initialise logger: %v", err)
	}
	defer closeLog()

	logger.App.Printf("[startup] logger initialised — output also tailing logs/app.log")

	utils.InitJWT(cfg.JWTSecretKey)
	handlers.InitOAuth(cfg.GithubClientID, cfg.GithubClientSecret, cfg.GithubOAuthRedirectURL, cfg.FrontendURL)

	if err := db.InitDB(cfg.DatabaseURL, db.DefaultConfig()); err != nil {
		log.Fatalf("[startup] failed to initialise database: %v", err)
	}
	defer db.CloseDBConnection()

	rdb, err := cache.Connect(cfg.RedisURL)
	if err != nil {
		log.Fatalf("[startup] failed to connect to Redis: %v", err)
	}
	defer rdb.Close()
	cache.SetClient(rdb)
	logger.App.Printf("[startup] Redis connected url=%s", cfg.RedisURL)

	jobClient := jobs.NewClient(db.DB, rdb)

	lkService, err := livekit.NewService(cfg.LiveKit)
	if err != nil {
		log.Fatalf("[startup] failed to initialise livekit service: %v", err)
	}
	logger.App.Printf("[startup] livekit service connected host=%s", cfg.LiveKit.Host)

	roomsSvc := rooms.NewService(lkService, cfg.GuestTier)
	roomsHandler := rooms.NewHandler(roomsSvc)

	webhookHandler := webhooks.NewHandler(cfg.LiveKit.APIKey, cfg.LiveKit.WebhookSecret, jobClient)

	transcriptSvc := transcripts.NewService(db.DB, cfg.Deepgram, cfg.Spaces, jobClient)
	summarySvc := summaries.NewService(db.DB, cfg.Grok)

	workerPool := jobs.NewWorkerPool(db.DB, rdb)
	workerPool.Register(jobs.TypeTranscribe, transcriptSvc.Handle)
	workerPool.Register(jobs.TypeSummarize, summarySvc.Handle)
	workerCtx, stopWorkers := context.WithCancel(context.Background())
	go workerPool.Start(workerCtx, map[jobs.JobType]int{
		jobs.TypeTranscribe: 3,
		jobs.TypeSummarize:  2,
	})

	hub := realtime.NewHub()
	defer hub.Shutdown()

	enforcerCtx, stopEnforcer := context.WithCancel(context.Background())
	enforcer := rooms.NewSessionEnforcer(roomsSvc, 60*time.Second)
	go enforcer.Run(enforcerCtx)

	transcriptsHandler := transcripts.NewHandler(db.DB)
	summariesHandler := summaries.NewHandler(db.DB)
	insightsHandler := insights.NewHandler(db.DB)
	routeHandler := routes.RegisterRoutes(hub, roomsHandler, webhookHandler, transcriptsHandler, summariesHandler, insightsHandler)

	// Apply logging middleware on top of the route handler.
	handler := middleware.Loggingmiddleware(routeHandler)

	server := &http.Server{
		Addr:         cfg.HTTPServer.Address,
		Handler:      handler,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	shutdownCh := make(chan os.Signal, 1)
	signal.Notify(shutdownCh, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		logger.App.Printf("[startup] server listening on %s  env=%s", cfg.HTTPServer.Address, cfg.Env)
		logger.App.Printf("[startup] registered routes:")
		logger.App.Printf("  GET  /api/v1/healthcheck")
		logger.App.Printf("  POST /api/v1/auth/register")
		logger.App.Printf("  POST /api/v1/auth/login")
		logger.App.Printf("  POST /api/v1/auth/refresh-session")
		logger.App.Printf("  POST /api/v1/auth/logout              [protected]")
		logger.App.Printf("  GET  /api/v1/auth/current-user        [protected]")
		logger.App.Printf("  GET  /api/v1/users/{id}               [protected]")
		logger.App.Printf("  GET  /api/v1/conversations            [protected]")
		logger.App.Printf("  POST /api/v1/conversation/private/create             [protected]")
		logger.App.Printf("  GET  /api/v1/conversation/private/{private_id}       [protected]")
		logger.App.Printf("  GET  /api/v1/conversation/private/{private_id}/messages [protected]")
		logger.App.Printf("  POST /api/v1/files/{private_id}       [protected]")
		logger.App.Printf("  GET  /api/v1/files/                   [protected]")
		logger.App.Printf("  GET  /api/v1/ws                       [websocket]")
		logger.App.Printf("  POST   /api/v1/rooms                  [guest - public]")
		logger.App.Printf("  GET    /api/v1/rooms/{id}             [guest - public]")
		logger.App.Printf("  DELETE /api/v1/rooms/{id}             [guest - host only]")
		logger.App.Printf("  GET    /api/v1/rooms/{id}/token       [guest - token boundary]")
		logger.App.Printf("  POST   /api/v1/rooms/{id}/extend      [guest - one-time extend]")
		logger.App.Printf("  POST   /webhooks/livekit              [livekit webhook receiver]")
		logger.App.Printf("  GET    /api/v1/rooms/{room_name}/transcript  [transcripts]")
		logger.App.Printf("  GET    /api/v1/transcripts/{id}              [transcripts]")
		logger.App.Printf("  GET    /api/v1/rooms/{room_name}/summary     [summaries]")
		logger.App.Printf("  GET    /api/v1/summaries/by-transcript/{id}  [summaries]")
		logger.App.Printf("  GET    /api/v1/summaries/{id}                [summaries]")

		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.App.Printf("[server] fatal err=%v", err)
		}
	}()

	sig := <-shutdownCh
	logger.App.Printf("[server] signal received: %v — initiating graceful shutdown", sig)

	stopWorkers()
	stopEnforcer()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		logger.App.Printf("[server] shutdown error: %v", err)
	} else {
		logger.App.Printf("[server] shut down cleanly")
	}
}
