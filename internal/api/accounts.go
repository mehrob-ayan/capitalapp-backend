package api

import (
	"net/http"
	"time"

	"capitalapp/internal/calc"
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
	s.postSalary(uid) // catch up any due salary before showing balances
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

// deleteAccount removes a ledger account. It first undoes any debt payments made
// from it (restoring the debts), then deletes the account's entries and the
// account itself — so removing an account leaves no trace.
func (s *Server) deleteAccount(c echo.Context) error {
	uid := c.Get(ctxUserID).(uint)
	id, err := parseID(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad id")
	}
	var acc model.Asset
	if err := s.db.Where("id = ? AND user_id = ? AND is_account = ?", id, uid, true).First(&acc).Error; err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "account not found")
	}
	var pays []model.AccountEntry
	s.db.Where("user_id = ? AND account_id = ? AND source = ?", uid, id, "debt_payment").Find(&pays)
	for _, e := range pays {
		if e.LinkedDebtID == nil {
			continue
		}
		restore := e.DebtAmount
		if restore <= 0 {
			restore = e.Amount
		}
		s.db.Model(&model.Asset{}).Where("id = ? AND user_id = ?", *e.LinkedDebtID, uid).
			Update("value", gorm.Expr("value + ?", restore))
	}
	s.db.Where("user_id = ? AND account_id = ?", uid, id).Delete(&model.AccountEntry{})
	s.db.Where("id = ? AND user_id = ?", id, uid).Delete(&model.Asset{})
	// If it was the salary account, turn off auto-salary.
	s.db.Model(&model.User{}).Where("id = ? AND salary_account_id = ?", uid, id).
		Updates(map[string]any{"salary_account_id": nil})
	return c.NoContent(http.StatusNoContent)
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
	// Order by when each movement was actually recorded, so the ledger reads in
	// the true sequence operations were made (date alone can't tell same-day order).
	s.db.Where("user_id = ? AND account_id = ?", uid, id).Order("created_at desc, id desc").Find(&entries)
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
	// Deleting a debt payment (or a lent repayment) restores the linked asset's
	// balance — the exact debt-currency amount, converting for older entries.
	if (e.Source == "debt_payment" || e.Source == "lent_repayment") && e.LinkedDebtID != nil {
		// Prefer the stored debt-currency amount (exact); fall back to converting
		// the account amount for older entries that predate DebtAmount.
		restore := e.DebtAmount
		if restore <= 0 {
			var debt, acc model.Asset
			if s.db.Where("id = ? AND user_id = ?", *e.LinkedDebtID, uid).First(&debt).Error == nil {
				restore = e.Amount
				if s.db.Where("id = ? AND user_id = ?", e.AccountID, uid).First(&acc).Error == nil && acc.Currency != debt.Currency {
					rates, _ := s.userRates(uid)
					restore = round2(calc.Convert(e.Amount, acc.Currency, debt.Currency, rates))
				}
			}
		}
		s.db.Model(&model.Asset{}).Where("id = ? AND user_id = ?", *e.LinkedDebtID, uid).Update("value", gorm.Expr("value + ?", restore))
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

	// The entered amount is in the debt's currency. When the paying account is in
	// a different currency, the account is debited the converted amount so both
	// sides stay honest (e.g. paying a сомони mortgage from a сумы account).
	rates, _ := s.userRates(uid)
	debit := round2(calc.Convert(in.Amount, debt.Currency, acc.Currency, rates))

	// A manual payment breaks a fixed instalment schedule. Switch annuity /
	// differentiated debts to balance-driven pay-down so the payment stays the
	// same and the term shortens (bank-style curtailment) — the way every other
	// debt here behaves. The current scheduled payment becomes the fixed payment.
	scheme := calc.DebtScheme(debt.DebtScheme)
	if scheme == calc.SchemeAnnuity || scheme == calc.SchemeDifferentiated {
		if ls := calc.Compute(debt, debt.Currency, rates, time.Now()).Loan; ls != nil && ls.MonthlyPayment > 0 && debt.MonthlyPayment <= 0 {
			debt.MonthlyPayment = round2(ls.MonthlyPayment)
		}
		if debt.RatePercent > 0 {
			debt.DebtScheme = string(calc.SchemeAccruing)
		} else {
			debt.DebtScheme = string(calc.SchemeInterestFree)
		}
	}

	// Reduce the debt balance (in its own currency) and re-anchor accrual to now.
	newBal := debt.Value - in.Amount
	if newBal < 0 {
		newBal = 0
	}
	debt.Value = round2(newBal)
	debt.BalanceAsOf = time.Now()
	// Bank-style: keep the payoff date, recompute the monthly payment for the new
	// balance over the months remaining until it.
	if debt.PayoffDate != nil && debt.RatePercent > 0 {
		now := time.Now()
		n := (debt.PayoffDate.Year()-now.Year())*12 + int(debt.PayoffDate.Month()) - int(now.Month())
		if n >= 1 {
			debt.MonthlyPayment = round2(calc.AnnuityPayment(debt.Value, debt.RatePercent, n))
		}
	}
	s.db.Save(&debt)

	// Money leaves the account (in the account's currency).
	did := debt.ID
	note := "Платёж: " + debt.Name
	if debt.Currency != acc.Currency {
		note += " (" + fmtAmount(in.Amount, debt.Currency) + ")"
	}
	s.db.Create(&model.AccountEntry{
		UserID: uid, AccountID: acc.ID, Date: date, Kind: "payment", Amount: debit,
		Note: note, Source: "debt_payment", LinkedDebtID: &did, DebtAmount: in.Amount,
	})
	s.recomputeAccount(uid, acc.ID)

	s.logActivity(uid, "debt_payment", "Платёж по долгу «"+debt.Name+"»",
		fmtAmount(in.Amount, debt.Currency)+" · с «"+acc.Name+"»", in.Amount, debt.Currency, before)

	return c.NoContent(http.StatusOK)
}

// repayLent records a repayment of money you lent out: money comes INTO the
// account (income) and the receivable ("Долг мне") goes down. Mirror of payDebt.
func (s *Server) repayLent(c echo.Context) error {
	uid := c.Get(ctxUserID).(uint)
	lent, err := s.findAsset(uid, c.Param("id"))
	if err != nil || lent.Kind != model.KindLent {
		return echo.NewHTTPError(http.StatusNotFound, "not found")
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
	rates, _ := s.userRates(uid)
	credit := round2(calc.Convert(in.Amount, lent.Currency, acc.Currency, rates))

	newBal := lent.Value - in.Amount
	if newBal < 0 {
		newBal = 0
	}
	lent.Value = round2(newBal)
	s.db.Save(&lent)

	lid := lent.ID
	note := "Возврат: " + lent.Name
	if lent.Currency != acc.Currency {
		note += " (" + fmtAmount(in.Amount, lent.Currency) + ")"
	}
	s.db.Create(&model.AccountEntry{
		UserID: uid, AccountID: acc.ID, Date: date, Kind: "income", Amount: credit,
		Note: note, Source: "lent_repayment", LinkedDebtID: &lid, DebtAmount: in.Amount,
	})
	s.recomputeAccount(uid, acc.ID)

	s.logActivity(uid, "lent_repayment", "Возврат по «"+lent.Name+"»",
		fmtAmount(in.Amount, lent.Currency)+" · на «"+acc.Name+"»", in.Amount, lent.Currency, before)

	return c.NoContent(http.StatusOK)
}
