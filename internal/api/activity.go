package api

import (
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"

	"capitalapp/internal/calc"
	"capitalapp/internal/model"

	"github.com/labstack/echo/v4"
)

// logActivity records one action and how it moved net worth. netBeforeUSD is the
// net worth captured just before the change; the "after" is read now. Best-effort
// — a logging failure must never block the actual operation.
func (s *Server) logActivity(uid uint, kind, title, detail string, amount float64, currency string, netBeforeUSD float64) {
	afterUSD, _ := s.netWorthUSD(uid)
	_ = s.db.Create(&model.Activity{
		UserID: uid, Kind: kind, Title: title, Detail: detail,
		Amount: amount, Currency: currency,
		NetBeforeUSD: netBeforeUSD, NetAfterUSD: afterUSD,
	}).Error
}

func assetActivityTitle(verb string, a model.Asset) string {
	return fmt.Sprintf("%s · %s «%s»", verb, kindLabels[a.Kind], a.Name)
}

// groupNum renders 3540000 as "3 540 000".
func groupNum(n int64) string {
	neg := n < 0
	if neg {
		n = -n
	}
	s := strconv.FormatInt(n, 10)
	var out []byte
	for i := 0; i < len(s); i++ {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ' ')
		}
		out = append(out, s[i])
	}
	if neg {
		return "-" + string(out)
	}
	return string(out)
}

// fmtAmount renders a value with its currency symbol, matching the app's style.
func fmtAmount(v float64, cur string) string {
	s := groupNum(int64(math.Round(v)))
	switch cur {
	case "USD":
		return "$" + s
	case "TJS":
		return s + " смн"
	case "UZS":
		return s + " сўм"
	default:
		return s + " " + cur
	}
}

// assetEditDetail describes what actually changed on an edit — "было → стало" —
// so the log answers "what changed and by how much", not just the new value.
func assetEditDetail(oldA, newA model.Asset) string {
	var parts []string
	if oldA.Value != newA.Value || oldA.Currency != newA.Currency {
		parts = append(parts, fmt.Sprintf("%s → %s", fmtAmount(oldA.Value, oldA.Currency), fmtAmount(newA.Value, newA.Currency)))
	}
	if oldA.Name != newA.Name {
		parts = append(parts, fmt.Sprintf("«%s» → «%s»", oldA.Name, newA.Name))
	}
	if newA.ExcludeFromNetWorth {
		parts = append(parts, "вне капитала")
	}
	if len(parts) == 0 {
		return fmtAmount(newA.Value, newA.Currency)
	}
	return strings.Join(parts, " · ")
}

// perDollar renders a stored rate (value of 1 unit in USD) as the friendlier
// "how many <currency> per $1" — 9.24 for TJS, 12690 for UZS.
func perDollar(cur string, perUSD float64) string {
	if perUSD <= 0 {
		return "—"
	}
	v := 1 / perUSD
	if cur == "UZS" {
		return fmt.Sprintf("%.0f", v)
	}
	return fmt.Sprintf("%.2f", v)
}

// rateChangeDetail summarises which currencies moved, e.g. "TJS 9.24 → 9.30".
// Returns "" if nothing meaningfully changed.
func rateChangeDetail(oldR, newR calc.Rates) string {
	var parts []string
	for _, cur := range editableCurrencies {
		o, n := oldR[cur], newR[cur]
		if o <= 0 || n <= 0 || math.Abs(o-n)/o < 0.0001 {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s %s → %s", cur, perDollar(cur, o), perDollar(cur, n)))
	}
	return strings.Join(parts, ", ")
}

type activityResp struct {
	model.Activity
	NetBefore float64 `json:"netBefore"`
	NetAfter  float64 `json:"netAfter"`
	ChangeAbs float64 `json:"changeAbs"`
}

func (s *Server) listActivity(c echo.Context) error {
	uid := c.Get(ctxUserID).(uint)
	base, err := s.userBase(uid)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "user not found")
	}
	rates, err := s.userRates(uid)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "db error")
	}
	var rows []model.Activity
	if err := s.db.Where("user_id = ?", uid).Order("created_at desc").Limit(200).Find(&rows).Error; err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "db error")
	}
	out := make([]activityResp, 0, len(rows))
	for _, a := range rows {
		nb := round2(calc.Convert(a.NetBeforeUSD, "USD", base, rates))
		na := round2(calc.Convert(a.NetAfterUSD, "USD", base, rates))
		out = append(out, activityResp{Activity: a, NetBefore: nb, NetAfter: na, ChangeAbs: round2(na - nb)})
	}
	return c.JSON(http.StatusOK, echo.Map{"baseCurrency": base, "items": out})
}
