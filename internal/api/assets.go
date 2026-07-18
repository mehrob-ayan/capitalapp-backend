package api

import (
	"math"
	"net/http"
	"strconv"
	"time"

	"capitalapp/internal/calc"
	"capitalapp/internal/model"

	"github.com/labstack/echo/v4"
)

var kindLabels = map[model.AssetKind]string{
	model.KindRealEstate: "Недвижимость",
	model.KindCar:        "Транспорт",
	model.KindDeposit:    "Вклады",
	model.KindCash:       "Наличные и счета",
	model.KindInvestment: "Инвестиции",
	model.KindDebt:       "Кредиты и долги",
}

// order used for categories; assetKindOrder excludes debt for composition.
var kindOrder = []model.AssetKind{
	model.KindRealEstate, model.KindCar, model.KindDeposit,
	model.KindCash, model.KindInvestment, model.KindDebt,
}
var assetKindOrder = []model.AssetKind{
	model.KindRealEstate, model.KindCar, model.KindDeposit,
	model.KindCash, model.KindInvestment,
}

var validCurrency = map[string]bool{"USD": true, "TJS": true, "UZS": true}

// ---- request/response DTOs ----

type assetInput struct {
	Kind             string  `json:"kind"`
	Name             string  `json:"name"`
	Currency         string  `json:"currency"`
	Value            float64 `json:"value"`
	Invested         float64 `json:"invested"`
	MonthlyIncome    float64 `json:"monthlyIncome"`
	MonthlyPayment   float64 `json:"monthlyPayment"`
	RatePercent      float64 `json:"ratePercent"`
	PurchaseDate     *string `json:"purchaseDate"`
	MaintenanceHours float64 `json:"maintenanceHours"`
	Subtype          string  `json:"subtype"`
	Status           string  `json:"status"`
	DebtScheme       string  `json:"debtScheme"`
	LoanType         string  `json:"loanType"`
	TermMonths       int     `json:"termMonths"`
	FirstPaymentDate *string `json:"firstPaymentDate"`
	LinkedAssetID    *uint   `json:"linkedAssetId"`
}

type assetResp struct {
	model.Asset
	Metrics calc.AssetMetrics `json:"metrics"`
}

type compSlice struct {
	Kind      string  `json:"kind"`
	Label     string  `json:"label"`
	ValueBase float64 `json:"valueBase"`
	Percent   float64 `json:"percent"`
}

type categorySummary struct {
	Kind         string  `json:"kind"`
	Label        string  `json:"label"`
	SubtotalBase float64 `json:"subtotalBase"`
	Count        int     `json:"count"`
	IsLiability  bool    `json:"isLiability"`
}

type overviewResp struct {
	BaseCurrency string            `json:"baseCurrency"`
	NetWorth     float64           `json:"netWorth"`
	Assets       float64           `json:"assets"`
	Liabilities  float64           `json:"liabilities"`
	MonthlyFlow  float64           `json:"monthlyFlow"`
	Composition  []compSlice       `json:"composition"`
	Categories   []categorySummary `json:"categories"`
}

// ---- handlers ----

func (s *Server) overview(c echo.Context) error {
	uid := c.Get(ctxUserID).(uint)
	user, err := s.loadUser(uid)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "user not found")
	}
	base := baseOr(user.BaseCurrency)

	var assets []model.Asset
	if err := s.db.Where("user_id = ?", uid).Find(&assets).Error; err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "db error")
	}

	rates, err := s.userRates(uid)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "db error")
	}
	now := time.Now()

	var totalAssets, totalLiab, totalFlow float64
	byKindValue := map[model.AssetKind]float64{}
	byKindCount := map[model.AssetKind]int{}

	for _, a := range assets {
		m := calc.Compute(a, base, rates, now)
		totalAssets += m.ValueBase
		totalLiab += m.LiabilityBase
		totalFlow += m.MonthlyFlowBase
		byKindCount[a.Kind]++
		if a.Kind == model.KindDebt {
			byKindValue[a.Kind] += m.LiabilityBase
		} else {
			byKindValue[a.Kind] += m.ValueBase
		}
	}

	comp := make([]compSlice, 0, len(assetKindOrder))
	for _, k := range assetKindOrder {
		v := byKindValue[k]
		if v <= 0 {
			continue
		}
		pct := 0.0
		if totalAssets > 0 {
			pct = v / totalAssets * 100
		}
		comp = append(comp, compSlice{Kind: string(k), Label: kindLabels[k], ValueBase: round2(v), Percent: round2(pct)})
	}

	cats := make([]categorySummary, 0, len(kindOrder))
	for _, k := range kindOrder {
		if byKindCount[k] == 0 {
			continue
		}
		cats = append(cats, categorySummary{
			Kind: string(k), Label: kindLabels[k],
			SubtotalBase: round2(byKindValue[k]), Count: byKindCount[k],
			IsLiability: k == model.KindDebt,
		})
	}

	// Opening the overview also catches up any snapshots missed while the
	// machine was asleep/off (idempotent).
	_ = s.backfillSnapshots(uid)

	return c.JSON(http.StatusOK, overviewResp{
		BaseCurrency: base,
		NetWorth:     round2(totalAssets - totalLiab),
		Assets:       round2(totalAssets),
		Liabilities:  round2(totalLiab),
		MonthlyFlow:  round2(totalFlow),
		Composition:  comp,
		Categories:   cats,
	})
}

func (s *Server) listAssets(c echo.Context) error {
	uid := c.Get(ctxUserID).(uint)
	base, err := s.userBase(uid)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "user not found")
	}
	var assets []model.Asset
	if err := s.db.Where("user_id = ?", uid).Order("created_at").Find(&assets).Error; err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "db error")
	}
	rates, err := s.userRates(uid)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "db error")
	}
	now := time.Now()
	out := make([]assetResp, 0, len(assets))
	for _, a := range assets {
		out = append(out, assetResp{Asset: a, Metrics: calc.Compute(a, base, rates, now)})
	}
	return c.JSON(http.StatusOK, out)
}

