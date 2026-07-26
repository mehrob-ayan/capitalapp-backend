package api

import (
	"net/http"
	"time"

	"capitalapp/internal/calc"
	"capitalapp/internal/model"

	"github.com/labstack/echo/v4"
)

type effMonth struct {
	Month  string  `json:"month"`
	Growth float64 `json:"growth"` // capital change that month, base
	Share  float64 `json:"share"`  // growth ÷ income, %
}

type efficiencyResp struct {
	BaseCurrency  string     `json:"baseCurrency"`
	HasIncome     bool       `json:"hasIncome"`
	MonthlyIncome float64    `json:"monthlyIncome"`
	MonthExpenses float64    `json:"monthExpenses"`
	CapitalGrowth float64    `json:"capitalGrowth"` // ≈ last 30 days, base currency
	CapitalShare  float64    `json:"capitalShare"`  // growth ÷ income, %
	SavingsRate   *float64   `json:"savingsRate"`   // (income − expenses) ÷ income, %; null if expenses not tracked
	Trend         []effMonth `json:"trend"`         // capital added per month, last months with data
}

// netWorthBefore returns net worth (base) from the latest snapshot strictly
// before `t`, and whether one existed.
func (s *Server) netWorthBefore(uid uint, base string, rates calc.Rates, t time.Time) (float64, bool) {
	var sn model.Snapshot
	if s.db.Where("user_id = ? AND date < ?", uid, t).Order("date desc").First(&sn).Error != nil {
		return 0, false
	}
	nw, _, _ := snapshotIn(sn, base, rates)
	return nw, true
}

// efficiencyTrend builds "capital added per month" for the last 6 months. A
// month is included only once there's snapshot data in it; the first month with
// data is measured from the earliest snapshot.
func (s *Server) efficiencyTrend(uid uint, base string, rates calc.Rates, income float64) []effMonth {
	now := time.Now()
	thisMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)

	var earliest model.Snapshot
	if s.db.Where("user_id = ?", uid).Order("date asc").First(&earliest).Error != nil {
		return nil
	}
	earliestNW, _, _ := snapshotIn(earliest, base, rates)

	out := make([]effMonth, 0, 6)
	for i := 5; i >= 0; i-- {
		mStart := thisMonth.AddDate(0, -i, 0)
		mEnd := mStart.AddDate(0, 1, 0)
		nwEnd, ok := s.netWorthBefore(uid, base, rates, mEnd)
		if !ok {
			continue // no data by end of this month yet
		}
		nwStart, had := s.netWorthBefore(uid, base, rates, mStart)
		if !had {
			nwStart = earliestNW // first month of tracking
		}
		growth := round2(nwEnd - nwStart)
		share := 0.0
		if income > 0 {
			share = round2(growth / income * 100)
		}
		out = append(out, effMonth{Month: mStart.Format("2006-01"), Growth: growth, Share: share})
	}
	return out
}

// efficiency answers "how much of my income becomes capital". Income is the
// user's recurring monthly figure; growth is the ~last-30-days change in net
// worth from snapshots. capitalShare mixes savings with market/FX moves (shown
// honestly in the UI); savingsRate is the cleaner income−spend ratio when
// expenses are tracked.
func (s *Server) efficiency(c echo.Context) error {
	uid := c.Get(ctxUserID).(uint)
	user, err := s.loadUser(uid)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "user not found")
	}
	base := baseOr(user.BaseCurrency)
	rates, err := s.userRates(uid)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "db error")
	}

	income := 0.0
	if user.MonthlyIncome > 0 {
		ic := user.IncomeCurrency
		if ic == "" {
			ic = base
		}
		income = calc.Convert(user.MonthlyIncome, ic, base, rates)
	}

	now := time.Now()
	mStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	var txs []model.Transaction
	s.db.Where("user_id = ? AND type = 'expense' AND date >= ?", uid, mStart).Find(&txs)
	var expenses float64
	for _, t := range txs {
		expenses += calc.Convert(t.Amount, t.Currency, base, rates)
	}

	growth := 0.0
	var latest model.Snapshot
	if s.db.Where("user_id = ?", uid).Order("date desc").First(&latest).Error == nil {
		var past model.Snapshot
		cutoff := latest.Date.AddDate(0, 0, -30)
		if s.db.Where("user_id = ? AND date <= ?", uid, cutoff).Order("date desc").First(&past).Error != nil {
			s.db.Where("user_id = ?", uid).Order("date asc").First(&past) // history < 30d: use earliest
		}
		nwL, _, _ := snapshotIn(latest, base, rates)
		nwP, _, _ := snapshotIn(past, base, rates)
		growth = round2(nwL - nwP)
	}

	resp := efficiencyResp{
		BaseCurrency:  base,
		HasIncome:     income > 0,
		MonthlyIncome: round2(income),
		MonthExpenses: round2(expenses),
		CapitalGrowth: growth,
	}
	if income > 0 {
		resp.CapitalShare = round2(growth / income * 100)
		if expenses > 0 {
			sr := round2((income - expenses) / income * 100)
			resp.SavingsRate = &sr
		}
	}
	resp.Trend = s.efficiencyTrend(uid, base, rates, income)
	return c.JSON(http.StatusOK, resp)
}
