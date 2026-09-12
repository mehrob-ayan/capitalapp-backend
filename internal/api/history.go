package api

import (
	"encoding/json"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"capitalapp/internal/calc"
	"capitalapp/internal/model"

	"github.com/labstack/echo/v4"
)

// upsertSnapshotDay writes (or overwrites) the day's snapshot in every currency.
// Idempotent — the (user, date) unique index makes re-runs harmless. Assign uses
// a map so zero values (e.g. zero liabilities) are written, not skipped.
func (s *Server) upsertSnapshotDay(uid uint, day time.Time, sn model.Snapshot) {
	day = day.UTC().Truncate(24 * time.Hour)
	_ = s.db.
		Where(model.Snapshot{UserID: uid, Date: day}).
		Assign(map[string]any{
			"net_worth_usd": sn.NetWorthUSD, "assets_usd": sn.AssetsUSD, "liabilities_usd": sn.LiabilitiesUSD,
			"net_worth_tjs": sn.NetWorthTJS, "assets_tjs": sn.AssetsTJS, "liabilities_tjs": sn.LiabilitiesTJS,
			"net_worth_uzs": sn.NetWorthUZS, "assets_uzs": sn.AssetsUZS, "liabilities_uzs": sn.LiabilitiesUZS,
			"composition_usd": sn.CompositionUSD,
		}).
		FirstOrCreate(&model.Snapshot{}).Error
}

// snapshotIn returns a snapshot's net worth / assets / liabilities in the given
// currency. New snapshots store every currency at that day's rate (honest — no
// cross-currency wobble). Legacy rows predating per-currency storage hold only
// USD; those fall back to converting USD at the current rate.
func snapshotIn(sn model.Snapshot, base string, rates calc.Rates) (net, assets, liab float64) {
	switch base {
	case "TJS":
		if sn.NetWorthTJS != 0 || sn.AssetsTJS != 0 || sn.LiabilitiesTJS != 0 {
			return sn.NetWorthTJS, sn.AssetsTJS, sn.LiabilitiesTJS
		}
	case "UZS":
		if sn.NetWorthUZS != 0 || sn.AssetsUZS != 0 || sn.LiabilitiesUZS != 0 {
			return sn.NetWorthUZS, sn.AssetsUZS, sn.LiabilitiesUZS
		}
	default:
		return sn.NetWorthUSD, sn.AssetsUSD, sn.LiabilitiesUSD
	}
	return calc.Convert(sn.NetWorthUSD, "USD", base, rates),
		calc.Convert(sn.AssetsUSD, "USD", base, rates),
		calc.Convert(sn.LiabilitiesUSD, "USD", base, rates)
}

// tracksValue lists kinds whose value the user sets manually and which benefit
// from a per-asset value chart (they appreciate/depreciate over time).
var tracksValue = map[model.AssetKind]bool{
	model.KindRealEstate: true,
	model.KindCar:        true,
	model.KindInvestment: true,
	model.KindMetals:     true,
}

// recordAssetValue upserts today's value point for an asset. Best-effort.
func (s *Server) recordAssetValue(assetID, userID uint, value float64) {
	if assetID == 0 {
		return
	}
	day := time.Now().UTC().Truncate(24 * time.Hour)
	_ = s.db.
		Where(model.AssetValue{AssetID: assetID, Date: day}).
		Assign(model.AssetValue{UserID: userID, Value: value}).
		FirstOrCreate(&model.AssetValue{}).Error
}

type assetValuePoint struct {
	Date  string  `json:"date"`
	Value float64 `json:"value"`
}

type assetHistoryResp struct {
	Currency string            `json:"currency"`
	Points   []assetValuePoint `json:"points"`
}

// assetHistory returns the value-over-time points for a single asset.
func (s *Server) assetHistory(c echo.Context) error {
	uid := c.Get(ctxUserID).(uint)
	a, err := s.findAsset(uid, c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "asset not found")
	}
	var rows []model.AssetValue
	if err := s.db.Where("asset_id = ?", a.ID).Order("date asc").Find(&rows).Error; err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "db error")
	}
	points := make([]assetValuePoint, 0, len(rows))
	for _, r := range rows {
		points = append(points, assetValuePoint{Date: r.Date.Format("2006-01-02"), Value: round2(r.Value)})
	}
	return c.JSON(http.StatusOK, assetHistoryResp{Currency: a.Currency, Points: points})
}

