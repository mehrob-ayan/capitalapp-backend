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

	// Serve the built frontend from the same origin (no CORS, one tunnel).
	// HTML5 mode falls back to index.html for client-side routes; the API and
	// health endpoints are skipped so they still reach their handlers.
	if cfg.WebDir != "" {
		e.Use(middleware.StaticWithConfig(middleware.StaticConfig{
			Root:  cfg.WebDir,
			Index: "index.html",
			HTML5: true,
			Skipper: func(c echo.Context) bool {
				p := c.Request().URL.Path
				return strings.HasPrefix(p, "/api") || p == "/__health"
			},
		}))
	}

	v1 := e.Group("/api/v1")
	v1.POST("/auth/telegram", s.authTelegram)

	authed := v1.Group("", s.requireAuth)
	authed.GET("/me", s.me)
	authed.PATCH("/me", s.updateMe)

	authed.GET("/overview", s.overview)
	authed.GET("/assets", s.listAssets)
	authed.POST("/assets", s.createAsset)
	authed.GET("/assets/:id", s.getAsset)
	authed.GET("/assets/:id/history", s.assetHistory)
	authed.PATCH("/assets/:id", s.updateAsset)
	authed.POST("/assets/:id/topup", s.topupDeposit)
	authed.DELETE("/assets/:id", s.deleteAsset)

	authed.GET("/rates", s.getRates)
	authed.PATCH("/rates", s.updateRates)

	authed.GET("/export", s.exportData)
	authed.POST("/import", s.importData)

	authed.GET("/expenses", s.expenses)
	authed.POST("/expenses", s.createExpense)
	authed.DELETE("/expenses/:id", s.deleteExpense)
	authed.GET("/expenses/categories", s.expenseCategories)

	authed.GET("/history", s.history)
	authed.GET("/history/composition", s.composition)
	authed.PATCH("/history/:date", s.patchSnapshot)
	authed.PATCH("/history/:date/note", s.patchSnapshotNote)
	authed.DELETE("/history/:date", s.deleteSnapshot)
	authed.GET("/efficiency", s.efficiency)

	authed.GET("/goals", s.listGoals)
	authed.POST("/goals", s.createGoal)
	authed.PATCH("/goals/:id", s.updateGoal)
	authed.DELETE("/goals/:id", s.deleteGoal)

	authed.GET("/activity", s.listActivity)

	authed.GET("/accounts", s.listAccounts)
	authed.POST("/accounts", s.createAccount)
	authed.DELETE("/accounts/:id", s.deleteAccount)
	authed.GET("/accounts/:id/entries", s.accountEntries)
	authed.POST("/accounts/:id/entries", s.addEntry)
	authed.PATCH("/entries/:id", s.updateEntry)
	authed.DELETE("/entries/:id", s.deleteEntry)
	authed.POST("/debts/:id/pay", s.payDebt)
	authed.GET("/debts/:id/payments", s.debtPayments)
	authed.POST("/lent/:id/repay", s.repayLent)
	authed.GET("/lent/:id/repayments", s.debtPayments)
	authed.GET("/salary/schedule", s.salarySchedule)

	authed.GET("/options", s.listOptions)
	authed.POST("/options", s.createOption)
	authed.PATCH("/options/:id", s.updateOption)
	authed.DELETE("/options/:id", s.deleteOption)

	// Catch-up worker: backfills history snapshots missed while the machine
	// was asleep/off. Runs on startup and hourly.
	go s.runWorkers()

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
