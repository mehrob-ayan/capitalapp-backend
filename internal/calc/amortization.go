package calc

import (
	"math"
	"time"
)

type LoanType string

const (
	Annuity        LoanType = "annuity"        // equal payments; interest falls, principal rises
	Differentiated LoanType = "differentiated" // equal principal; payment falls over time
)

// LoanState is a loan's situation as of a given date.
type LoanState struct {
	Outstanding     float64   `json:"outstanding"`     // remaining balance today
	MonthlyPayment  float64   `json:"monthlyPayment"`  // next scheduled payment
	InterestPart    float64   `json:"interestPart"`    // interest portion of that payment
	PrincipalPart   float64   `json:"principalPart"`   // principal portion of that payment
	PaidMonths      int       `json:"paidMonths"`      // payments already made
	RemainingMonths int       `json:"remainingMonths"` // payments left
	TotalInterest   float64   `json:"totalInterest"`   // total interest over the whole loan
	PayoffDate      time.Time `json:"payoffDate"`
}

// Amortize computes a loan's state as of asOf.
//
//	principal     original amount borrowed
//	annualRatePct nominal annual rate, e.g. 8 for 8%
//	term          total number of monthly payments
//	firstPayment  date of the first payment (month index 0)
//
// Convention: PaidMonths is the number of completed monthly periods since
// firstPayment (0 on the first-payment date itself); Outstanding is the balance
// after that many payments.
func Amortize(principal, annualRatePct float64, term int, lt LoanType, firstPayment, asOf time.Time) LoanState {
	if principal <= 0 || term <= 0 {
		return LoanState{Outstanding: math.Max(principal, 0), PayoffDate: firstPayment}
	}

	i := annualRatePct / 100 / 12
	k := monthsElapsed(firstPayment, asOf)
	if k < 0 {
		k = 0
	}
	if k > term {
		k = term
	}
	payoff := firstPayment.AddDate(0, term, 0)

	if lt == Differentiated {
		principalPart := principal / float64(term)
		outstanding := principal - principalPart*float64(k)
		if outstanding < 0 {
			outstanding = 0
		}
		interest := outstanding * i
		payment := principalPart + interest
		if outstanding == 0 {
			payment, interest, principalPart = 0, 0, 0
		}
		// sum of interest over the life = i * P * (term+1) / 2
		total := i * principal * float64(term+1) / 2
		return LoanState{
			Outstanding: outstanding, MonthlyPayment: payment,
			InterestPart: interest, PrincipalPart: principalPart,
			PaidMonths: k, RemainingMonths: term - k,
			TotalInterest: total, PayoffDate: payoff,
		}
	}

	// Annuity (default)
	var payment float64
	if i == 0 {
		payment = principal / float64(term)
	} else {
		pow := math.Pow(1+i, float64(term))
		payment = principal * i * pow / (pow - 1)
	}

	var outstanding float64
	if i == 0 {
		outstanding = principal - payment*float64(k)
	} else {
		powk := math.Pow(1+i, float64(k))
		outstanding = principal*powk - payment*(powk-1)/i
	}
	if outstanding < 0 {
		outstanding = 0
	}

	interest := outstanding * i
	principalPart := payment - interest
	if principalPart < 0 {
		principalPart = 0
	}
	if outstanding == 0 {
		payment, interest, principalPart = 0, 0, 0
	}

	total := payment*float64(term) - principal
	if i == 0 {
		total = 0
	}
	if total < 0 {
		total = 0
	}

	return LoanState{
		Outstanding: outstanding, MonthlyPayment: payment,
		InterestPart: interest, PrincipalPart: principalPart,
		PaidMonths: k, RemainingMonths: term - k,
		TotalInterest: total, PayoffDate: payoff,
	}
}

// AccrueBalance grows a debt balance by daily-compounded interest from `from`
// to `asOf`. This mirrors how a bank accrues interest each day on the amount
// owed. With no rate, no anchor date, or a future anchor, the balance is
// returned unchanged.
func AccrueBalance(principal, annualRatePct float64, from, asOf time.Time) float64 {
	if principal <= 0 || annualRatePct <= 0 || from.IsZero() || !asOf.After(from) {
		return math.Max(principal, 0)
	}
	days := asOf.Sub(from).Hours() / 24
	daily := annualRatePct / 100 / 365
	return principal * math.Pow(1+daily, days)
}

// LoanFromBalance derives a loan's state from what the borrower actually knows:
// the current balance and monthly payment. This is robust to early/extra
// repayments (where an origination-date schedule no longer holds) and also
// covers interest-free debts (payment or rate may be zero).
func LoanFromBalance(balance, annualRatePct, payment float64, asOf time.Time) LoanState {
	ls := LoanState{Outstanding: math.Max(balance, 0)}
	if balance <= 0 {
		return ls
	}
	i := annualRatePct / 100 / 12
	ls.InterestPart = balance * i

	if payment <= 0 {
		return ls // static debt, no schedule
	}
	ls.MonthlyPayment = payment
	ls.PrincipalPart = math.Max(payment-ls.InterestPart, 0)

	var n float64
	switch {
	case i == 0:
		n = math.Ceil(balance / payment)
	case payment > balance*i:
		n = math.Ceil(-math.Log(1-balance*i/payment) / math.Log(1+i))
	default:
		return ls // payment doesn't cover interest — never amortizes
	}
	ls.RemainingMonths = int(n)
	ls.PayoffDate = asOf.AddDate(0, int(n), 0)
	ls.TotalInterest = math.Max(payment*n-balance, 0)
	return ls
}

// monthsElapsed returns the number of complete calendar months from `from` to
// `asOf` (0 if asOf is before from).
func monthsElapsed(from, asOf time.Time) int {
	if asOf.Before(from) {
		return 0
	}
	total := (asOf.Year()-from.Year())*12 + int(asOf.Month()) - int(from.Month())
	if asOf.Day() < from.Day() {
		total--
	}
	if total < 0 {
		total = 0
	}
	return total
}
