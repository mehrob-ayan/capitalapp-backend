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
	rates, err := s.userRates(uid)
	if err != nil {
		return 0, err
	}
	assets, liab, err := s.totalsAsOf(uid, "USD", rates, time.Now())
	return assets - liab, err
}

type goalInput struct {
	Title               string  `json:"title"`
	TargetAmount        float64 `json:"targetAmount"`
	Currency            string  `json:"currency"`
	MonthlyContribution float64 `json:"monthlyContribution"`
	LinkedAssetID       *uint   `json:"linkedAssetId"`
}

type goalResp struct {
	model.Goal
	GoalKind             string  `json:"goalKind"` // capital | savings | debt
	CurrentAmount        float64 `json:"currentAmount"`
	ProgressPercent      float64 `json:"progressPercent"`
	Remaining            float64 `json:"remaining"`
	MonthsToGoal         *int    `json:"monthsToGoal"`
	LinkedAssetName      string  `json:"linkedAssetName"`
	ContributedThisMonth float64 `json:"contributedThisMonth"`
	OnTrack              bool    `json:"onTrack"`
	TargetDate           string  `json:"targetDate"`
	Exceeded             bool    `json:"exceeded"`   // current already past target
	OverIncome           bool    `json:"overIncome"` // monthly plan exceeds monthly income
}

// monthlyIncomeUSD converts the user's stated monthly income to USD (0 if unset).
func (s *Server) monthlyIncomeUSD(uid uint, rates calc.Rates) float64 {
	var u model.User
	if s.db.First(&u, uid).Error != nil || u.MonthlyIncome <= 0 {
		return 0
	}
	cur := u.IncomeCurrency
	if cur == "" {
		cur = "USD"
	}
	return calc.Convert(u.MonthlyIncome, cur, "USD", rates)
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
	incomeUSD := s.monthlyIncomeUSD(uid, rates)
	var goals []model.Goal
	if err := s.db.Where("user_id = ?", uid).Order("created_at").Find(&goals).Error; err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "db error")
	}
	out := make([]goalResp, 0, len(goals))
	for _, g := range goals {
		out = append(out, s.buildGoalResp(uid, g, netUSD, rates, incomeUSD))
	}
	// Debts flagged "show as goal" appear as payoff goals (progress toward zero).
	var debts []model.Asset
	s.db.Where("user_id = ? AND kind = ? AND show_as_goal = ?", uid, model.KindDebt, true).Order("created_at").Find(&debts)
	for _, d := range debts {
		out = append(out, s.debtGoalResp(uid, d, rates))
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
		LinkedAssetID: s.validLinkedAsset(uid, in.LinkedAssetID),
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
	g.LinkedAssetID = s.validLinkedAsset(uid, in.LinkedAssetID)
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
	return c.JSON(status, s.buildGoalResp(uid, g, netUSD, rates, s.monthlyIncomeUSD(uid, rates)))
}

// validLinkedAsset returns id only if it points to one of the user's own assets;
// otherwise nil (unlinked). Guards against linking someone else's asset.
func (s *Server) validLinkedAsset(uid uint, id *uint) *uint {
	if id == nil || *id == 0 {
		return nil
	}
	var n int64
	s.db.Model(&model.Asset{}).Where("id = ? AND user_id = ?", *id, uid).Count(&n)
	if n == 0 {
		return nil
	}
	return id
}

