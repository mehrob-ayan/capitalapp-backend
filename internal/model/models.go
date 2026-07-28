package model

import "time"

// User is a Telegram-authenticated account. All financial data hangs off it.
type User struct {
	ID           uint   `gorm:"primaryKey" json:"id"`
	TelegramID   int64  `gorm:"uniqueIndex;not null" json:"telegramId"`
	Username     string `json:"username,omitempty"`
	FirstName    string `json:"firstName,omitempty"`
	LastName     string `json:"lastName,omitempty"`
	LanguageCode string `json:"languageCode,omitempty"`
	BaseCurrency string `gorm:"default:USD" json:"baseCurrency"`

	// AutoRates enables a daily rate fetch from an FX API; RatesSyncedAt is when
	// it last succeeded. Off by default — manual rates are the fallback.
	AutoRates     bool      `gorm:"default:false" json:"autoRates"`
	RatesSyncedAt time.Time `json:"ratesSyncedAt,omitempty"`

	// Recurring monthly income (salary). Drives the savings-rate / efficiency
	// view and the "monthly flow" figure. Optional — 0 means not set.
	MonthlyIncome  float64 `json:"monthlyIncome"`
	IncomeCurrency string  `json:"incomeCurrency,omitempty"`

	// SalaryAccountID is the ledger account that receives auto-posted salary
	// (advance + remainder). nil = auto-salary off.
	SalaryAccountID *uint `json:"salaryAccountId,omitempty"`

	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// RateHistory records a currency's value (1 unit in USD) on a given day, so
// snapshots for days the machine was off can be valued at that day's rate
// rather than today's. One row per user/currency/day.
type RateHistory struct {
	ID       uint      `gorm:"primaryKey" json:"-"`
	UserID   uint      `gorm:"uniqueIndex:idx_ratehist;not null" json:"-"`
	Currency string    `gorm:"uniqueIndex:idx_ratehist;not null" json:"currency"`
	Date     time.Time `gorm:"uniqueIndex:idx_ratehist;not null" json:"date"`
	PerUSD   float64   `json:"perUSD"`
}

// AssetKind enumerates the categories a user sees. Debt is modelled as an asset
// with a negative contribution so everything lives in one table and one CRUD.
type AssetKind string

const (
	KindRealEstate AssetKind = "realestate"
	KindCar        AssetKind = "car"
	KindDeposit    AssetKind = "deposit"
	KindCash       AssetKind = "cash"
	KindInvestment AssetKind = "investment"
	KindMetals     AssetKind = "metals"
	KindDebt       AssetKind = "debt"
	KindLent       AssetKind = "lent" // money you lent out — a receivable (asset)
)

func (k AssetKind) Valid() bool {
	switch k {
	case KindRealEstate, KindCar, KindDeposit, KindCash, KindInvestment, KindMetals, KindDebt, KindLent:
		return true
	}
	return false
}

// Asset is a single item of net worth. Which fields are meaningful depends on
// Kind (see internal/calc). A wide table keeps the first version simple; it can
// be normalised later without changing the API.
type Asset struct {
	ID       uint      `gorm:"primaryKey" json:"id"`
	UserID   uint      `gorm:"index;not null" json:"userId"`
	Kind     AssetKind `gorm:"index;not null" json:"kind"`
	Name     string    `json:"name"`
	Currency string    `gorm:"not null" json:"currency"`

	Value          float64 `json:"value"`          // worth now / debt balance as of BalanceAsOf
	Invested       float64 `json:"invested"`       // cost basis (realestate, car, investment)
	MonthlyIncome  float64 `json:"monthlyIncome"`  // rent / dividends / coupons
	MonthlyPayment float64 `json:"monthlyPayment"` // debt: actual monthly payment
	RatePercent    float64 `json:"ratePercent"`    // annual %, for deposit and debt

	// BalanceAsOf anchors a debt's balance in time. Interest accrues daily from
	// this date, so the outstanding amount grows until the user re-enters the
	// balance (which re-anchors it to now).
	BalanceAsOf time.Time `json:"balanceAsOf,omitempty"`

	// ExcludeFromNetWorth: the item still shows in its category and in the
	// Assets/Liabilities totals, but is left out of the Net Worth figure. Handy
	// for consumer debts (e.g. a phone instalment) whose paying-off shouldn't
	// swing capital when the paying account isn't tracked.
	ExcludeFromNetWorth bool `gorm:"default:false" json:"excludeFromNetWorth"`

	// IsAccount marks a cash asset as a ledger account: its balance (Value) is
	// the sum of its AccountEntries (salary in, payments out), not typed directly.
	IsAccount bool `gorm:"default:false" json:"isAccount"`

	PurchaseDate     *time.Time `json:"purchaseDate,omitempty"`
	MaintenanceHours float64    `json:"maintenanceHours"` // hours per year to service

	Subtype string `json:"subtype,omitempty"` // investment: stock/bond/fund
	Status  string `json:"status,omitempty"`  // realestate: own/rented

	DebtScheme       string     `json:"debtScheme,omitempty"` // accruing/annuity/differentiated/interestfree
	LoanType         string     `json:"loanType,omitempty"`   // legacy; superseded by DebtScheme
	TermMonths       int        `json:"termMonths,omitempty"`
	FirstPaymentDate *time.Time `json:"firstPaymentDate,omitempty"`
	// PayoffDate is the loan's fixed end date. When set, a payment recomputes the
	// monthly payment to amortize the remaining balance to this date — the bank's
	// "keep the term, lower the payment" behaviour on early repayment.
	PayoffDate    *time.Time `json:"payoffDate,omitempty"`
	LinkedAssetID *uint      `json:"linkedAssetId,omitempty"`

	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Rate is a user-maintained exchange rate: the value of 1 unit of Currency in
// USD. Rates are entered manually — no third-party API dependency. USD is
// always 1 and is never stored.
type Rate struct {
	ID        uint      `gorm:"primaryKey" json:"-"`
	UserID    uint      `gorm:"uniqueIndex:idx_user_currency;not null" json:"-"`
	Currency  string    `gorm:"uniqueIndex:idx_user_currency;not null" json:"currency"`
	PerUSD    float64   `gorm:"not null" json:"perUSD"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Snapshot records net worth on a given day, stored in EACH supported currency
// at that day's rate. Storing per-currency (not just USD, converted later)
// keeps history honest: a portfolio of сомони assets shows a flat line in
// сомони and the FX wobble only in USD — instead of the USD wobble leaking into
// every currency. One row per user per day (upserted when the overview runs).
type Snapshot struct {
	ID     uint      `gorm:"primaryKey" json:"-"`
	UserID uint      `gorm:"uniqueIndex:idx_user_date;not null" json:"-"`
	Date   time.Time `gorm:"uniqueIndex:idx_user_date;not null" json:"date"`

	NetWorthUSD    float64 `json:"-"`
	AssetsUSD      float64 `json:"-"`
	LiabilitiesUSD float64 `json:"-"`

	NetWorthTJS    float64 `json:"-"`
	AssetsTJS      float64 `json:"-"`
	LiabilitiesTJS float64 `json:"-"`

	NetWorthUZS    float64 `json:"-"`
	AssetsUZS      float64 `json:"-"`
	LiabilitiesUZS float64 `json:"-"`

	// CompositionUSD is a JSON map of kind → asset value in USD on that day
	// (non-flagged assets + vested options), for the "composition over time"
	// chart. Empty on legacy rows.
	CompositionUSD string `json:"-"`
}

// AssetValue records the value of a single asset on a given day, so its own
// growth (e.g. a flat that appreciates) can be charted. One row per asset per
// day, upserted whenever the value is entered.
type AssetValue struct {
	ID      uint      `gorm:"primaryKey" json:"-"`
	AssetID uint      `gorm:"uniqueIndex:idx_asset_date;not null" json:"-"`
	UserID  uint      `gorm:"index;not null" json:"-"`
	Date    time.Time `gorm:"uniqueIndex:idx_asset_date;not null" json:"date"`
	Value   float64   `json:"value"`
}

// ExpenseCategories / IncomeCategories are the fixed lists shared by the app
// and the Telegram bot. Order matters — the bot references categories by index.
var ExpenseCategories = []string{
	"Продукты", "Кафе/Кофе", "Транспорт", "Дом/Коммуналка", "Здоровье",
	"Одежда", "Развлечения", "Дети", "Связь", "Прочее",
}
var IncomeCategories = []string{"Зарплата", "Подарок", "Прочее"}

// Transaction is a household income/expense entry (the Monefy-style module).
// It's a cash-flow record, separate from the net-worth (Asset) side.
type Transaction struct {
	ID             uint      `gorm:"primaryKey" json:"id"`
	UserID         uint      `gorm:"index;not null" json:"-"`
	Date           time.Time `gorm:"index" json:"date"`
	Type           string    `gorm:"not null" json:"type"` // "expense" | "income"
	Category       string    `json:"category"`
	Amount         float64   `json:"amount"`
	Currency       string    `gorm:"not null" json:"currency"`
	Person         string    `json:"person"` // who logged it (Telegram first name)
	Note           string    `json:"note,omitempty"`
	Source         string    `json:"source"`         // "bot" | "manual"
	TelegramChatID int64     `gorm:"index" json:"-"` // for dedup
	TelegramMsgID  int64     `json:"-"`              // for dedup
	CreatedAt      time.Time `json:"createdAt"`
}

// Activity is one entry in the user's action log: what changed, and how net
// worth moved as a result. NetBeforeUSD/NetAfterUSD are stored in USD (currency
// -neutral) so the delta stays valid whatever currency the user later views in.
// Rate changes are logged too, so "capital went up because TJS moved 9.24→9.30"
// is visible alongside "added a debt of N".
type Activity struct {
	ID           uint      `gorm:"primaryKey" json:"id"`
	UserID       uint      `gorm:"index;not null" json:"-"`
	CreatedAt    time.Time `gorm:"index" json:"createdAt"`
	Kind         string    `gorm:"not null" json:"kind"` // asset_added | asset_edited | asset_removed | option_* | rate_changed
	Title        string    `json:"title"`                // "Добавлен долг «Ремонт»"
	Detail       string    `json:"detail"`               // "10 000 смн" | "TJS 9.24 → 9.30"
	Amount       float64   `json:"amount"`               // item amount in its own currency (0 if n/a)
	Currency     string    `json:"currency"`
	NetBeforeUSD float64   `json:"netBeforeUsd"`
	NetAfterUSD  float64   `json:"netAfterUsd"`
}

// AccountEntry is one movement on a ledger account: income in (+) or payment /
// withdrawal out (−). The account's balance is the sum of its entries, so any
// entry can be added, edited or deleted and the balance stays consistent.
type AccountEntry struct {
	ID           uint      `gorm:"primaryKey" json:"id"`
	UserID       uint      `gorm:"index;not null" json:"-"`
	AccountID    uint      `gorm:"index;not null" json:"accountId"`
	Date         time.Time `gorm:"index" json:"date"`
	Kind         string    `gorm:"not null" json:"kind"` // "income" | "payment"
	Amount       float64   `json:"amount"`               // always positive; Kind gives the sign
	Note         string    `json:"note"`
	Source       string    `json:"source"`                 // opening | salary_auto | manual | debt_payment
	LinkedDebtID *uint     `json:"linkedDebtId,omitempty"` // set for debt payments
	DebtAmount   float64   `json:"debtAmount,omitempty"`   // debt payment: amount in the debt's own currency
	PayKey       string    `gorm:"index" json:"-"`         // idempotency key for auto-salary (e.g. "2026-08-adv")
	CreatedAt    time.Time `json:"createdAt"`
}

// OptionGrant is a batch of employee stock options received on GrantDate that
// crystallizes (fully vests, becoming real shares) VestMonths later. Value =
// Quantity × UnitPrice. Only crystallized grants count toward net worth; grants
// still vesting — or not yet received — are shown separately until they vest.
type OptionGrant struct {
	ID         uint      `gorm:"primaryKey" json:"id"`
	UserID     uint      `gorm:"index;not null" json:"-"`
	Name       string    `json:"name"`
	Quantity   float64   `json:"quantity"`
	UnitPrice  float64   `json:"unitPrice"`
	Currency   string    `gorm:"not null" json:"currency"`
	GrantDate  time.Time `json:"grantDate"`
	VestMonths int       `json:"vestMonths"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

// Goal is a target capital amount. Progress is computed against current net
// worth; ETA uses the optional monthly contribution.
type Goal struct {
	ID                  uint      `gorm:"primaryKey" json:"id"`
	UserID              uint      `gorm:"index;not null" json:"-"`
	Title               string    `json:"title"`
	TargetAmount        float64   `json:"targetAmount"`
	Currency            string    `gorm:"not null" json:"currency"`
	MonthlyContribution float64   `json:"monthlyContribution"`
	CreatedAt           time.Time `json:"createdAt"`
	UpdatedAt           time.Time `json:"updatedAt"`
}
