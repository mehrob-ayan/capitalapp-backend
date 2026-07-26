package api

import (
	"net/http"
	"time"

	"capitalapp/internal/model"

	"github.com/labstack/echo/v4"
	"gorm.io/gorm"
)

// exportPayload is the full, portable snapshot of a user's data. Derived data
// (snapshots, per-asset value history, rate history, the activity log) is left
// out — it rebuilds itself from the state below.
type exportPayload struct {
	Version         int                  `json:"version"`
	ExportedAt      time.Time            `json:"exportedAt"`
	BaseCurrency    string               `json:"baseCurrency"`
	AutoRates       bool                 `json:"autoRates"`
	MonthlyIncome   float64              `json:"monthlyIncome"`
	IncomeCurrency  string               `json:"incomeCurrency"`
	SalaryAccountID *uint                `json:"salaryAccountId"`
	Rates           map[string]float64   `json:"rates"`
	Assets          []model.Asset        `json:"assets"`
	Goals           []model.Goal         `json:"goals"`
	Options         []model.OptionGrant  `json:"options"`
	Entries         []model.AccountEntry `json:"entries"`
	Transactions    []model.Transaction  `json:"transactions"`
}

func (s *Server) exportData(c echo.Context) error {
	uid := c.Get(ctxUserID).(uint)
	user, err := s.loadUser(uid)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "user not found")
	}
	rates, err := s.userRates(uid)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "db error")
	}
	var (
		assets  []model.Asset
		goals   []model.Goal
		options []model.OptionGrant
		entries []model.AccountEntry
		txs     []model.Transaction
	)
	s.db.Where("user_id = ?", uid).Order("id").Find(&assets)
	s.db.Where("user_id = ?", uid).Order("id").Find(&goals)
	s.db.Where("user_id = ?", uid).Order("id").Find(&options)
	s.db.Where("user_id = ?", uid).Order("id").Find(&entries)
	s.db.Where("user_id = ?", uid).Order("id").Find(&txs)

	return c.JSON(http.StatusOK, exportPayload{
		Version:         2,
		ExportedAt:      time.Now(),
		BaseCurrency:    baseOr(user.BaseCurrency),
		AutoRates:       user.AutoRates,
		MonthlyIncome:   user.MonthlyIncome,
		IncomeCurrency:  user.IncomeCurrency,
		SalaryAccountID: user.SalaryAccountID,
		Rates:           rates,
		Assets:          assets,
		Goals:           goals,
		Options:         options,
		Entries:         entries,
		Transactions:    txs,
	})
}

// importData replaces ALL of the user's data with the contents of the file.
// Runs in a transaction so a bad file can't leave things half-imported. Asset
// IDs are reassigned on import, so references to them (account entries, the
// salary account) are remapped through idMap.
func (s *Server) importData(c echo.Context) error {
	uid := c.Get(ctxUserID).(uint)
	var in exportPayload
	if err := c.Bind(&in); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid file")
	}

	idMap := map[uint]uint{} // old asset ID → new asset ID

	err := s.db.Transaction(func(tx *gorm.DB) error {
		for _, m := range []any{
			&model.Asset{}, &model.Goal{}, &model.Rate{}, &model.Snapshot{}, &model.AssetValue{},
			&model.OptionGrant{}, &model.AccountEntry{}, &model.Transaction{}, &model.RateHistory{},
		} {
			if err := tx.Where("user_id = ?", uid).Delete(m).Error; err != nil {
				return err
			}
		}

		for i := range in.Assets {
			a := in.Assets[i]
			oldID := a.ID
			a.ID = 0
			a.UserID = uid
			a.LinkedAssetID = nil
			if a.BalanceAsOf.IsZero() {
				a.BalanceAsOf = time.Now()
			}
			if err := tx.Create(&a).Error; err != nil {
				return err
			}
			idMap[oldID] = a.ID
		}

		for i := range in.Goals {
			g := in.Goals[i]
			g.ID = 0
			g.UserID = uid
			if err := tx.Create(&g).Error; err != nil {
				return err
			}
		}

		for i := range in.Options {
			o := in.Options[i]
			o.ID = 0
			o.UserID = uid
			if err := tx.Create(&o).Error; err != nil {
				return err
			}
		}

		for i := range in.Entries {
			e := in.Entries[i]
			acc, ok := idMap[e.AccountID]
			if !ok {
				continue // account didn't import — drop the orphan entry
			}
			e.ID = 0
			e.UserID = uid
			e.AccountID = acc
			if e.LinkedDebtID != nil {
				if nd, ok := idMap[*e.LinkedDebtID]; ok {
					e.LinkedDebtID = &nd
				} else {
					e.LinkedDebtID = nil
				}
			}
			if err := tx.Create(&e).Error; err != nil {
				return err
			}
		}

		for i := range in.Transactions {
			t := in.Transactions[i]
			t.ID = 0
			t.UserID = uid
			if err := tx.Create(&t).Error; err != nil {
				return err
			}
		}

		for cur, v := range in.Rates {
			if cur == "USD" || !validCurrency[cur] || v <= 0 {
				continue
			}
			if err := tx.Where(model.Rate{UserID: uid, Currency: cur}).
				Assign(model.Rate{PerUSD: v}).
				FirstOrCreate(&model.Rate{}).Error; err != nil {
				return err
			}
		}

		upd := map[string]any{
			"auto_rates":     in.AutoRates,
			"monthly_income": in.MonthlyIncome,
		}
		if validCurrency[in.BaseCurrency] {
			upd["base_currency"] = in.BaseCurrency
		}
		if in.IncomeCurrency == "" || validCurrency[in.IncomeCurrency] {
			upd["income_currency"] = in.IncomeCurrency
		}
		if in.SalaryAccountID != nil {
			if nid, ok := idMap[*in.SalaryAccountID]; ok {
				upd["salary_account_id"] = nid
			} else {
				upd["salary_account_id"] = nil
			}
		} else {
			upd["salary_account_id"] = nil
		}
		return tx.Model(&model.User{}).Where("id = ?", uid).Updates(upd).Error
	})
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "import failed")
	}

	// Recompute account balances from their entries, and seed value-history for
	// tracked assets so their charts start fresh.
	var assets []model.Asset
	s.db.Where("user_id = ?", uid).Find(&assets)
	for _, a := range assets {
		if a.IsAccount {
			s.recomputeAccount(uid, a.ID)
		}
		if tracksValue[a.Kind] {
			s.recordAssetValue(a.ID, uid, a.Value)
		}
	}

	return c.JSON(http.StatusOK, echo.Map{
		"status": "ok", "assets": len(in.Assets), "goals": len(in.Goals),
		"options": len(in.Options), "entries": len(in.Entries), "transactions": len(in.Transactions),
	})
}