func (s *Server) createAsset(c echo.Context) error {
	uid := c.Get(ctxUserID).(uint)
	base, err := s.userBase(uid)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "user not found")
	}
	var in assetInput
	if err := c.Bind(&in); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid body")
	}
	a, err := in.toModel(uid)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	a.BalanceAsOf = time.Now() // anchor interest accrual to now
	if err := s.db.Create(&a).Error; err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "db error")
	}
	if tracksValue[a.Kind] {
		s.recordAssetValue(a.ID, uid, a.Value)
	}
	rates, err := s.userRates(uid)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "db error")
	}
	return c.JSON(http.StatusCreated, assetResp{Asset: a, Metrics: calc.Compute(a, base, rates, time.Now())})
}

func (s *Server) getAsset(c echo.Context) error {
	uid := c.Get(ctxUserID).(uint)
	base, err := s.userBase(uid)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "user not found")
	}
	a, err := s.findAsset(uid, c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "asset not found")
	}
	rates, err := s.userRates(uid)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "db error")
	}
	return c.JSON(http.StatusOK, assetResp{Asset: a, Metrics: calc.Compute(a, base, rates, time.Now())})
}

func (s *Server) updateAsset(c echo.Context) error {
	uid := c.Get(ctxUserID).(uint)
	base, err := s.userBase(uid)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "user not found")
	}
	existing, err := s.findAsset(uid, c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "asset not found")
	}
	var in assetInput
	if err := c.Bind(&in); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid body")
	}
	updated, err := in.toModel(uid)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	updated.ID = existing.ID
	updated.CreatedAt = existing.CreatedAt
	updated.BalanceAsOf = time.Now() // re-anchor: the entered balance is current as of now
	if err := s.db.Save(&updated).Error; err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "db error")
	}
	if tracksValue[updated.Kind] {
		s.recordAssetValue(updated.ID, uid, updated.Value)
	}
	rates, err := s.userRates(uid)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "db error")
	}
	return c.JSON(http.StatusOK, assetResp{Asset: updated, Metrics: calc.Compute(updated, base, rates, time.Now())})
}

func (s *Server) deleteAsset(c echo.Context) error {
	uid := c.Get(ctxUserID).(uint)
	a, err := s.findAsset(uid, c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "asset not found")
	}
	if err := s.db.Delete(&model.Asset{}, a.ID).Error; err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "db error")
	}
	s.db.Where("asset_id = ?", a.ID).Delete(&model.AssetValue{})
	return c.NoContent(http.StatusNoContent)
}

// ---- helpers ----

func (s *Server) loadUser(uid uint) (model.User, error) {
	var u model.User
	err := s.db.First(&u, uid).Error
	return u, err
}

func (s *Server) userBase(uid uint) (string, error) {
	u, err := s.loadUser(uid)
	if err != nil {
		return "", err
	}
	return baseOr(u.BaseCurrency), nil
}

func (s *Server) findAsset(uid uint, idParam string) (model.Asset, error) {
	var a model.Asset
	id, err := strconv.ParseUint(idParam, 10, 64)
	if err != nil {
		return a, err
	}
	err = s.db.Where("id = ? AND user_id = ?", uint(id), uid).First(&a).Error
	return a, err
}

func (in assetInput) toModel(uid uint) (model.Asset, error) {
	kind := model.AssetKind(in.Kind)
	if !kind.Valid() {
		return model.Asset{}, echo.NewHTTPError(http.StatusBadRequest, "unknown kind")
	}
	if !validCurrency[in.Currency] {
		return model.Asset{}, echo.NewHTTPError(http.StatusBadRequest, "unsupported currency")
	}
	purchase, err := parseDate(in.PurchaseDate)
	if err != nil {
		return model.Asset{}, echo.NewHTTPError(http.StatusBadRequest, "invalid purchaseDate")
	}
	firstPay, err := parseDate(in.FirstPaymentDate)
	if err != nil {
		return model.Asset{}, echo.NewHTTPError(http.StatusBadRequest, "invalid firstPaymentDate")
	}
	return model.Asset{
		UserID:           uid,
		Kind:             kind,
		Name:             in.Name,
		Currency:         in.Currency,
		Value:            in.Value,
		Invested:         in.Invested,
		MonthlyIncome:    in.MonthlyIncome,
		MonthlyPayment:   in.MonthlyPayment,
		RatePercent:      in.RatePercent,
		PurchaseDate:     purchase,
		MaintenanceHours: in.MaintenanceHours,
		Subtype:          in.Subtype,
		Status:           in.Status,
		DebtScheme:       in.DebtScheme,
		LoanType:         in.LoanType,
		TermMonths:       in.TermMonths,
		FirstPaymentDate: firstPay,
		LinkedAssetID:    in.LinkedAssetID,
	}, nil
}

// parseDate accepts "2006-01-02" or "2006-01"; nil/empty → nil.
func parseDate(s *string) (*time.Time, error) {
	if s == nil || *s == "" {
		return nil, nil
	}
	for _, layout := range []string{"2006-01-02", "2006-01"} {
		if t, err := time.Parse(layout, *s); err == nil {
			return &t, nil
		}
	}
	return nil, echo.NewHTTPError(http.StatusBadRequest, "bad date")
}

func baseOr(base string) string {
	if base == "" {
		return "USD"
	}
	return base
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

func parseID(s string) (uint, error) {
	id, err := strconv.ParseUint(s, 10, 64)
	return uint(id), err
}
