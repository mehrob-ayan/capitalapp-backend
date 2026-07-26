package api

import (
	"net/http"
	"time"

	"capitalapp/internal/model"

	"github.com/labstack/echo/v4"
	"gorm.io/gorm"
)

// recomputeAccount sets a ledger account's balance (its asset Value) to the sum
// of its entries: income adds, payment subtracts. Called after any entry change
// so add/edit/delete always leaves the balance consistent.
func (s *Server) recomputeAccount(uid, accountID uint) {
	var entries []model.AccountEntry
	s.db.Where("user_id = ? AND account_id = ?", uid, accountID).Find(&entries)
	var bal float64
	for _, e := range entries {
		if e.Kind == "payment" {
			bal -= e.Amount
		} else {
			bal += e.Amount
		}
	}
	s.db.Model(&model.Asset{}).Where("id = ? AND user_id = ?", accountID, uid).Update("value", round2(bal))
}

type accountResp struct {
	ID       uint    `json:"id"`
	Name     string  `json:"name"`
	Currency string  `json:"currency"`
	Balance  float64 `json:"balance"`
	IsSalary bool    `json:"isSalary"`
}

func (s *Server) accountRow(a model.Asset, salaryID *uint) accountResp {
	r := accountResp{ID: a.ID, Name: a.Name, Currency: a.Currency, Balance: round2(a.Value)}
	if salaryID != nil && *salaryID == a.ID {
		r.IsSalary = true
	}
	return r
}

// listAccounts returns the user's ledger accounts (cash assets marked IsAccount).
func (s *Server) listAccounts(c echo.Context) error {
	uid := c.Get(ctxUserID).(uint)
	user, _ := s.loadUser(uid)
	var accs []model.Asset
	if err := s.db.Where("user_id = ? AND is_account = ?", uid, true).Order("created_at").Find(&accs).Error; err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "db error")
	}
	out := make([]accountResp, 0, len(accs))
	for _, a := range accs {
		out = append(out, s.accountRow(a, user.SalaryAccountID))
	}
	return c.JSON(http.StatusOK, out)
}

type accountInput struct {
	Name            string  `json:"name"`
	Currency        string  `json:"currency"`
	StartingBalance float64 `json:"startingBalance"`
	IsSalary        bool    `json:"isSalary"`
}

func (s *Server) createAccount(c echo.Context) error {
	uid := c.Get(ctxUserID).(uint)
	var in accountInput
	if err := c.Bind(&in); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid body")
	}
	if !validCurrency[in.Currency] {
		return echo.NewHTTPError(http.StatusBadRequest, "unsupported currency")
	}
	a := model.Asset{UserID: uid, Kind: model.KindCash, Name: in.Name, Currency: in.Currency, IsAccount: true, BalanceAsOf: time.Now()}
	if a.Name == "" {
		a.Name = "Счёт"
	}
	if err := s.db.Create(&a).Error; err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "db error")
	}
	if in.StartingBalance != 0 {
		s.db.Create(&model.AccountEntry{UserID: uid, AccountID: a.ID, Date: time.Now(), Kind: "income", Amount: in.StartingBalance, Note: "Начальный остаток", Source: "opening"})
	}
	s.recomputeAccount(uid, a.ID)
	if in.IsSalary {
		s.db.Model(&model.User{}).Where("id = ?", uid).Update("salary_account_id", a.ID)
	}
	s.db.First(&a, a.ID)
	user, _ := s.loadUser(uid)
	return c.JSON(http.StatusCreated, s.accountRow(a, user.SalaryAccountID))
}

type entriesResp struct {
	Account accountResp          `json:"account"`
	Entries []model.AccountEntry `json:"entries"`
}

func (s *Server) accountEntries(c echo.Context) error {
	uid := c.Get(ctxUserID).(uint)
	id, err := parseID(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad id")
	}
	var a model.Asset
	if err := s.db.Where("id = ? AND user_id = ? AND is_account = ?", id, uid, true).First(&a).Error; err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "account not found")
	}
	var entries []model.AccountEntry
	s.db.Where("user_id = ? AND account_id = ?", uid, id).Order("date desc, id desc").Find(&entries)
	user, _ := s.loadUser(uid)
	return c.JSON(http.StatusOK, entriesResp{Account: s.accountRow(a, user.SalaryAccountID), Entries: entries})
}

type entryInput struct {
	Kind   string  `json:"kind"` // income | payment
	Amount float64 `json:"amount"`
	Note   string  `json:"note"`
	Date   *string `json:"date"`
}

func (s *Server) addEntry(c echo.Context) error {
	uid := c.Get(ctxUserID).(uint)
	id, err := parseID(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad id")
	}
	var a model.Asset
	if err := s.db.Where("id = ? AND user_id = ? AND is_account = ?", id, uid, true).First(&a).Error; err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "account not found")
	}
	e, err := bindEntry(c, uid, id)
	if err != nil {
		return err
	}
	e.Source = "manual"
	if err := s.db.Create(&e).Error; err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "db error")
	}
	s.recomputeAccount(uid, id)
	return c.NoContent(http.StatusCreated)
}

