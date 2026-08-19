package api

import (
	"net/http"
	"time"

	"capitalapp/internal/calc"
	"capitalapp/internal/model"

	"github.com/labstack/echo/v4"
)

type effMonth struct {
	Month    string  `json:"month"`
	Growth   float64 `json:"growth"`   // capital change that month, base
	Share    float64 `json:"share"`    // growth ÷ income, %
	CapShare float64 `json:"capShare"` // capitalization rate that month (0–100), from the salary account
}

type efficiencyResp struct {
	BaseCurrency  string     `json:"baseCurrency"`
	HasIncome     bool       `json:"hasIncome"`
	MonthlyIncome float64    `json:"monthlyIncome"`
	MonthExpenses float64    `json:"monthExpenses"`
	CapitalGrowth float64    `json:"capitalGrowth"` // ≈ last 30 days, base currency (factual)
	Inflow        float64    `json:"inflow"`        // money that landed on the salary account this month
	Spent         float64    `json:"spent"`         // manual withdrawals (spending) this month
	CapitalShare  float64    `json:"capitalShare"`  // (inflow − spent) ÷ inflow, % (0–100)
	SavingsRate   *float64   `json:"savingsRate"`   // (income − expenses) ÷ income, %; null if expenses not tracked

	// Financial-health metrics (all base currency, monthly unless noted).
	DebtPaymentsMonthly   float64 `json:"debtPaymentsMonthly"`
	DebtLoadPct           float64 `json:"debtLoadPct"`   // debt payments ÷ income, %
	InterestPaidMonthly   float64 `json:"interestPaidMonthly"`
	InterestEarnedMonthly float64 `json:"interestEarnedMonthly"`
	NetInterestMonthly    float64 `json:"netInterestMonthly"` // earned − paid
	Liquid                float64 `json:"liquid"`             // cash accounts + deposits
	MonthlyBurn           float64 `json:"monthlyBurn"`        // debt payments + tracked expenses
	RunwayMonths          float64 `json:"runwayMonths"`       // liquid ÷ burn (0 if no burn)
	Leverage              float64 `json:"leverage"`           // liabilities ÷ assets, %

	Trend []effMonth `json:"trend"` // capital added + capitalization % per month
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
func (s *Server) efficiencyTrend(uid uint, base string, rates calc.Rates, income float64, salaryAccID *uint) []effMonth {
	now := time.Now()
	thisMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)

	var earliest model.Snapshot
	if s.db.Where("user_id = ?", uid).Order("date asc").First(&earliest).Error != nil {
		return nil
	}
	earliestNW, _, _ := snapshotIn(earliest, base, rates)

	// Salary account currency, for the per-month capitalization rate.
	var salCur string
	if salaryAccID != nil {
		var acc model.Asset
		if s.db.Where("id = ? AND user_id = ?", *salaryAccID, uid).First(&acc).Error == nil {
			salCur = acc.Currency
		}
	}

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
		e := effMonth{Month: mStart.Format("2006-01"), Growth: growth, Share: share}

		// Capitalization rate that month from the salary account ledger.
		if salaryAccID != nil {
			var entries []model.AccountEntry
			s.db.Where("user_id = ? AND account_id = ? AND date >= ? AND date < ?", uid, *salaryAccID, mStart, mEnd).Find(&entries)
			var in, sp float64
			for _, en := range entries {
				amt := calc.Convert(en.Amount, salCur, base, rates)
				if en.Kind == "income" && en.Source != "opening" {
					in += amt
				} else if en.Kind == "payment" && en.Source == "manual" {
					sp += amt
				}
			}
			if in > 0 {
				cs := (in - sp) / in * 100
				if cs < 0 {
					cs = 0
				}
				if cs > 100 {
					cs = 100
				}
				e.CapShare = round2(cs)
			}
		}
		out = append(out, e)
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

	// Capitalization rate from the salary account: of the money that came in
	// this month, what share stayed as capital (didn't get spent). Debt payments
	// are NOT spending — they convert cash into a smaller liability. Only manual
	// withdrawals count as spending. Robust to one-offs like vacation pay, which
	// raise both the inflow and (if unspent) the retained amount.
	var inflow, spent float64
	if user.SalaryAccountID != nil {
		var acc model.Asset
		if s.db.Where("id = ? AND user_id = ? AND is_account = ?", *user.SalaryAccountID, uid, true).First(&acc).Error == nil {
			var entries []model.AccountEntry
			s.db.Where("user_id = ? AND account_id = ? AND date >= ?", uid, acc.ID, mStart).Find(&entries)
			for _, e := range entries {
				amt := calc.Convert(e.Amount, acc.Currency, base, rates)
				switch {
				case e.Kind == "income" && e.Source != "opening":
					inflow += amt
				case e.Kind == "payment" && e.Source == "manual":
					spent += amt
				}
			}
		}
	}

	// Portfolio aggregates for the health metrics.
	var assets []model.Asset
	s.db.Where("user_id = ?", uid).Find(&assets)
	var totalAssets, totalLiab, debtPay, interestPaid, interestEarned, liquid float64
	for _, a := range assets {
		m := calc.Compute(a, base, rates, now)
		totalAssets += m.ValueBase
		totalLiab += m.LiabilityBase
		switch a.Kind {
		case model.KindDebt:
			if m.Loan != nil {
				debtPay += calc.Convert(m.Loan.MonthlyPayment, a.Currency, base, rates)
				interestPaid += calc.Convert(m.Loan.InterestPart, a.Currency, base, rates)
			}
		case model.KindDeposit:
			if a.RatePercent > 0 {
				interestEarned += calc.Convert(m.AccruedValue*a.RatePercent/100/12, a.Currency, base, rates)
			}
			liquid += m.ValueBase
		case model.KindCash:
			liquid += m.ValueBase
		}
	}
	burn := debtPay + expenses

	resp := efficiencyResp{
		BaseCurrency:  base,
		HasIncome:     income > 0 || inflow > 0,
		MonthlyIncome: round2(income),
		MonthExpenses: round2(expenses),
		CapitalGrowth: growth,
		Inflow:        round2(inflow),
		Spent:         round2(spent),

		DebtPaymentsMonthly:   round2(debtPay),
		InterestPaidMonthly:   round2(interestPaid),
		InterestEarnedMonthly: round2(interestEarned),
		NetInterestMonthly:    round2(interestEarned - interestPaid),
		Liquid:                round2(liquid),
		MonthlyBurn:           round2(burn),
	}
	if inflow > 0 {
		share := (inflow - spent) / inflow * 100
		if share < 0 {
			share = 0
		}
		if share > 100 {
			share = 100
		}
		resp.CapitalShare = round2(share)
	}
	if income > 0 && expenses > 0 {
		sr := round2((income - expenses) / income * 100)
		resp.SavingsRate = &sr
	}
	loadBase := income
	if loadBase <= 0 {
		loadBase = inflow
	}
	if loadBase > 0 {
		resp.DebtLoadPct = round2(debtPay / loadBase * 100)
	}
	if burn > 0 {
		resp.RunwayMonths = round2(liquid / burn)
	}
	if totalAssets > 0 {
		resp.Leverage = round2(totalLiab / totalAssets * 100)
	}
	resp.Trend = s.efficiencyTrend(uid, base, rates, income, user.SalaryAccountID)
	return c.JSON(http.StatusOK, resp)
}
