package api

import (
	"math"
	"net/http"
	"time"

	"capitalapp/internal/calc"
	"capitalapp/internal/model"

	"github.com/labstack/echo/v4"
)

// upsertSnapshotDay writes (or overwrites) the net-worth snapshot for a given
// day. Idempotent — the (user, date) unique index makes re-runs harmless.
func (s *Server) upsertSnapshotDay(uid uint, day time.Time, netUSD, assetsUSD, liabUSD float64) {
	day = day.UTC().Truncate(24 * time.Hour)
	_ = s.db.
		Where(model.Snapshot{UserID: uid, Date: day}).
		Assign(model.Snapshot{NetWorthUSD: netUSD, AssetsUSD: assetsUSD, LiabilitiesUSD: liabUSD}).
		FirstOrCreate(&model.Snapshot{}).Error
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
	Date     string  `json:"date"`
	NetWorth float64 `json:"netWorth"`
}

type historyResp struct {
	BaseCurrency  string         `json:"baseCurrency"`
	Points        []historyPoint `json:"points"`
	Current       float64        `json:"current"`
	ChangeAbs     float64        `json:"changeAbs"`
	ChangePercent float64        `json:"changePercent"`
}

func (s *Server) history(c echo.Context) error {
	uid := c.Get(ctxUserID).(uint)
	base, err := s.userBase(uid)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "user not found")
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

	points := make([]historyPoint, 0, len(snaps))
	for _, sn := range snaps {
		points = append(points, historyPoint{
			Date:     sn.Date.Format("2006-01-02"),
			NetWorth: round2(calc.Convert(sn.NetWorthUSD, "USD", base, rates)),
		})
	}

	resp := historyResp{BaseCurrency: base, Points: points}
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
