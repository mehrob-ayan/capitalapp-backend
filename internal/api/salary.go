package api

import (
	"fmt"
	"math"
	"time"

	"capitalapp/internal/calc"
	"capitalapp/internal/model"
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