type historyPoint struct {
	Date        string  `json:"date"`
	NetWorth    float64 `json:"netWorth"`
	Assets      float64 `json:"assets"`
	Liabilities float64 `json:"liabilities"`
	Interest    float64 `json:"interest"` // daily interest cost of debts that existed on this day
	Note        string  `json:"note"`
}

type historyResp struct {
	BaseCurrency  string         `json:"baseCurrency"`
	Points        []historyPoint `json:"points"`
	Current       float64        `json:"current"`
	ChangeAbs     float64        `json:"changeAbs"`
	ChangePercent float64        `json:"changePercent"`
	DailyInterest float64        `json:"dailyInterest"`
}

// dailyInterestCost sums the interest that net-worth debts accrue per day, in
// base currency. Deterministic (balance × annual-rate / 365) — unlike the
// snapshot-diff "average per day", it is not polluted by FX drift or manual
// revaluations, so it honestly shows how much debt erodes capital each day.
func (s *Server) dailyInterestCost(uid uint, base string, rates calc.Rates) float64 {
	var assets []model.Asset
	if err := s.db.Where("user_id = ?", uid).Find(&assets).Error; err != nil {
		return 0
	}
	return dailyInterestAt(assets, base, rates, time.Now())
}

// dailyInterestAt sums the daily interest cost of the given debts as of asOf,
// skipping debts that did not exist yet. Pure, so the history endpoint can call
// it once per snapshot day to draw the "interest over time" line.
func dailyInterestAt(assets []model.Asset, base string, rates calc.Rates, asOf time.Time) float64 {
	var cost float64
	for _, a := range assets {
		if a.Kind != model.KindDebt || a.ExcludeFromNetWorth || a.RatePercent <= 0 {
			continue
		}
		if a.CreatedAt.After(asOf) {
			continue // debt didn't exist on this day
		}
		m := calc.Compute(a, base, rates, asOf)
		cost += m.LiabilityBase * a.RatePercent / 100 / 365
	}
	return round2(cost)
}

func (s *Server) history(c echo.Context) error {
	uid := c.Get(ctxUserID).(uint)
	base, err := s.userBase(uid)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "user not found")
	}
	// Optional ?currency= lets the History screen compare capital in each
	// currency without changing the user's global base.
	if cur := c.QueryParam("currency"); validCurrency[cur] {
		base = cur
	}
	rates, err := s.userRates(uid)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "db error")
	}

	q := s.db.Where("user_id = ?", uid)
	if cutoff := periodCutoff(c.QueryParam("period")); !cutoff.IsZero() {
		q = q.Where("date >= ?", cutoff)
	}
	var snaps []model.Snapshot
	if err := q.Order("date asc").Find(&snaps).Error; err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "db error")
	}

	// Load debts once and derive each day's interest cost from the balances that
	// existed then — an honest "interest over time" line without a schema change.
	var debts []model.Asset
	s.db.Where("user_id = ? AND kind = ?", uid, model.KindDebt).Find(&debts)

	points := make([]historyPoint, 0, len(snaps))
	for _, sn := range snaps {
		nw, as, li := snapshotIn(sn, base, rates)
		endOfDay := sn.Date.UTC().Truncate(24 * time.Hour).Add(24*time.Hour - time.Second)
		points = append(points, historyPoint{
			Date:        sn.Date.Format("2006-01-02"),
			NetWorth:    round2(nw),
			Assets:      round2(as),
			Liabilities: round2(li),
			Interest:    dailyInterestAt(debts, base, rates, endOfDay),
			Note:        sn.Note,
		})
	}

	resp := historyResp{BaseCurrency: base, Points: points, DailyInterest: s.dailyInterestCost(uid, base, rates)}
	if n := len(points); n > 0 {
		resp.Current = points[n-1].NetWorth
		first := points[0].NetWorth
		resp.ChangeAbs = round2(resp.Current - first)
		if first != 0 {
			resp.ChangePercent = round2((resp.Current - first) / math.Abs(first) * 100)
		}
	}
	return c.JSON(http.StatusOK, resp)
}

// ---- composition over time (#10) ----

type compositionPoint struct {
	Date  string             `json:"date"`
	Parts map[string]float64 `json:"parts"`
}

type compositionResp struct {
	BaseCurrency string             `json:"baseCurrency"`
	Kinds        []string           `json:"kinds"`
	Points       []compositionPoint `json:"points"`
}

