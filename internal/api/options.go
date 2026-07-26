package api

import (
	"net/http"
	"time"

	"capitalapp/internal/calc"
	"capitalapp/internal/model"

	"github.com/labstack/echo/v4"
)

// Option grant lifecycle. A grant moves pending → vesting → vested purely as a
// function of dates, so its status is always correct on read (no cron needed).
const (
	optPending = "pending" // grant date is in the future — not received yet
	optVesting = "vesting" // received, but the crystallization period hasn't elapsed
	optVested  = "vested"  // crystallized: now real shares, counts toward net worth
)

func vestDate(g model.OptionGrant) time.Time {
	return g.GrantDate.AddDate(0, g.VestMonths, 0)
}

func optionStatus(g model.OptionGrant, asOf time.Time) string {
	switch {
	case asOf.Before(g.GrantDate):
		return optPending
	case asOf.Before(vestDate(g)):
		return optVesting
	default:
		return optVested
	}
}

// vestedOptions returns the value (in base currency) and count of grants
// crystallized as of asOf — the part that has become real shares and belongs in
// net worth. Grants still vesting or not yet received are excluded. Converts at
// current rates, like the rest of the app.
func (s *Server) vestedOptions(uid uint, base string, rates calc.Rates, asOf time.Time) (val float64, count int) {
	var grants []model.OptionGrant
	if err := s.db.Where("user_id = ?", uid).Find(&grants).Error; err != nil {
		return 0, 0
	}
	for _, g := range grants {
		if optionStatus(g, asOf) == optVested {
			val += calc.Convert(g.Quantity*g.UnitPrice, g.Currency, base, rates)
			count++
		}
	}
	return val, count
}

// optionsTotalBase is the value of ALL grants (pending + vesting + vested) in
// base currency — for the "total options" card on the overview. Vested grants
// are already inside net worth; this figure is purely informational.
func (s *Server) optionsTotalBase(uid uint, base string, rates calc.Rates) float64 {
	var grants []model.OptionGrant
	if err := s.db.Where("user_id = ?", uid).Find(&grants).Error; err != nil {
		return 0
	}
	var sum float64
	for _, g := range grants {
		sum += calc.Convert(g.Quantity*g.UnitPrice, g.Currency, base, rates)
	}
	return round2(sum)
}

type optionInput struct {
	Name       string  `json:"name"`
	Quantity   float64 `json:"quantity"`
	UnitPrice  float64 `json:"unitPrice"`
	Currency   string  `json:"currency"`
	GrantDate  *string `json:"grantDate"`
	VestMonths int     `json:"vestMonths"`
}

type optionResp struct {
	model.OptionGrant
	Status    string  `json:"status"`
	VestDate  string  `json:"vestDate"`
	ValueBase float64 `json:"valueBase"`
}

type optionsListResp struct {
	BaseCurrency string       `json:"baseCurrency"`
	Grants       []optionResp `json:"grants"`
	VestedBase   float64      `json:"vestedBase"`
	VestingBase  float64      `json:"vestingBase"`
	PendingBase  float64      `json:"pendingBase"`
	TotalBase    float64      `json:"totalBase"`
}

func (s *Server) listOptions(c echo.Context) error {
	uid := c.Get(ctxUserID).(uint)
	base, err := s.userBase(uid)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "user not found")
	}
	rates, err := s.userRates(uid)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "db error")
	}
	var grants []model.OptionGrant
	if err := s.db.Where("user_id = ?", uid).Order("grant_date").Find(&grants).Error; err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "db error")
	}
	now := time.Now()
	out := make([]optionResp, 0, len(grants))
	var vested, vesting, pending float64
	for _, g := range grants {
		st := optionStatus(g, now)
		vb := round2(calc.Convert(g.Quantity*g.UnitPrice, g.Currency, base, rates))
		switch st {
		case optVested:
			vested += vb
		case optVesting:
			vesting += vb
		default:
			pending += vb
		}
		out = append(out, optionResp{OptionGrant: g, Status: st, VestDate: vestDate(g).Format("2006-01-02"), ValueBase: vb})
	}
	return c.JSON(http.StatusOK, optionsListResp{
		BaseCurrency: base,
		Grants:       out,
		VestedBase:   round2(vested),
		VestingBase:  round2(vesting),
		PendingBase:  round2(pending),
		TotalBase:    round2(vested + vesting + pending),
	})
}

