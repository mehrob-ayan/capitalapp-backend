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
	model.KindMetals:     "Металлы",
	model.KindInvestment: "Инвестиции",
	model.KindDebt:       "Кредиты и долги",
}

// order used for categories; assetKindOrder excludes debt for composition.
var kindOrder = []model.AssetKind{
	model.KindRealEstate, model.KindCar, model.KindDeposit,
	model.KindCash, model.KindMetals, model.KindInvestment, model.KindDebt,
}
var assetKindOrder = []model.AssetKind{
	model.KindRealEstate, model.KindCar, model.KindDeposit,
	model.KindCash, model.KindMetals, model.KindInvestment,
}

var validCurrency = map[string]bool{"USD": true, "TJS": true, "UZS": true}

// currencyList is the ordered set of supported currencies (snapshots store net
// worth in each). USD first — it's the fallback for legacy rows.
var currencyList = []string{"USD", "TJS", "UZS"}

// ---- request/response DTOs ----

type assetInput struct {
	Kind                string  `json:"kind"`
	Name                string  `json:"name"`
	Currency            string  `json:"currency"`
	Value               float64 `json:"value"`
	Invested            float64 `json:"invested"`
	MonthlyIncome       float64 `json:"monthlyIncome"`
	MonthlyPayment      float64 `json:"monthlyPayment"`
	RatePercent         float64 `json:"ratePercent"`
	PurchaseDate        *string `json:"purchaseDate"`
	MaintenanceHours    float64 `json:"maintenanceHours"`
	Subtype             string  `json:"subtype"`
	Status              string  `json:"status"`
	ExcludeFromNetWorth bool    `json:"excludeFromNetWorth"`
	DebtScheme          string  `json:"debtScheme"`
	LoanType            string  `json:"loanType"`
	TermMonths          int     `json:"termMonths"`
	FirstPaymentDate    *string `json:"firstPaymentDate"`
	PayoffDate          *string `json:"payoffDate"`
	LinkedAssetID       *uint   `json:"linkedAssetId"`
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
	BaseCurrency  string            `json:"baseCurrency"`
	NetWorth      float64           `json:"netWorth"`
	Assets        float64           `json:"assets"`
	Liabilities   float64           `json:"liabilities"`
	MonthlyFlow   float64           `json:"monthlyFlow"`
	Options       float64           `json:"options"`
	OptionsVested float64           `json:"optionsVested"`
	Composition   []compSlice       `json:"composition"`
	Categories    []categorySummary `json:"categories"`
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

	// Displayed totals (totalAssets/totalLiab) include everything. The net-worth
	// basis (nwAssets/nwLiab/totalFlow) and composition skip items flagged
	// ExcludeFromNetWorth — they stay visible but don't move capital.
	var totalAssets, totalLiab, totalFlow, nwAssets, nwLiab float64
	byKindValue := map[model.AssetKind]float64{}
	byKindCount := map[model.AssetKind]int{}
	compValue := map[model.AssetKind]float64{}

	for _, a := range assets {
		m := calc.Compute(a, base, rates, now)
		totalAssets += m.ValueBase
		totalLiab += m.LiabilityBase
		// Monthly flow is real cash you move each month, so it counts every
		// item — including debts flagged out of net worth (you still pay them).
		totalFlow += m.MonthlyFlowBase
		byKindCount[a.Kind]++
		if a.Kind == model.KindDebt {
			byKindValue[a.Kind] += m.LiabilityBase
		} else {
			byKindValue[a.Kind] += m.ValueBase
		}
		// All items count in net worth now (double-entry: debts are funded from a
		// tracked account, so no more ExcludeFromNetWorth exclusion).
		nwAssets += m.ValueBase
		nwLiab += m.LiabilityBase
		if a.Kind != model.KindDebt {
			compValue[a.Kind] += m.ValueBase
		}
	}

	// Crystallized options are real shares now — fold them into assets and net
	// worth. Vesting/pending grants live only in the Options module until vested.
	vestedOptRaw, vestedOptCount := s.vestedOptions(uid, base, rates, now)
	vestedOptBase := round2(vestedOptRaw)
	totalAssets += vestedOptBase
	nwAssets += vestedOptBase

	// Monthly income (salary) is real inflow — fold it into the monthly flow so
	// "поток" is доход − расходы/платежи, not just payments.
	if user.MonthlyIncome > 0 {
		ic := user.IncomeCurrency
		if ic == "" {
			ic = base
		}
		totalFlow += calc.Convert(user.MonthlyIncome, ic, base, rates)
	}

	comp := make([]compSlice, 0, len(assetKindOrder)+1)
	for _, k := range assetKindOrder {
		v := compValue[k]
		if v <= 0 {
			continue
		}
		pct := 0.0
		if nwAssets > 0 {
			pct = v / nwAssets * 100
		}
		comp = append(comp, compSlice{Kind: string(k), Label: kindLabels[k], ValueBase: round2(v), Percent: round2(pct)})
	}
	if vestedOptBase > 0 {
		pct := 0.0
		if nwAssets > 0 {
			pct = vestedOptBase / nwAssets * 100
		}
		comp = append(comp, compSlice{Kind: "options", Label: "Опционы", ValueBase: vestedOptBase, Percent: round2(pct)})
	}

	cats := make([]categorySummary, 0, len(kindOrder)+1)
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
	if vestedOptCount > 0 {
		cats = append(cats, categorySummary{Kind: "options", Label: "Опционы", SubtotalBase: vestedOptBase, Count: vestedOptCount, IsLiability: false})
	}

	// Opening the overview also catches up any snapshots missed while the
	// machine was asleep/off (idempotent).
	_ = s.backfillSnapshots(uid)

	return c.JSON(http.StatusOK, overviewResp{
		BaseCurrency:  base,
		NetWorth:      round2(nwAssets - nwLiab),
		Assets:        round2(totalAssets),
		Liabilities:   round2(totalLiab),
		MonthlyFlow:   round2(totalFlow),
		Options:       s.optionsTotalBase(uid, base, rates),
		OptionsVested: vestedOptBase,
		Composition:   comp,
		Categories:    cats,
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
	before, _ := s.netWorthUSD(uid)
	a.BalanceAsOf = time.Now() // anchor interest accrual to now
	// Cash is a ledger account: its balance comes from entries (add funds /
	// withdraw), with history — the typed amount becomes the opening entry.
	if a.Kind == model.KindCash {
		a.IsAccount = true
	}
	if err := s.db.Create(&a).Error; err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "db error")
	}
	if a.IsAccount {
		if a.Value != 0 {
			s.db.Create(&model.AccountEntry{UserID: uid, AccountID: a.ID, Date: time.Now(), Kind: "income", Amount: a.Value, Note: "Начальный остаток", Source: "opening"})
		}
		s.recomputeAccount(uid, a.ID)
	}
	if tracksValue[a.Kind] {
		s.recordAssetValue(a.ID, uid, a.Value)
	}
	addDetail := fmtAmount(a.Value, a.Currency)
	if a.ExcludeFromNetWorth {
		addDetail += " · вне капитала"
	}
	s.logActivity(uid, "asset_added", assetActivityTitle("Добавлено", a), addDetail, a.Value, a.Currency, before)
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
	before, _ := s.netWorthUSD(uid)
	if err := s.db.Save(&updated).Error; err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "db error")
	}
	if tracksValue[updated.Kind] {
		s.recordAssetValue(updated.ID, uid, updated.Value)
	}
	s.logActivity(uid, "asset_edited", assetActivityTitle("Изменено", updated), assetEditDetail(existing, updated), updated.Value, updated.Currency, before)
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
	before, _ := s.netWorthUSD(uid)
	if err := s.db.Delete(&model.Asset{}, a.ID).Error; err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "db error")
	}
	s.db.Where("asset_id = ?", a.ID).Delete(&model.AssetValue{})
	s.logActivity(uid, "asset_removed", assetActivityTitle("Удалено", a), fmtAmount(a.Value, a.Currency), a.Value, a.Currency, before)
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
	payoff, err := parseDate(in.PayoffDate)
	if err != nil {
		return model.Asset{}, echo.NewHTTPError(http.StatusBadRequest, "invalid payoffDate")
	}
	return model.Asset{
		UserID:              uid,
		Kind:                kind,
		Name:                in.Name,
		Currency:            in.Currency,
		Value:               in.Value,
		Invested:            in.Invested,
		MonthlyIncome:       in.MonthlyIncome,
		MonthlyPayment:      in.MonthlyPayment,
		RatePercent:         in.RatePercent,
		PurchaseDate:        purchase,
		MaintenanceHours:    in.MaintenanceHours,
		Subtype:             in.Subtype,
		Status:              in.Status,
		ExcludeFromNetWorth: in.ExcludeFromNetWorth,
		DebtScheme:          in.DebtScheme,
		LoanType:            in.LoanType,
		TermMonths:          in.TermMonths,
		FirstPaymentDate:    firstPay,
		PayoffDate:          payoff,
		LinkedAssetID:       in.LinkedAssetID,
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
