package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ruruamour/codebuddy2api/internal/app"
)

func main() {
	cfg := app.LoadConfig()
	store, err := app.NewStore(cfg.DBPath)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer store.Close()

	// 建立账号类型表与公共模型表，并把老账号归类。种子取自库里当前生效的值，
	// 所以跑完之后对外行为不变；重复启动是幂等的。
	if err := store.Bootstrap(cfg); err != nil {
		log.Fatalf("bootstrap: %v", err)
	}

	server := app.NewServer(cfg, store)
	rootCtx, stopLoops := context.WithCancel(context.Background())
	defer stopLoops()
	server.StartCreditsLoop(rootCtx)
	server.StartTokenRefreshLoop(rootCtx)
	httpServer := &http.Server{
		Addr:              cfg.ListenAddr(),
		Handler:           server.Routes(),
		ReadHeaderTimeout: 15 * time.Second,
	}

	go func() {
		log.Printf("codebuddy2api listening on %s", cfg.ListenAddr())
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http server: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	stopLoops()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(ctx); err != nil {
		log.Printf("shutdown error: %v", err)
	}
}