func (s *Server) createOption(c echo.Context) error {
	uid := c.Get(ctxUserID).(uint)
	in, gd, err := bindOption(c)
	if err != nil {
		return err
	}
	g := model.OptionGrant{
		UserID: uid, Name: in.Name, Quantity: in.Quantity, UnitPrice: in.UnitPrice,
		Currency: in.Currency, GrantDate: *gd, VestMonths: in.VestMonths,
	}
	if g.Name == "" {
		g.Name = "Опционы"
	}
	before, _ := s.netWorthUSD(uid)
	if err := s.db.Create(&g).Error; err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "db error")
	}
	s.logActivity(uid, "option_added", "Добавлены опционы «"+g.Name+"»", fmtAmount(g.Quantity*g.UnitPrice, g.Currency), g.Quantity*g.UnitPrice, g.Currency, before)
	return c.JSON(http.StatusCreated, s.buildOptionResp(uid, g))
}

func (s *Server) updateOption(c echo.Context) error {
	uid := c.Get(ctxUserID).(uint)
	g, err := s.findOption(uid, c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "option not found")
	}
	in, gd, err := bindOption(c)
	if err != nil {
		return err
	}
	oldVal, oldCur := g.Quantity*g.UnitPrice, g.Currency
	g.Name = in.Name
	if g.Name == "" {
		g.Name = "Опционы"
	}
	g.Quantity = in.Quantity
	g.UnitPrice = in.UnitPrice
	g.Currency = in.Currency
	g.GrantDate = *gd
	g.VestMonths = in.VestMonths
	newVal := g.Quantity * g.UnitPrice
	detail := fmtAmount(newVal, g.Currency)
	if oldVal != newVal || oldCur != g.Currency {
		detail = fmtAmount(oldVal, oldCur) + " → " + fmtAmount(newVal, g.Currency)
	}
	before, _ := s.netWorthUSD(uid)
	if err := s.db.Save(&g).Error; err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "db error")
	}
	s.logActivity(uid, "option_edited", "Изменены опционы «"+g.Name+"»", detail, newVal, g.Currency, before)
	return c.JSON(http.StatusOK, s.buildOptionResp(uid, g))
}

func (s *Server) deleteOption(c echo.Context) error {
	uid := c.Get(ctxUserID).(uint)
	g, err := s.findOption(uid, c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "option not found")
	}
	before, _ := s.netWorthUSD(uid)
	if err := s.db.Delete(&model.OptionGrant{}, g.ID).Error; err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "db error")
	}
	s.logActivity(uid, "option_removed", "Удалены опционы «"+g.Name+"»", fmtAmount(g.Quantity*g.UnitPrice, g.Currency), g.Quantity*g.UnitPrice, g.Currency, before)
	return c.NoContent(http.StatusNoContent)
}

// ---- helpers ----

func bindOption(c echo.Context) (optionInput, *time.Time, error) {
	var in optionInput
	if err := c.Bind(&in); err != nil {
		return in, nil, echo.NewHTTPError(http.StatusBadRequest, "invalid body")
	}
	if !validCurrency[in.Currency] {
		return in, nil, echo.NewHTTPError(http.StatusBadRequest, "unsupported currency")
	}
	if in.Quantity <= 0 || in.UnitPrice < 0 {
		return in, nil, echo.NewHTTPError(http.StatusBadRequest, "invalid quantity or price")
	}
	if in.VestMonths < 0 {
		return in, nil, echo.NewHTTPError(http.StatusBadRequest, "invalid vesting period")
	}
	gd, err := parseDate(in.GrantDate)
	if err != nil {
		return in, nil, err
	}
	if gd == nil {
		return in, nil, echo.NewHTTPError(http.StatusBadRequest, "grantDate required")
	}
	return in, gd, nil
}

func (s *Server) findOption(uid uint, idParam string) (model.OptionGrant, error) {
	var g model.OptionGrant
	id, err := parseID(idParam)
	if err != nil {
		return g, err
	}
	err = s.db.Where("id = ? AND user_id = ?", id, uid).First(&g).Error
	return g, err
}

func (s *Server) buildOptionResp(uid uint, g model.OptionGrant) optionResp {
	base, _ := s.userBase(uid)
	rates, _ := s.userRates(uid)
	return optionResp{
		OptionGrant: g,
		Status:      optionStatus(g, time.Now()),
		VestDate:    vestDate(g).Format("2006-01-02"),
		ValueBase:   round2(calc.Convert(g.Quantity*g.UnitPrice, g.Currency, base, rates)),
	}
}
