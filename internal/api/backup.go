package api

import (
	"net/http"
	"time"

	"capitalapp/internal/model"

	"github.com/labstack/echo/v4"
	"gorm.io/gorm"
)

// exportPayload is the full, portable snapshot of a user's data. Snapshots and
// per-asset history are derived, so they're left out — they rebuild themselves.
type exportPayload struct {
	Version      int                `json:"version"`
	ExportedAt   time.Time          `json:"exportedAt"`
	BaseCurrency string             `json:"baseCurrency"`
	Rates        map[string]float64 `json:"rates"`
	Assets       []model.Asset      `json:"assets"`
	Goals        []model.Goal       `json:"goals"`
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
	var assets []model.Asset
	var goals []model.Goal
	s.db.Where("user_id = ?", uid).Order("id").Find(&assets)
	s.db.Where("user_id = ?", uid).Order("id").Find(&goals)

	return c.JSON(http.StatusOK, exportPayload{
		Version:      1,
		ExportedAt:   time.Now(),
		BaseCurrency: baseOr(user.BaseCurrency),
		Rates:        rates,
		Assets:       assets,
		Goals:        goals,
	})
}

// importData replaces ALL of the user's data with the contents of the file.
// Runs in a transaction so a bad file can't leave things half-imported.
func (s *Server) importData(c echo.Context) error {
	uid := c.Get(ctxUserID).(uint)
	var in exportPayload
	if err := c.Bind(&in); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid file")
	}

	err := s.db.Transaction(func(tx *gorm.DB) error {
		for _, m := range []any{&model.Asset{}, &model.Goal{}, &model.Rate{}, &model.Snapshot{}, &model.AssetValue{}} {
			if err := tx.Where("user_id = ?", uid).Delete(m).Error; err != nil {
				return err
			}
		}

		for i := range in.Assets {
			a := in.Assets[i]
			a.ID = 0              // let the DB assign fresh IDs
			a.UserID = uid        // bind to the importing user
			a.LinkedAssetID = nil // links reference old IDs; drop them
			if a.BalanceAsOf.IsZero() {
				a.BalanceAsOf = time.Now()
			}
			if err := tx.Create(&a).Error; err != nil {
				return err
			}
		}

		for i := range in.Goals {
			g := in.Goals[i]
			g.ID = 0
			g.UserID = uid
			if err := tx.Create(&g).Error; err != nil {
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

		if validCurrency[in.BaseCurrency] {
			if err := tx.Model(&model.User{}).Where("id = ?", uid).
				Update("base_currency", in.BaseCurrency).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "import failed")
	}

	// Seed a value-history point for tracked assets so their charts start fresh.
	var assets []model.Asset
	s.db.Where("user_id = ?", uid).Find(&assets)
	for _, a := range assets {
		if tracksValue[a.Kind] {
			s.recordAssetValue(a.ID, uid, a.Value)
		}
	}

	return c.JSON(http.StatusOK, echo.Map{"status": "ok", "assets": len(in.Assets), "goals": len(in.Goals)})
}
