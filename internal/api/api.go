// Package api wires the HTTP layer: Echo server, middleware and routes.
package api

import (
	"strings"
	"time"

	"capitalapp/internal/config"
	"capitalapp/internal/token"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"gorm.io/gorm"
)

type Server struct {
	cfg   config.Config
	db    *gorm.DB
	token *token.Manager
}

// New builds a fully-configured Echo instance.
func New(cfg config.Config, database *gorm.DB) *echo.Echo {
	s := &Server{
		cfg:   cfg,
		db:    database,
		token: token.NewManager(cfg.JWTSecret, 30*24*time.Hour),
	}

	e := echo.New()
	e.HideBanner = true
	e.Use(middleware.Recover())
	e.Use(middleware.Logger())
	e.Use(middleware.CORSWithConfig(middleware.CORSConfig{
		AllowOrigins: splitOrigins(cfg.CORSOrigins),
		AllowMethods: []string{echo.GET, echo.POST, echo.PATCH, echo.DELETE, echo.OPTIONS},
		AllowHeaders: []string{echo.HeaderAuthorization, echo.HeaderContentType},
	}))

	e.GET("/__health", s.health)

	v1 := e.Group("/api/v1")
	v1.POST("/auth/telegram", s.authTelegram)

	authed := v1.Group("", s.requireAuth)
	authed.GET("/me", s.me)
	authed.PATCH("/me", s.updateMe)

	authed.GET("/overview", s.overview)
	authed.GET("/assets", s.listAssets)
	authed.POST("/assets", s.createAsset)
	authed.GET("/assets/:id", s.getAsset)
	authed.PATCH("/assets/:id", s.updateAsset)
	authed.DELETE("/assets/:id", s.deleteAsset)

	authed.GET("/rates", s.getRates)
	authed.PATCH("/rates", s.updateRates)

	authed.GET("/history", s.history)

	authed.GET("/goals", s.listGoals)
	authed.POST("/goals", s.createGoal)
	authed.PATCH("/goals/:id", s.updateGoal)
	authed.DELETE("/goals/:id", s.deleteGoal)

	return e
}

func splitOrigins(s string) []string {
	if s == "" || s == "*" {
		return []string{"*"}
	}
	parts := strings.Split(s, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}
