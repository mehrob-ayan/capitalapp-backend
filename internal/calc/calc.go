// Package calc holds all financial computation: currency conversion, per-asset
// metrics and loan amortization. Everything here is pure and unit-testable.
package calc

import (
	"math"
	"time"

	"capitalapp/internal/model"
)

// Rates maps a currency code to the value of 1 unit in USD.
type Rates map[string]float64

// DefaultRates seeds a new user's manual rates (value of 1 unit in USD).
// TJS ≈ 1/9.4, UZS ≈ 1/12600 — the user edits these in settings.
func DefaultRates() Rates {
	return Rates{
		"USD": 1,
		"TJS": 0.106383,
		"UZS": 0.0000794,
	}
}

// Convert changes an amount from one currency to another. Unknown currencies
// yield 0 (caller should validate currencies on input).
func Convert(amount float64, from, to string, r Rates) float64 {
	rf, rt := r[from], r[to]
	if rf == 0 || rt == 0 {
		return 0
	}
	return amount * rf / rt
}

// AssetMetrics is everything derived for a single asset. Base-currency figures
// feed aggregation; the per-asset percentages feed the position screen.
type AssetMetrics struct {
	ValueBase        float64    `json:"valueBase"`       // asset contribution (0 for debt)
	AccruedValue     float64    `json:"accruedValue"`    // current value in the asset's OWN currency (deposits accrue; others = entered value)
	LiabilityBase    float64    `json:"liabilityBase"`   // debt outstanding (0 otherwise)
	MonthlyFlowBase  float64    `json:"monthlyFlowBase"` // signed monthly cash flow
	Profit           float64    `json:"profit"`          // value - invested (asset currency)
	ProfitBase       float64    `json:"profitBase"`      // same, converted to base currency
	ProfitPercent    float64    `json:"profitPercent"`
	CashYieldPercent float64    `json:"cashYieldPercent"` // кешфлоу: income*12 / invested
	CagrPercent      float64    `json:"cagrPercent"`      // доходность: annualised total growth
	Loan             *LoanState `json:"loan,omitempty"`
}

// Compute derives metrics for one asset in the given base currency, as of asOf.
func Compute(a model.Asset, base string, r Rates, asOf time.Time) AssetMetrics {
	conv := func(v float64) float64 { return Convert(v, a.Currency, base, r) }
	m := AssetMetrics{AccruedValue: a.Value}

	switch a.Kind {
	case model.KindDeposit:
		// Interest compounds into the balance (капитализация): the deposit grows
		// daily, so the yield shows up as value growth rather than separate flow.
		accrued := AccrueBalance(a.Value, a.RatePercent, a.BalanceAsOf, asOf)
		m.AccruedValue = accrued
		m.ValueBase = conv(accrued)
		m.CashYieldPercent = a.RatePercent

	case model.KindCash:
		m.ValueBase = conv(a.Value)

	case model.KindLent:
		// Money lent out — a receivable. Counts as an asset; the balance falls as
		// it's repaid (money comes back into an account).
		m.ValueBase = conv(a.Value)
		m.AccruedValue = a.Value

	case model.KindRealEstate, model.KindInvestment, model.KindCar, model.KindMetals:
		m.ValueBase = conv(a.Value)
		m.MonthlyFlowBase = conv(a.MonthlyIncome)
		m.Profit = a.Value - a.Invested
		if a.Invested > 0 {
			m.ProfitBase = conv(m.Profit)
			m.ProfitPercent = m.Profit / a.Invested * 100
			m.CashYieldPercent = a.MonthlyIncome * 12 / a.Invested * 100
			m.CagrPercent = cagr(a.Invested, a.Value, a.PurchaseDate, asOf)
		}

	case model.KindDebt:
		ls := debtState(a, asOf)
		m.Loan = &ls
		m.LiabilityBase = conv(ls.Outstanding)
		m.MonthlyFlowBase = -conv(ls.MonthlyPayment)
	}

	return m
}

// DebtScheme selects how a debt's outstanding balance behaves over time,
// matching the product the user's bank actually offers.
type DebtScheme string

const (
	SchemeAccruing       DebtScheme = "accruing"       // daily interest, manual paydown
	SchemeAnnuity        DebtScheme = "annuity"        // equal payments on a schedule
	SchemeDifferentiated DebtScheme = "differentiated" // equal principal on a schedule
	SchemeInterestFree   DebtScheme = "interestfree"   // 0% installment / private debt
)

// debtState computes a debt's current state according to its scheme. An empty
// scheme is treated as accruing (also correct for 0% debts, which simply don't grow).
func debtState(a model.Asset, asOf time.Time) LoanState {
	switch DebtScheme(a.DebtScheme) {
	case SchemeAnnuity:
		return Amortize(a.Value, a.RatePercent, a.TermMonths, Annuity, firstPay(a, asOf), asOf)
	case SchemeDifferentiated:
		return Amortize(a.Value, a.RatePercent, a.TermMonths, Differentiated, firstPay(a, asOf), asOf)
	case SchemeInterestFree:
		return LoanFromBalance(a.Value, 0, a.MonthlyPayment, asOf)
	default: // accruing
		accrued := AccrueBalance(a.Value, a.RatePercent, a.BalanceAsOf, asOf)
		return LoanFromBalance(accrued, a.RatePercent, a.MonthlyPayment, asOf)
	}
}

func firstPay(a model.Asset, asOf time.Time) time.Time {
	if a.FirstPaymentDate != nil {
		return *a.FirstPaymentDate
	}
	return asOf
}

// cagr is the compound annual growth rate between invested and current value.
func cagr(invested, current float64, purchase *time.Time, asOf time.Time) float64 {
	if invested <= 0 || current <= 0 || purchase == nil {
		return 0
	}
	years := asOf.Sub(*purchase).Hours() / 24 / 365.25
	if years < 0.08 { // < ~1 month: not meaningful to annualise
		return 0
	}
	return (math.Pow(current/invested, 1/years) - 1) * 100
}