func (s *Server) buildGoalResp(uid uint, g model.Goal, netUSD float64, rates calc.Rates, incomeUSD float64) goalResp {
	// When linked to a savings pot (account/deposit), progress is that asset's
	// balance; otherwise it falls back to total net worth (the "capital" goal).
	var current float64
	resp := goalResp{Goal: g, GoalKind: "capital"}
	if g.LinkedAssetID != nil {
		resp.GoalKind = "savings"
		var a model.Asset
		if s.db.Where("id = ? AND user_id = ?", *g.LinkedAssetID, uid).First(&a).Error == nil {
			current = calc.Compute(a, g.Currency, rates, time.Now()).ValueBase
			resp.LinkedAssetName = a.Name
			resp.ContributedThisMonth = round2(s.contributedThisMonth(uid, *g.LinkedAssetID, g.Currency, rates))
		}
	} else {
		current = calc.Convert(netUSD, "USD", g.Currency, rates)
	}

	remaining := g.TargetAmount - current
	progress := 0.0
	if g.TargetAmount > 0 {
		progress = math.Max(0, current/g.TargetAmount*100)
	}
	resp.CurrentAmount = round2(current)
	resp.ProgressPercent = round2(progress)
	resp.Remaining = round2(remaining)
	resp.OnTrack = g.MonthlyContribution > 0 && resp.ContributedThisMonth >= g.MonthlyContribution
	resp.Exceeded = current > g.TargetAmount && g.TargetAmount > 0

	// Plan is unrealistic if it exceeds the user's monthly income.
	if g.MonthlyContribution > 0 && incomeUSD > 0 {
		incomeCur := calc.Convert(incomeUSD, "USD", g.Currency, rates)
		resp.OverIncome = g.MonthlyContribution > incomeCur
	}

	if remaining <= 0 {
		zero := 0
		resp.MonthsToGoal = &zero
	} else if g.MonthlyContribution > 0 {
		months := int(math.Ceil(remaining / g.MonthlyContribution))
		resp.MonthsToGoal = &months
		resp.TargetDate = time.Now().AddDate(0, months, 0).Format("2006-01-02")
	}
	return resp
}

// debtGoalResp turns a debt flagged "show as goal" into a payoff goal: target is
// the original amount owed, progress is how much has been paid off toward zero.
func (s *Server) debtGoalResp(uid uint, d model.Asset, rates calc.Rates) goalResp {
	outstanding := calc.Compute(d, d.Currency, rates, time.Now()).LiabilityBase

	monthStart := time.Now()
	monthStart = time.Date(monthStart.Year(), monthStart.Month(), 1, 0, 0, 0, 0, monthStart.Location())
	var paidTotal, paidThisMonth float64
	s.db.Model(&model.AccountEntry{}).
		Where("user_id = ? AND linked_debt_id = ? AND source = ?", uid, d.ID, "debt_payment").
		Select("COALESCE(SUM(debt_amount), 0)").Scan(&paidTotal)
	s.db.Model(&model.AccountEntry{}).
		Where("user_id = ? AND linked_debt_id = ? AND source = ? AND date >= ?", uid, d.ID, "debt_payment", monthStart).
		Select("COALESCE(SUM(debt_amount), 0)").Scan(&paidThisMonth)

	original := outstanding + paidTotal
	progress := 0.0
	if original > 0 {
		progress = paidTotal / original * 100
	}
	linkID := d.ID
	resp := goalResp{
		GoalKind:             "debt",
		CurrentAmount:        round2(paidTotal),
		ProgressPercent:      round2(progress),
		Remaining:            round2(outstanding),
		LinkedAssetName:      d.Name,
		ContributedThisMonth: round2(paidThisMonth),
		OnTrack:              d.MonthlyPayment > 0 && paidThisMonth >= d.MonthlyPayment,
	}
	resp.Goal = model.Goal{
		ID: d.ID, UserID: uid, Title: "Закрыть: " + d.Name, Currency: d.Currency,
		TargetAmount: round2(original), MonthlyContribution: d.MonthlyPayment, LinkedAssetID: &linkID,
	}
	if outstanding <= 0.5 {
		zero := 0
		resp.MonthsToGoal = &zero
	} else if d.MonthlyPayment > 0 {
		months := int(math.Ceil(outstanding / d.MonthlyPayment))
		resp.MonthsToGoal = &months
		resp.TargetDate = time.Now().AddDate(0, months, 0).Format("2006-01-02")
	}
	return resp
}

// contributedThisMonth sums income credited to a linked account since the start
// of the current month, in the goal's currency. Deposits funded via top-ups from
// another account won't have their own entries, so this reflects direct deposits.
func (s *Server) contributedThisMonth(uid, assetID uint, cur string, rates calc.Rates) float64 {
	now := time.Now()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	var acc model.Asset
	if s.db.Where("id = ? AND user_id = ?", assetID, uid).First(&acc).Error != nil {
		return 0
	}
	var sum float64
	s.db.Model(&model.AccountEntry{}).
		Where("user_id = ? AND account_id = ? AND kind = ? AND date >= ?", uid, assetID, "income", monthStart).
		Select("COALESCE(SUM(amount), 0)").Scan(&sum)
	return calc.Convert(sum, acc.Currency, cur, rates)
}
