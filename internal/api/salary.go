package api

import (
	"fmt"
	"math"
	"net/http"
	"time"

	"capitalapp/internal/calc"
	"capitalapp/internal/model"

	"github.com/labstack/echo/v4"
)

// postSalary auto-credits the salary account: an advance around the 15th and the
// remainder at month-end, for every payday from the account's creation up to
// today that hasn't been posted yet. Idempotent via PayKey, so it safely catches
// up any paydays missed while the machine was off — without ever double-posting.
func (s *Server) postSalary(uid uint) {
	var u model.User
	if s.db.First(&u, uid).Error != nil || u.SalaryAccountID == nil || u.MonthlyIncome <= 0 {
		return
	}
	var acc model.Asset
	if s.db.Where("id = ? AND user_id = ? AND is_account = ?", *u.SalaryAccountID, uid, true).First(&acc).Error != nil {
		return
	}
	rates, err := s.userRates(uid)
	if err != nil {
		return
	}
	ic := u.IncomeCurrency
	if ic == "" {
		ic = acc.Currency
	}
	monthly := calc.Convert(u.MonthlyIncome, ic, acc.Currency, rates)
	if monthly <= 0 {
		return
	}

	from := acc.CreatedAt.UTC().Truncate(24 * time.Hour)
	today := time.Now().UTC().Truncate(24 * time.Hour)
	changed := false

	y, m := from.Year(), int(from.Month())
	for {
		if time.Date(y, time.Month(m), 1, 0, 0, 0, 0, time.UTC).After(today) {
			break
		}
		adv, rem := salarySplit(y, m, monthly)
		advDate := prevFriday(time.Date(y, time.Month(m), 15, 12, 0, 0, 0, time.UTC))
		remDate := prevFriday(time.Date(y, time.Month(m), daysInMonth(y, m), 12, 0, 0, 0, time.UTC))
		if s.postSalaryEntry(uid, acc, fmt.Sprintf("%04d-%02d-adv", y, m), advDate, adv, "Аванс", from, today) {
			changed = true
		}
		if s.postSalaryEntry(uid, acc, fmt.Sprintf("%04d-%02d-rem", y, m), remDate, rem, "Зарплата (остаток)", from, today) {
			changed = true
		}
		m++
		if m > 12 {
			m = 1
			y++
		}
	}
	if changed {
		s.recomputeAccount(uid, acc.ID)
	}
}

// ---- salary schedule projection (read-only calendar) ----

type schedPayday struct {
	Date   string  `json:"date"`
	Amount float64 `json:"amount"`
	Posted bool    `json:"posted"`
}
type schedMonth struct {
	Year      int          `json:"year"`
	Month     int          `json:"month"`
	Advance   *schedPayday `json:"advance"`
	Remainder *schedPayday `json:"remainder"`
}
type salarySchedResp struct {
	HasSalary bool         `json:"hasSalary"`
	Currency  string       `json:"currency"`
	Monthly   float64      `json:"monthly"`
	Total12   float64      `json:"total12"`
	Months    []schedMonth `json:"months"`
}

