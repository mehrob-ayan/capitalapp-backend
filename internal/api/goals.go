package api

import (
	"math"
	"net/http"
	"time"

	"capitalapp/internal/calc"
	"capitalapp/internal/model"

	"github.com/labstack/echo/v4"
)

// netWorthUSD sums the user's current net worth in USD (assets − liabilities).
func (s *Server) netWorthUSD(uid uint) (float64, error) {
	var assets []model.Asset
	if err := s.db.Where("user_id = ?", uid).Find(&assets).Error; err != nil {
		return 0, err
	}
	rates, err := s.userRates(uid)
	if err != nil {
		return 0, err
	}
	now := time.Now()
	var nw float64
	for _, a := range assets {
		m := calc.Compute(a, "USD", rates, now)
		nw += m.ValueBase - m.LiabilityBase
	}
	return nw, nil
}

type goalInput struct {
	Title               string  `json:"title"`
	TargetAmount        float64 `json:"targetAmount"`
	Currency            string  `json:"currency"`
	MonthlyContribution float64 `json:"monthlyContribution"`
}

type goalResp struct {
	model.Goal
	CurrentAmount   float64 `json:"currentAmount"`
	ProgressPercent float64 `json:"progressPercent"`
	Remaining       float64 `json:"remaining"`
	MonthsToGoal    *int    `json:"monthsToGoal"`
}

func (s *Server) listGoals(c echo.Context) error {
	uid := c.Get(ctxUserID).(uint)
	rates, err := s.userRates(uid)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "db error")
	}
	netUSD, err := s.netWorthUSD(uid)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "db error")
	}
	var goals []model.Goal
	if err := s.db.Where("user_id = ?", uid).Order("created_at").Find(&goals).Error; err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "db error")
	}
	out := make([]goalResp, 0, len(goals))
	for _, g := range goals {
		out = append(out, buildGoalResp(g, netUSD, rates))
	}
	return c.JSON(http.StatusOK, out)
}

func (s *Server) createGoal(c echo.Context) error {
	uid := c.Get(ctxUserID).(uint)
	in, err := bindGoal(c)
	if err != nil {
		return err
	}
	g := model.Goal{
		UserID: uid, Title: in.Title, TargetAmount: in.TargetAmount,
		Currency: in.Currency, MonthlyContribution: in.MonthlyContribution,
	}
	if g.Title == "" {
		g.Title = "Цель по капиталу"
	}
	if err := s.db.Create(&g).Error; err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "db error")
	}
	return s.respondGoal(c, http.StatusCreated, uid, g)
}

func (s *Server) updateGoal(c echo.Context) error {
	uid := c.Get(ctxUserID).(uint)
	g, err := s.findGoal(uid, c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "goal not found")
	}
	in, err := bindGoal(c)
	if err != nil {
		return err
	}
	g.Title = in.Title
	if g.Title == "" {
		g.Title = "Цель по капиталу"
	}
	g.TargetAmount = in.TargetAmount
	g.Currency = in.Currency
	g.MonthlyContribution = in.MonthlyContribution
	if err := s.db.Save(&g).Error; err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "db error")
	}
	return s.respondGoal(c, http.StatusOK, uid, g)
}

func (s *Server) deleteGoal(c echo.Context) error {
	uid := c.Get(ctxUserID).(uint)
	g, err := s.findGoal(uid, c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "goal not found")
	}
	if err := s.db.Delete(&model.Goal{}, g.ID).Error; err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "db error")
	}
	return c.NoContent(http.StatusNoContent)
}

// ---- helpers ----

func bindGoal(c echo.Context) (goalInput, error) {
	var in goalInput
	if err := c.Bind(&in); err != nil {
		return in, echo.NewHTTPError(http.StatusBadRequest, "invalid body")
	}
	if !validCurrency[in.Currency] {
		return in, echo.NewHTTPError(http.StatusBadRequest, "unsupported currency")
	}
	if in.TargetAmount <= 0 {
		return in, echo.NewHTTPError(http.StatusBadRequest, "target must be positive")
	}
	return in, nil
}

func (s *Server) findGoal(uid uint, idParam string) (model.Goal, error) {
	var g model.Goal
	id, err := parseID(idParam)
	if err != nil {
		return g, err
	}
	err = s.db.Where("id = ? AND user_id = ?", id, uid).First(&g).Error
	return g, err
}

func (s *Server) respondGoal(c echo.Context, status int, uid uint, g model.Goal) error {
	rates, err := s.userRates(uid)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "db error")
	}
	netUSD, err := s.netWorthUSD(uid)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "db error")
	}
	return c.JSON(status, buildGoalResp(g, netUSD, rates))
}

func buildGoalResp(g model.Goal, netUSD float64, rates calc.Rates) goalResp {
	current := calc.Convert(netUSD, "USD", g.Currency, rates)
	remaining := g.TargetAmount - current
	progress := 0.0
	if g.TargetAmount > 0 {
		progress = current / g.TargetAmount * 100
	}
	resp := goalResp{
		Goal:            g,
		CurrentAmount:   round2(current),
		ProgressPercent: round2(progress),
		Remaining:       round2(remaining),
	}
	if remaining <= 0 {
		zero := 0
		resp.MonthsToGoal = &zero
	} else if g.MonthlyContribution > 0 {
		months := int(math.Ceil(remaining / g.MonthlyContribution))
		resp.MonthsToGoal = &months
	}
	return resp
}