func (s *Server) composition(c echo.Context) error {
	uid := c.Get(ctxUserID).(uint)
	base, err := s.userBase(uid)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "user not found")
	}
	if cur := c.QueryParam("currency"); validCurrency[cur] {
		base = cur
	}
	rates, err := s.userRates(uid)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "db error")
	}
	q := s.db.Where("user_id = ?", uid)
	if cutoff := periodCutoff(c.QueryParam("period")); !cutoff.IsZero() {
		q = q.Where("date >= ?", cutoff)
	}
	var snaps []model.Snapshot
	if err := q.Order("date asc").Find(&snaps).Error; err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "db error")
	}

	// Collapse to one point per calendar month (the month's last snapshot) —
	// this chart is "composition BY MONTH", so daily snapshots within a month
	// must not each become their own column. Snapshots arrive date-ascending,
	// so overwriting keeps the latest and `order` preserves chronology.
	lastOfMonth := make(map[string]model.Snapshot)
	var order []string
	for _, sn := range snaps {
		if sn.CompositionUSD == "" {
			continue
		}
		key := sn.Date.Format("2006-01")
		if _, seen := lastOfMonth[key]; !seen {
			order = append(order, key)
		}
		lastOfMonth[key] = sn
	}

	present := map[string]bool{}
	points := make([]compositionPoint, 0, len(order))
	for _, key := range order {
		sn := lastOfMonth[key]
		var raw map[string]float64
		if json.Unmarshal([]byte(sn.CompositionUSD), &raw) != nil {
			continue
		}
		parts := make(map[string]float64, len(raw))
		for k, v := range raw {
			parts[k] = round2(calc.Convert(v, "USD", base, rates))
			present[k] = true
		}
		points = append(points, compositionPoint{Date: sn.Date.Format("2006-01-02"), Parts: parts})
	}
	kinds := make([]string, 0, len(present))
	for _, k := range kindOrder {
		if k != model.KindDebt && present[string(k)] {
			kinds = append(kinds, string(k))
		}
	}
	if present["options"] {
		kinds = append(kinds, "options")
	}
	return c.JSON(http.StatusOK, compositionResp{BaseCurrency: base, Kinds: kinds, Points: points})
}

// ---- manual snapshot fix (#12) ----

func (s *Server) deleteSnapshot(c echo.Context) error {
	uid := c.Get(ctxUserID).(uint)
	day, err := time.Parse("2006-01-02", c.Param("date"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad date")
	}
	s.db.Where("user_id = ? AND date = ?", uid, day.UTC().Truncate(24*time.Hour)).Delete(&model.Snapshot{})
	return c.NoContent(http.StatusNoContent)
}

type snapshotPatch struct {
	NetWorth float64 `json:"netWorth"`
}

// patchSnapshot overrides a day's net worth (manual correction). The entered
// value is in the user's base currency; other currencies are derived at current
// rates. Assets are set equal to net, liabilities to zero — a manual override
// isn't a full breakdown.
func (s *Server) patchSnapshot(c echo.Context) error {
	uid := c.Get(ctxUserID).(uint)
	base, err := s.userBase(uid)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "user not found")
	}
	rates, err := s.userRates(uid)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "db error")
	}
	day, err := time.Parse("2006-01-02", c.Param("date"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad date")
	}
	var in snapshotPatch
	if err := c.Bind(&in); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid body")
	}
	conv := func(cur string) float64 { return round2(calc.Convert(in.NetWorth, base, cur, rates)) }
	upd := map[string]any{
		"net_worth_usd": conv("USD"), "assets_usd": conv("USD"), "liabilities_usd": 0,
		"net_worth_tjs": conv("TJS"), "assets_tjs": conv("TJS"), "liabilities_tjs": 0,
		"net_worth_uzs": conv("UZS"), "assets_uzs": conv("UZS"), "liabilities_uzs": 0,
	}
	if err := s.db.Where(model.Snapshot{UserID: uid, Date: day.UTC().Truncate(24 * time.Hour)}).
		Assign(upd).FirstOrCreate(&model.Snapshot{}).Error; err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "db error")
	}
	return c.NoContent(http.StatusOK)
}

type snapshotNote struct {
	Note string `json:"note"`
}