func (s *Server) updateEntry(c echo.Context) error {
	uid := c.Get(ctxUserID).(uint)
	var e model.AccountEntry
	if err := s.db.Where("id = ? AND user_id = ?", c.Param("id"), uid).First(&e).Error; err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "entry not found")
	}
	ne, err := bindEntry(c, uid, e.AccountID)
	if err != nil {
		return err
	}
	e.Kind, e.Amount, e.Note, e.Date = ne.Kind, ne.Amount, ne.Note, ne.Date
	if err := s.db.Save(&e).Error; err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "db error")
	}
	s.recomputeAccount(uid, e.AccountID)
	return c.NoContent(http.StatusOK)
}

func (s *Server) deleteEntry(c echo.Context) error {
	uid := c.Get(ctxUserID).(uint)
	var e model.AccountEntry
	if err := s.db.Where("id = ? AND user_id = ?", c.Param("id"), uid).First(&e).Error; err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "entry not found")
	}
	// Deleting a debt payment gives the money back to the debt too.
	if e.Source == "debt_payment" && e.LinkedDebtID != nil {
		s.db.Model(&model.Asset{}).Where("id = ? AND user_id = ?", *e.LinkedDebtID, uid).
			Update("value", gorm.Expr("value + ?", e.Amount))
	}
	if err := s.db.Delete(&model.AccountEntry{}, e.ID).Error; err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "db error")
	}
	s.recomputeAccount(uid, e.AccountID)
	return c.NoContent(http.StatusNoContent)
}

func bindEntry(c echo.Context, uid, accountID uint) (model.AccountEntry, error) {
	var in entryInput
	if err := c.Bind(&in); err != nil {
		return model.AccountEntry{}, echo.NewHTTPError(http.StatusBadRequest, "invalid body")
	}
	if in.Kind != "income" && in.Kind != "payment" {
		return model.AccountEntry{}, echo.NewHTTPError(http.StatusBadRequest, "kind must be income or payment")
	}
	if in.Amount <= 0 {
		return model.AccountEntry{}, echo.NewHTTPError(http.StatusBadRequest, "amount must be positive")
	}
	date := time.Now()
	if in.Date != nil && *in.Date != "" {
		if d, err := time.Parse("2006-01-02", *in.Date); err == nil {
			date = d
		}
	}
	return model.AccountEntry{UserID: uid, AccountID: accountID, Date: date, Kind: in.Kind, Amount: in.Amount, Note: in.Note}, nil
}

// debtPayments returns the payment history for a debt (entries linked to it).
func (s *Server) debtPayments(c echo.Context) error {
	uid := c.Get(ctxUserID).(uint)
	id, err := parseID(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad id")
	}
	var entries []model.AccountEntry
	s.db.Where("user_id = ? AND linked_debt_id = ?", uid, id).Order("date desc, id desc").Find(&entries)
	return c.JSON(http.StatusOK, entries)
}

type payInput struct {
	Amount    float64 `json:"amount"`
	AccountID uint    `json:"accountId"`
	Date      *string `json:"date"`
}

// payDebt records a debt payment: money leaves the account (a payment entry) and
// the debt's balance goes down by the same amount. Double-entry — net worth only
// drops by interest, principal paydown is neutral.
func (s *Server) payDebt(c echo.Context) error {
	uid := c.Get(ctxUserID).(uint)
	debt, err := s.findAsset(uid, c.Param("id"))
	if err != nil || debt.Kind != model.KindDebt {
		return echo.NewHTTPError(http.StatusNotFound, "debt not found")
	}
	var in payInput
	if err := c.Bind(&in); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid body")
	}
	if in.Amount <= 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "amount must be positive")
	}
	var acc model.Asset
	if err := s.db.Where("id = ? AND user_id = ? AND is_account = ?", in.AccountID, uid, true).First(&acc).Error; err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "account not found")
	}
	date := time.Now()
	if in.Date != nil && *in.Date != "" {
		if d, err := time.Parse("2006-01-02", *in.Date); err == nil {
			date = d
		}
	}
	before, _ := s.netWorthUSD(uid)

	// Reduce the debt balance and re-anchor accrual to now.
	newBal := debt.Value - in.Amount
	if newBal < 0 {
		newBal = 0
	}
	debt.Value = round2(newBal)
	debt.BalanceAsOf = time.Now()
	s.db.Save(&debt)

	// Money leaves the account.
	did := debt.ID
	s.db.Create(&model.AccountEntry{
		UserID: uid, AccountID: acc.ID, Date: date, Kind: "payment", Amount: in.Amount,
		Note: "Платёж: " + debt.Name, Source: "debt_payment", LinkedDebtID: &did,
	})
	s.recomputeAccount(uid, acc.ID)

	s.logActivity(uid, "debt_payment", "Платёж по долгу «"+debt.Name+"»",
		fmtAmount(in.Amount, debt.Currency)+" · с «"+acc.Name+"»", in.Amount, debt.Currency, before)

	return c.NoContent(http.StatusOK)
}
