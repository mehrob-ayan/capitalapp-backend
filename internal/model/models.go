package model

import "time"

// User is a Telegram-authenticated account. All financial data hangs off it.
type User struct {
	ID           uint      `gorm:"primaryKey" json:"id"`
	TelegramID   int64     `gorm:"uniqueIndex;not null" json:"telegramId"`
	Username     string    `json:"username,omitempty"`
	FirstName    string    `json:"firstName,omitempty"`
	LastName     string    `json:"lastName,omitempty"`
	LanguageCode string    `json:"languageCode,omitempty"`
	BaseCurrency string    `gorm:"default:USD" json:"baseCurrency"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
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
	KindDebt       AssetKind = "debt"
)

func (k AssetKind) Valid() bool {
	switch k {
	case KindRealEstate, KindCar, KindDeposit, KindCash, KindInvestment, KindDebt:
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

	PurchaseDate     *time.Time `json:"purchaseDate,omitempty"`
	MaintenanceHours float64    `json:"maintenanceHours"` // hours per year to service

	Subtype string `json:"subtype,omitempty"` // investment: stock/bond/fund
	Status  string `json:"status,omitempty"`  // realestate: own/rented

	DebtScheme       string     `json:"debtScheme,omitempty"` // accruing/annuity/differentiated/interestfree
	LoanType         string     `json:"loanType,omitempty"`   // legacy; superseded by DebtScheme
	TermMonths       int        `json:"termMonths,omitempty"`
	FirstPaymentDate *time.Time `json:"firstPaymentDate,omitempty"`
	LinkedAssetID    *uint      `json:"linkedAssetId,omitempty"`

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

// Snapshot records net worth on a given day, stored in USD so it stays valid
// regardless of the user's display currency or later rate edits. One row per
// user per day (upserted whenever the overview is computed).
type Snapshot struct {
	ID             uint      `gorm:"primaryKey" json:"-"`
	UserID         uint      `gorm:"uniqueIndex:idx_user_date;not null" json:"-"`
	Date           time.Time `gorm:"uniqueIndex:idx_user_date;not null" json:"date"`
	NetWorthUSD    float64   `json:"-"`
	AssetsUSD      float64   `json:"-"`
	LiabilitiesUSD float64   `json:"-"`
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
