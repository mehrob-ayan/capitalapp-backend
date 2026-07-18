package calc

import (
	"math"
	"testing"
	"time"
)

func approx(t *testing.T, name string, got, want, eps float64) {
	t.Helper()
	if math.Abs(got-want) > eps {
		t.Errorf("%s = %.4f, want %.4f (±%.4f)", name, got, want, eps)
	}
}

var sep2021 = time.Date(2021, 9, 1, 0, 0, 0, 0, time.UTC)

func TestAnnuityPayment(t *testing.T) {
	// 1,000,000 @ 12% annual, 12 months → known annuity payment ≈ 88,848.79
	ls := Amortize(1_000_000, 12, 12, Annuity, sep2021, sep2021)
	approx(t, "payment", ls.MonthlyPayment, 88848.79, 0.5)
	approx(t, "outstanding@0", ls.Outstanding, 1_000_000, 0.01)
	approx(t, "interest@0", ls.InterestPart, 10_000, 0.5)
	approx(t, "principal@0", ls.PrincipalPart, 78848.79, 0.5)
	if ls.PaidMonths != 0 {
		t.Errorf("paidMonths = %d, want 0", ls.PaidMonths)
	}
}

func TestAnnuityPaysOff(t *testing.T) {
	// After the full term the balance must reach ~0.
	end := sep2021.AddDate(0, 12, 0)
	ls := Amortize(1_000_000, 12, 12, Annuity, sep2021, end)
	approx(t, "outstanding@term", ls.Outstanding, 0, 1)
	if ls.RemainingMonths != 0 {
		t.Errorf("remaining = %d, want 0", ls.RemainingMonths)
	}
}

func TestAnnuityMonotonicDecrease(t *testing.T) {
	prev := math.Inf(1)
	for k := 0; k <= 12; k++ {
		ls := Amortize(1_000_000, 12, 12, Annuity, sep2021, sep2021.AddDate(0, k, 0))
		if ls.Outstanding > prev+0.01 {
			t.Fatalf("balance increased at month %d: %.2f > %.2f", k, ls.Outstanding, prev)
		}
		prev = ls.Outstanding
	}
}

func TestDifferentiated(t *testing.T) {
	// 1,200,000 @ 12%, 12 months. Equal principal = 100,000.
	ls := Amortize(1_200_000, 12, 12, Differentiated, sep2021, sep2021)
	approx(t, "principalPart", ls.PrincipalPart, 100_000, 0.01)
	approx(t, "interest@0", ls.InterestPart, 12_000, 0.01)
	approx(t, "payment@0", ls.MonthlyPayment, 112_000, 0.01)
	// total interest = i*P*(n+1)/2 = 0.01*1.2M*13/2 = 78,000
	approx(t, "totalInterest", ls.TotalInterest, 78_000, 0.01)
}

func TestZeroInterest(t *testing.T) {
	ls := Amortize(120_000, 0, 12, Annuity, sep2021, sep2021.AddDate(0, 6, 0))
	approx(t, "payment", ls.MonthlyPayment, 10_000, 0.01)
	approx(t, "outstanding@6", ls.Outstanding, 60_000, 0.01)
	approx(t, "totalInterest", ls.TotalInterest, 0, 0.01)
}

func TestMonthsElapsed(t *testing.T) {
	got := monthsElapsed(
		time.Date(2021, 9, 15, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 18, 0, 0, 0, 0, time.UTC),
	)
	if got != 58 {
		t.Errorf("monthsElapsed = %d, want 58", got)
	}
	// Before first payment → 0.
	if got := monthsElapsed(sep2021, sep2021.AddDate(0, -1, 0)); got != 0 {
		t.Errorf("monthsElapsed(before) = %d, want 0", got)
	}
}

func TestLoanFromBalance(t *testing.T) {
	// Mortgage as the user actually knows it: 330 000 owed now, 12%/yr, pays 5057.
	ls := LoanFromBalance(330000, 12, 5057, sep2021)
	approx(t, "outstanding", ls.Outstanding, 330000, 0.01)
	approx(t, "interest", ls.InterestPart, 3300, 0.01)   // 330000 * 0.01
	approx(t, "principal", ls.PrincipalPart, 1757, 0.01) // 5057 - 3300
	if ls.RemainingMonths != 107 {
		t.Errorf("remaining = %d, want 107", ls.RemainingMonths)
	}
	// Interest-free debt with no payment: static balance, no schedule.
	st := LoanFromBalance(6500, 0, 0, sep2021)
	approx(t, "static outstanding", st.Outstanding, 6500, 0.01)
	if st.MonthlyPayment != 0 || st.RemainingMonths != 0 {
		t.Errorf("static debt should have no schedule, got payment=%.2f remaining=%d", st.MonthlyPayment, st.RemainingMonths)
	}
	// Payment below monthly interest never amortizes.
	never := LoanFromBalance(1_000_000, 12, 100, sep2021)
	if never.RemainingMonths != 0 {
		t.Errorf("under-interest payment should not amortize, got remaining=%d", never.RemainingMonths)
	}
}

func TestAccrueBalance(t *testing.T) {
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// 30 days at 12%/yr, daily compounded: 333000 * (1 + 0.12/365)^30 ≈ 336300
	approx(t, "30d", AccrueBalance(333000, 12, from, from.AddDate(0, 0, 30)), 336300.09, 1)
	// No time elapsed → unchanged.
	approx(t, "0d", AccrueBalance(333000, 12, from, from), 333000, 0.01)
	// No rate → unchanged (interest-free debt does not grow).
	approx(t, "0rate", AccrueBalance(12840, 0, from, from.AddDate(0, 1, 0)), 12840, 0.01)
	// No anchor date → unchanged.
	approx(t, "noanchor", AccrueBalance(6500, 12, time.Time{}, from), 6500, 0.01)
}

func TestConvert(t *testing.T) {
	r := DefaultRates()
	approx(t, "same", Convert(100, "USD", "USD", r), 100, 0.001)
	approx(t, "tjs→usd", Convert(100, "TJS", "USD", r), 10.6383, 0.001)
	approx(t, "usd→tjs", Convert(1, "USD", "TJS", r), 9.4, 0.01)
	if got := Convert(100, "XXX", "USD", r); got != 0 {
		t.Errorf("unknown currency = %.2f, want 0", got)
	}
}
