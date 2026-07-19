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

	"capitalapp/internal/api"
	"capitalapp/internal/config"
	"capitalapp/internal/db"

	"gorm.io/gorm"
)

func main() {
	cfg := config.Load()

	// Postgres (Docker) may not be ready yet when this starts at login or wakes
	// from sleep, so retry the connection with backoff instead of dying.
	var (
		database *gorm.DB
		err      error
	)
	for attempt := 1; attempt <= 30; attempt++ {
		database, err = db.Connect(cfg.DatabaseURL)
		if err == nil {
			if sqlDB, derr := database.DB(); derr == nil {
				err = sqlDB.Ping()
			}
		}
		if err == nil {
			break
		}
		log.Printf("db not ready (attempt %d/30): %v", attempt, err)
		time.Sleep(2 * time.Second)
	}
	if err != nil {
		log.Fatalf("db connect: %v", err)
	}
	if err := db.Migrate(database); err != nil {
		log.Fatalf("db migrate: %v", err)
	}

	e := api.New(cfg, database)

	addr := cfg.Host + ":" + cfg.Port
	go func() {
		if err := e.Start(addr); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server: %v", err)
		}
	}()
	log.Printf("capital-app api listening on %s", addr)

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := e.Shutdown(ctx); err != nil {
		log.Printf("shutdown: %v", err)
	}
}