// patchSnapshotNote sets (or clears) the free-text note on a day's snapshot,
// without touching the stored capital figures. Kept separate from
// patchSnapshot so annotating a day never overwrites its assets/liabilities
// breakdown. Creates the row if the day has no snapshot yet.
func (s *Server) patchSnapshotNote(c echo.Context) error {
	uid := c.Get(ctxUserID).(uint)
	day, err := time.Parse("2006-01-02", c.Param("date"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad date")
	}
	var in snapshotNote
	if err := c.Bind(&in); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid body")
	}
	if err := s.db.Where(model.Snapshot{UserID: uid, Date: day.UTC().Truncate(24 * time.Hour)}).
		Assign(map[string]any{"note": strings.TrimSpace(in.Note)}).
		FirstOrCreate(&model.Snapshot{}).Error; err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "db error")
	}
	return c.NoContent(http.StatusOK)
}

// ---- monthly cash flow ----

type flowPoint struct {
	Month    string  `json:"month"` // YYYY-MM
	Income   float64 `json:"income"`
	Payments float64 `json:"payments"` // debt payments out
	Net      float64 `json:"net"`      // income − payments
}

type flowResp struct {
	BaseCurrency string      `json:"baseCurrency"`
	Points       []flowPoint `json:"points"`
}

// flow aggregates real account movements by calendar month: income (excluding
// opening balances and deposit transfers) minus debt payments, converted to the
// user's base currency. This is the actual money flow over time, complementing
// the projected "поток в месяц" tile on the overview.
func (s *Server) flow(c echo.Context) error {
	uid := c.Get(ctxUserID).(uint)
	base, err := s.userBase(uid)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "user not found")
	}
	if cur := c.QueryParam("currency"); validCurrency[cur] {
		base = cur
	}
	rates, err := s.userRates(uid)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "db error")
	}

	type row struct {
		Date     time.Time
		Kind     string
		Amount   float64
		Source   string
		Currency string
	}
	q := s.db.Table("account_entries ae").
		Select("ae.date, ae.kind, ae.amount, ae.source, a.currency").
		Joins("JOIN assets a ON a.id = ae.account_id").
		Where("ae.user_id = ?", uid)
	if cutoff := periodCutoff(c.QueryParam("period")); !cutoff.IsZero() {
		q = q.Where("ae.date >= ?", cutoff)
	}
	var rows []row
	if err := q.Scan(&rows).Error; err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "db error")
	}

	type agg struct{ income, payments float64 }
	byMonth := map[string]*agg{}
	order := []string{}
	for _, r := range rows {
		amt := calc.Convert(r.Amount, r.Currency, base, rates)
		key := r.Date.Format("2006-01")
		a, ok := byMonth[key]
		if !ok {
			a = &agg{}
			byMonth[key] = a
			order = append(order, key)
		}
		switch {
		case r.Source == "debt_payment":
			a.payments += amt
		case r.Kind == "income" && r.Source != "opening" && r.Source != "deposit_topup":
			a.income += amt
		}
	}
	sort.Strings(order)
	points := make([]flowPoint, 0, len(order))
	for _, k := range order {
		a := byMonth[k]
		points = append(points, flowPoint{
			Month: k, Income: round2(a.income), Payments: round2(a.payments), Net: round2(a.income - a.payments),
		})
	}
	return c.JSON(http.StatusOK, flowResp{BaseCurrency: base, Points: points})
}

// ---- what changed in liabilities over the period ----

type debtChange struct {
	Name        string  `json:"name"`
	Currency    string  `json:"currency"`
	Paid        float64 `json:"paid"`        // total paid in period, base currency
	Outstanding float64 `json:"outstanding"` // current balance, base currency
	Closed      bool    `json:"closed"`
	Payments    int     `json:"payments"`
}

type debtChangesResp struct {
	BaseCurrency string       `json:"baseCurrency"`
	PaidTotal    float64      `json:"paidTotal"`
	Items        []debtChange `json:"items"`
}

