package api

import (
	"net/http"
	"time"

	"capitalapp/internal/calc"
	"capitalapp/internal/model"

	"github.com/labstack/echo/v4"
)

// editableCurrencies are everything except USD (the anchor, always 1).
var editableCurrencies = []string{"TJS", "UZS"}

// userRates loads a user's manual rates as a calc.Rates map, seeding sensible
// defaults on first use. USD is always present and equal to 1.
func (s *Server) userRates(uid uint) (calc.Rates, error) {
	var rows []model.Rate
	if err := s.db.Where("user_id = ?", uid).Find(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		rows = defaultRateRows(uid)
		if err := s.db.Create(&rows).Error; err != nil {
			return nil, err
		}
	}
	r := calc.Rates{"USD": 1}
	for _, row := range rows {
		r[row.Currency] = row.PerUSD
	}
	return r, nil
}

func defaultRateRows(uid uint) []model.Rate {
	def := calc.DefaultRates()
	rows := make([]model.Rate, 0, len(editableCurrencies))
	for _, c := range editableCurrencies {
		rows = append(rows, model.Rate{UserID: uid, Currency: c, PerUSD: def[c]})
	}
	return rows
}

// getRates returns the user's rate map, e.g. {"USD":1,"EUR":1.08,...}.
func (s *Server) getRates(c echo.Context) error {
	uid := c.Get(ctxUserID).(uint)
	rates, err := s.userRates(uid)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "db error")
	}
	return c.JSON(http.StatusOK, rates)
}

type updateRatesRequest struct {
	Rates map[string]float64 `json:"rates"`
}

// updateRates upserts manually-entered rates. USD is ignored (always 1).
func (s *Server) updateRates(c echo.Context) error {
	uid := c.Get(ctxUserID).(uint)
	var req updateRatesRequest
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid body")
	}
	before, _ := s.netWorthUSD(uid)
	oldR, _ := s.userRates(uid)
	for cur, v := range req.Rates {
		if cur == "USD" || !validCurrency[cur] {
			continue
		}
		if v <= 0 {
			return echo.NewHTTPError(http.StatusBadRequest, "rate must be positive")
		}
		if err := s.db.
			Where(model.Rate{UserID: uid, Currency: cur}).
			Assign(model.Rate{PerUSD: v}).
			FirstOrCreate(&model.Rate{}).Error; err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, "db error")
		}
	}
	if newR, err := s.userRates(uid); err == nil {
		s.recordRateHistory(uid, newR, time.Now())
		if detail := rateChangeDetail(oldR, newR); detail != "" {
			s.logActivity(uid, "rate_changed", "Изменён курс вручную", detail, 0, "", before)
		}
	}
	return s.getRates(c)
}