// salarySchedule projects the next ~12 months of paydays (advance + remainder,
// weekend-shifted) using the exact same split as the auto-crediting worker, so
// the calendar shows what will actually land. Only upcoming paydays are listed;
// already-passed ones are dropped (the worker posts those).
func (s *Server) salarySchedule(c echo.Context) error {
	uid := c.Get(ctxUserID).(uint)
	var u model.User
	if s.db.First(&u, uid).Error != nil {
		return echo.NewHTTPError(http.StatusNotFound, "user not found")
	}
	if u.MonthlyIncome <= 0 {
		return c.JSON(http.StatusOK, salarySchedResp{HasSalary: false})
	}
	rates, err := s.userRates(uid)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "db error")
	}

	// Amount is shown in the salary account's currency when one is set,
	// otherwise in the income currency.
	cur := u.IncomeCurrency
	var acc *model.Asset
	if u.SalaryAccountID != nil {
		var a model.Asset
		if s.db.Where("id = ? AND user_id = ? AND is_account = ?", *u.SalaryAccountID, uid, true).First(&a).Error == nil {
			acc = &a
			cur = a.Currency
		}
	}
	if cur == "" {
		cur = "USD"
	}
	monthly := u.MonthlyIncome
	if u.IncomeCurrency != "" && u.IncomeCurrency != cur {
		monthly = calc.Convert(u.MonthlyIncome, u.IncomeCurrency, cur, rates)
	}
	monthly = round2(monthly)

	// Which paydays are already posted (only meaningful when an account exists).
	posted := map[string]bool{}
	if acc != nil {
		var keys []string
		s.db.Model(&model.AccountEntry{}).Where("user_id = ? AND pay_key <> ''", uid).Pluck("pay_key", &keys)
		for _, k := range keys {
			posted[k] = true
		}
	}

	today := time.Now().UTC().Truncate(24 * time.Hour)
	y, m := today.Year(), int(today.Month())
	months := make([]schedMonth, 0, 13)
	for i := 0; i < 13; i++ {
		adv, rem := salarySplit(y, m, monthly)
		advDate := prevFriday(time.Date(y, time.Month(m), 15, 12, 0, 0, 0, time.UTC))
		remDate := prevFriday(time.Date(y, time.Month(m), daysInMonth(y, m), 12, 0, 0, 0, time.UTC))
		entry := schedMonth{Year: y, Month: m}
		if adv > 0 && !advDate.Before(today) {
			entry.Advance = &schedPayday{Date: advDate.Format("2006-01-02"), Amount: round2(adv), Posted: posted[fmt.Sprintf("%04d-%02d-adv", y, m)]}
		}
		if rem > 0 && !remDate.Before(today) {
			entry.Remainder = &schedPayday{Date: remDate.Format("2006-01-02"), Amount: round2(rem), Posted: posted[fmt.Sprintf("%04d-%02d-rem", y, m)]}
		}
		if entry.Advance != nil || entry.Remainder != nil {
			months = append(months, entry)
		}
		if m++; m > 12 {
			m = 1
			y++
		}
	}

	return c.JSON(http.StatusOK, salarySchedResp{
		HasSalary: true, Currency: cur, Monthly: monthly, Total12: round2(monthly * 12), Months: months,
	})
}

// postSalaryEntry posts one payday if it falls in [from, today] and hasn't been
// posted before (checked by PayKey). Returns whether it posted.
func (s *Server) postSalaryEntry(uid uint, acc model.Asset, key string, day time.Time, amount float64, label string, from, today time.Time) bool {
	d := day.UTC().Truncate(24 * time.Hour)
	if d.Before(from) || d.After(today) || amount <= 0 {
		return false
	}
	var n int64
	s.db.Model(&model.AccountEntry{}).Where("user_id = ? AND pay_key = ?", uid, key).Count(&n)
	if n > 0 {
		return false
	}
	before, _ := s.netWorthUSD(uid)
	s.db.Create(&model.AccountEntry{
		UserID: uid, AccountID: acc.ID, Date: day, Kind: "income",
		Amount: round2(amount), Note: label, Source: "salary_auto", PayKey: key,
	})
	s.recomputeAccount(uid, acc.ID)
	s.logActivity(uid, "salary", label+" зачислен", fmtAmount(amount, acc.Currency), round2(amount), acc.Currency, before)
	return true
}

// salarySplit returns the advance and remainder for a month. Advance is
// proportional to working days in the first half (1–15), but never more than
// half the salary; the remainder makes the month whole.
func salarySplit(y, m int, monthly float64) (adv, rem float64) {
	last := daysInMonth(y, m)
	wf := workdays(y, m, 1, 15)
	wt := workdays(y, m, 1, last)
	if wt == 0 {
		return 0, monthly
	}
	adv = math.Round(monthly * float64(wf) / float64(wt))
	if half := math.Round(monthly / 2); adv > half {
		adv = half
	}
	return adv, monthly - adv
}

func workdays(y, m, a, b int) int {
	last := daysInMonth(y, m)
	if b > last {
		b = last
	}
	n := 0
	for d := a; d <= b; d++ {
		if wd := time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC).Weekday(); wd != time.Saturday && wd != time.Sunday {
			n++
		}
	}
	return n
}

func daysInMonth(y, m int) int {
	return time.Date(y, time.Month(m+1), 0, 0, 0, 0, 0, time.UTC).Day()
}

// prevFriday moves a weekend date back to the preceding Friday (payday rule).
func prevFriday(d time.Time) time.Time {
	for d.Weekday() == time.Saturday || d.Weekday() == time.Sunday {
		d = d.AddDate(0, 0, -1)
	}
	return d
}
