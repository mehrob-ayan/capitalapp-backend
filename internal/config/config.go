package config

import (
	"log"
	"os"

	"github.com/joho/godotenv"
)

// Config holds runtime configuration, sourced from environment variables
// (optionally loaded from a local .env file in development).
type Config struct {
	Port          string
	DatabaseURL   string
	BotToken      string
	JWTSecret     string
	AllowDevLogin bool
	CORSOrigins   string
	WebDir        string // if set, serve the built frontend from this directory
}

func Load() Config {
	_ = godotenv.Load() // .env is optional; ignore if missing

	cfg := Config{
		Port:          getenv("PORT", "8080"),
		DatabaseURL:   getenv("DATABASE_URL", "postgres://capital:capital@localhost:5432/capital?sslmode=disable"),
		BotToken:      os.Getenv("TELEGRAM_BOT_TOKEN"),
		JWTSecret:     getenv("JWT_SECRET", "dev-secret-change-me"),
		AllowDevLogin: getenv("ALLOW_DEV_LOGIN", "false") == "true",
		CORSOrigins:   getenv("CORS_ORIGINS", "*"),
		WebDir:        os.Getenv("WEB_DIR"),
	}

	if cfg.BotToken == "" && !cfg.AllowDevLogin {
		log.Println("config: TELEGRAM_BOT_TOKEN is empty and ALLOW_DEV_LOGIN=false — /auth/telegram will reject all requests")
	}
	return cfg
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
