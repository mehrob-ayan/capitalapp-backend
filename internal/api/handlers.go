package api

import (
	"net/http"
	"time"

	"capitalapp/internal/model"
	"capitalapp/internal/telegram"

	"github.com/labstack/echo/v4"
)

func (s *Server) health(c echo.Context) error {
	return c.JSON(http.StatusOK, echo.Map{"status": "ok"})
}

type authRequest struct {
	InitData string `json:"initData"`
}

type authResponse struct {
	Token string      `json:"token"`
	User  *model.User `json:"user"`
}

// authTelegram validates Telegram initData (or a dev bypass), upserts the user
// and returns a session token.
func (s *Server) authTelegram(c echo.Context) error {
	var req authRequest
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid body")
	}

	var tgUser telegram.User
	switch {
	case req.InitData != "" && s.cfg.BotToken != "":
		data, err := telegram.Validate(req.InitData, s.cfg.BotToken, 24*time.Hour)
		if err != nil {
			return echo.NewHTTPError(http.StatusUnauthorized, err.Error())
		}
		tgUser = data.User
	case s.cfg.AllowDevLogin:
		// Local development outside Telegram — one fixed dev account.
		tgUser = telegram.User{ID: 1, FirstName: "Dev", Username: "dev", LanguageCode: "ru"}
	default:
		return echo.NewHTTPError(http.StatusUnauthorized, "initData required")
	}

	user := model.User{TelegramID: tgUser.ID}
	if err := s.db.
		Where(model.User{TelegramID: tgUser.ID}).
		Assign(model.User{
			Username:     tgUser.Username,
			FirstName:    tgUser.FirstName,
			LastName:     tgUser.LastName,
			LanguageCode: tgUser.LanguageCode,
		}).
		FirstOrCreate(&user).Error; err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "db error")
	}

	tok, err := s.token.Issue(user.ID)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "token error")
	}
	return c.JSON(http.StatusOK, authResponse{Token: tok, User: &user})
}

func (s *Server) me(c echo.Context) error {
	uid := c.Get(ctxUserID).(uint)
	var user model.User
	if err := s.db.First(&user, uid).Error; err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "user not found")
	}
	return c.JSON(http.StatusOK, user)
}

type updateMeRequest struct {
	BaseCurrency   string   `json:"baseCurrency"`
	AutoRates      *bool    `json:"autoRates"`
	MonthlyIncome  *float64 `json:"monthlyIncome"`
	IncomeCurrency string   `json:"incomeCurrency"`
}

// updateMe changes the display base currency and/or the auto-rate toggle.
func (s *Server) updateMe(c echo.Context) error {
	uid := c.Get(ctxUserID).(uint)
	var req updateMeRequest
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid body")
	}
	if req.BaseCurrency != "" {
		if !validCurrency[req.BaseCurrency] {
			return echo.NewHTTPError(http.StatusBadRequest, "unsupported currency")
		}
		if err := s.db.Model(&model.User{}).Where("id = ?", uid).
			Update("base_currency", req.BaseCurrency).Error; err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, "db error")
		}
	}
	if req.AutoRates != nil {
		if err := s.db.Model(&model.User{}).Where("id = ?", uid).
			Update("auto_rates", *req.AutoRates).Error; err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, "db error")
		}
		// Turning it on: fetch immediately so rates update now. Non-fatal —
		// if the API is unreachable, the toggle is still saved and manual
		// rates stay until the next daily attempt.
		if *req.AutoRates {
			_ = s.syncRates(uid, "Курс обновлён автоматически")
		}
	}
	if req.MonthlyIncome != nil {
		upd := map[string]any{"monthly_income": *req.MonthlyIncome}
		if req.IncomeCurrency != "" && validCurrency[req.IncomeCurrency] {
			upd["income_currency"] = req.IncomeCurrency
		}
		if err := s.db.Model(&model.User{}).Where("id = ?", uid).Updates(upd).Error; err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, "db error")
		}
	}
	return s.me(c)
}