// debtChanges breaks down how liabilities moved over the period: how much was
// paid toward each debt and which debts are now closed. Powers the "что
// изменилось" panel on the Обязательства view.
func (s *Server) debtChanges(c echo.Context) error {
	uid := c.Get(ctxUserID).(uint)
	base, err := s.userBase(uid)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "user not found")
	}
	if cur := c.QueryParam("currency"); validCurrency[cur] {
		base = cur
	}
	rates, err := s.userRates(uid)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "db error")
	}

	type sumRow struct {
		LinkedDebtID uint
		N            int
		Sum          float64
	}
	q := s.db.Table("account_entries").
		Select("linked_debt_id, count(*) as n, sum(debt_amount) as sum").
		Where("user_id = ? AND source = ? AND linked_debt_id IS NOT NULL", uid, "debt_payment")
	if cutoff := periodCutoff(c.QueryParam("period")); !cutoff.IsZero() {
		q = q.Where("date >= ?", cutoff)
	}
	var sums []sumRow
	if err := q.Group("linked_debt_id").Scan(&sums).Error; err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "db error")
	}

	var debts []model.Asset
	s.db.Where("user_id = ? AND kind = ?", uid, model.KindDebt).Find(&debts)
	byID := map[uint]model.Asset{}
	for _, d := range debts {
		byID[d.ID] = d
	}

	now := time.Now()
	var resp debtChangesResp
	resp.BaseCurrency = base
	for _, sr := range sums {
		d, ok := byID[sr.LinkedDebtID]
		if !ok {
			continue
		}
		paid := calc.Convert(sr.Sum, d.Currency, base, rates)
		out := calc.Compute(d, base, rates, now).LiabilityBase
		resp.Items = append(resp.Items, debtChange{
			Name: d.Name, Currency: d.Currency, Paid: round2(paid),
			Outstanding: round2(out), Closed: out <= 0.5, Payments: sr.N,
		})
		resp.PaidTotal += paid
	}
	resp.PaidTotal = round2(resp.PaidTotal)
	sort.Slice(resp.Items, func(i, j int) bool { return resp.Items[i].Paid > resp.Items[j].Paid })
	return c.JSON(http.StatusOK, resp)
}

// ---- what changed in assets over the period ----

type assetChange struct {
	Kind  string  `json:"kind"`
	Start float64 `json:"start"`
	Now   float64 `json:"now"`
	Delta float64 `json:"delta"`
}

type assetChangesResp struct {
	BaseCurrency string        `json:"baseCurrency"`
	TotalDelta   float64       `json:"totalDelta"`
	Items        []assetChange `json:"items"`
}

// assetChanges breaks down how assets moved over the period by category, from
// the per-kind composition stored in each day's snapshot. Powers the "что
// изменилось" panel on the Активы view.
func (s *Server) assetChanges(c echo.Context) error {
	uid := c.Get(ctxUserID).(uint)
	base, err := s.userBase(uid)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "user not found")
	}
	if cur := c.QueryParam("currency"); validCurrency[cur] {
		base = cur
	}
	rates, err := s.userRates(uid)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "db error")
	}

	q := s.db.Where("user_id = ?", uid)
	if cutoff := periodCutoff(c.QueryParam("period")); !cutoff.IsZero() {
		q = q.Where("date >= ?", cutoff)
	}
	var snaps []model.Snapshot
	if err := q.Order("date asc").Find(&snaps).Error; err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "db error")
	}

	resp := assetChangesResp{BaseCurrency: base}
	// Find the first and last snapshot in the window that carry a composition.
	var first, last map[string]float64
	for _, sn := range snaps {
		if sn.CompositionUSD == "" {
			continue
		}
		var m map[string]float64
		if json.Unmarshal([]byte(sn.CompositionUSD), &m) != nil {
			continue
		}
		if first == nil {
			first = m
		}
		last = m
	}
	if first == nil || last == nil {
		return c.JSON(http.StatusOK, resp)
	}

	kinds := map[string]bool{}
	for k := range first {
		kinds[k] = true
	}
	for k := range last {
		kinds[k] = true
	}
	for k := range kinds {
		startB := calc.Convert(first[k], "USD", base, rates)
		nowB := calc.Convert(last[k], "USD", base, rates)
		if math.Abs(startB) < 0.5 && math.Abs(nowB) < 0.5 {
			continue
		}
		resp.Items = append(resp.Items, assetChange{
			Kind: k, Start: round2(startB), Now: round2(nowB), Delta: round2(nowB - startB),
		})
		resp.TotalDelta += nowB - startB
	}
	resp.TotalDelta = round2(resp.TotalDelta)
	sort.Slice(resp.Items, func(i, j int) bool { return resp.Items[i].Delta > resp.Items[j].Delta })
	return c.JSON(http.StatusOK, resp)
}

func periodCutoff(period string) time.Time {
	now := time.Now()
	switch period {
	case "1m":
		return now.AddDate(0, -1, 0)
	case "6m":
		return now.AddDate(0, -6, 0)
	case "1y":
		return now.AddDate(-1, 0, 0)
	default:
		return time.Time{} // all
	}
}
