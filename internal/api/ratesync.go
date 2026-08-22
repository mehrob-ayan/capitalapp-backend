package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"capitalapp/internal/calc"
	"capitalapp/internal/model"

	"github.com/labstack/echo/v4"
)

// Rate sources. The Central Bank of Uzbekistan is the primary, most accurate
// source for UZS and TJS (it also quotes TJS, from which we derive TJS/USD via
// the UZS cross). open.er-api is a no-key fallback if the CBU is unreachable.
const cbuEndpoint = "https://cbu.uz/ru/arkhiv-kursov-valyut/json/"
const nbtEndpoint = "https://nbt.tj/ru/kurs/kurs.php"
const ratesEndpoint = "https://open.er-api.com/v6/latest/USD"

var (
	nbtRowRe  = regexp.MustCompile(`(?s)<tr[^>]*>(.*?)</tr>`)
	nbtCellRe = regexp.MustCompile(`(?s)<td[^>]*>(.*?)</td>`)
	nbtTagRe  = regexp.MustCompile(`<[^>]*>`)
)

// fetchNBT scrapes the National Bank of Tajikistan rates page for the official
// USD→TJS rate and returns the value of 1 TJS in USD. The table row for code
// 840 (US Dollar) has columns: №, code, nominal, name, rate.
func fetchNBT(ctx context.Context) (float64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, nbtEndpoint, nil)
	if err != nil {
		return 0, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("nbt status %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return 0, err
	}
	cellText := func(cell string) string {
		return strings.TrimSpace(nbtTagRe.ReplaceAllString(cell, ""))
	}
	for _, row := range nbtRowRe.FindAllStringSubmatch(string(raw), -1) {
		cells := nbtCellRe.FindAllStringSubmatch(row[1], -1)
		if len(cells) < 5 || cellText(cells[1][1]) != "840" {
			continue
		}
		nominal, e1 := strconv.ParseFloat(cellText(cells[2][1]), 64)
		rate, e2 := strconv.ParseFloat(cellText(cells[4][1]), 64) // TJS per `nominal` USD
		if e1 == nil && e2 == nil && rate > 0 && nominal > 0 {
			return nominal / rate, nil // value of 1 TJS in USD
		}
	}
	return 0, fmt.Errorf("nbt: USD row not found")
}

const rateSyncInterval = 23 * time.Hour

// fetchCBU returns the value of 1 unit in USD (the stored form) for the editable
// currencies, from the Central Bank of Uzbekistan.
func fetchCBU(ctx context.Context) (map[string]float64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cbuEndpoint, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("cbu status %d", resp.StatusCode)
	}
	var rows []struct {
		Ccy     string `json:"Ccy"`
		Rate    string `json:"Rate"`
		Nominal string `json:"Nominal"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		return nil, err
	}
	// UZS per 1 unit, for each listed currency.
	uzsPer := map[string]float64{}
	for _, r := range rows {
		rate, _ := strconv.ParseFloat(r.Rate, 64)
		nom, _ := strconv.ParseFloat(r.Nominal, 64)
		if rate > 0 && nom > 0 {
			uzsPer[r.Ccy] = rate / nom
		}
	}
	usd := uzsPer["USD"] // UZS per 1 USD
	if usd <= 0 {
		return nil, fmt.Errorf("cbu: no USD rate")
	}
	out := map[string]float64{
		"UZS": 1 / usd, // value of 1 UZS in USD
	}
	if tjs := uzsPer["TJS"]; tjs > 0 {
		out["TJS"] = tjs / usd // value of 1 TJS in USD (via the UZS cross)
	}
	return out, nil
}

// fetchERAPI is the no-key fallback source (units of currency per 1 USD).
func fetchERAPI(ctx context.Context) (map[string]float64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ratesEndpoint, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("rates api status %d", resp.StatusCode)
	}
	var body struct {
		Result string             `json:"result"`
		Rates  map[string]float64 `json:"rates"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	if body.Result != "success" {
		return nil, fmt.Errorf("rates api result %q", body.Result)
	}
	out := map[string]float64{}
	for _, cur := range editableCurrencies {
		if v, ok := body.Rates[cur]; ok && v > 0 {
			out[cur] = 1 / v // value of 1 unit in USD
		}
	}
	return out, nil
}

// syncRates fetches fresh rates and upserts the user's editable currencies.
// A failure (offline, API down) is non-fatal: existing manual rates stay.
// `title` is the activity-log label (differs for auto vs manual refresh).
func (s *Server) syncRates(uid uint, title string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// UZS from the Central Bank of Uzbekistan; TJS from the National Bank of
	// Tajikistan directly (falling back to the CBU cross). If both central-bank
	// sources fail entirely, use the no-key API.
	perUSD, err := fetchCBU(ctx)
	if err != nil {
		perUSD = map[string]float64{}
	}
	if tjs, e := fetchNBT(ctx); e == nil && tjs > 0 {
		perUSD["TJS"] = tjs // official NBT rate overrides the CBU cross
	}
	if len(perUSD) == 0 {
		perUSD, err = fetchERAPI(ctx)
		if err != nil {
			return err
		}
	}

	before, _ := s.netWorthUSD(uid)
	oldR, _ := s.userRates(uid)
	for _, cur := range editableCurrencies {
		v, ok := perUSD[cur]
		if !ok || v <= 0 {
			continue
		}
		if err := s.db.Where(model.Rate{UserID: uid, Currency: cur}).
			Assign(model.Rate{PerUSD: v}).
			FirstOrCreate(&model.Rate{}).Error; err != nil {
			return err
		}
	}
	if newR, err := s.userRates(uid); err == nil {
		s.recordRateHistory(uid, newR, time.Now())
		if detail := rateChangeDetail(oldR, newR); detail != "" {
			s.logActivity(uid, "rate_changed", title, detail, 0, "", before)
		}
	}
	return s.db.Model(&model.User{}).Where("id = ?", uid).
		Update("rates_synced_at", time.Now()).Error
}

// recordRateHistory upserts the day's rate for each editable currency, so past
// snapshots can be valued at the rate that actually applied on their day.
func (s *Server) recordRateHistory(uid uint, rates calc.Rates, day time.Time) {
	day = day.UTC().Truncate(24 * time.Hour)
	for _, cur := range editableCurrencies {
		v := rates[cur]
		if v <= 0 {
			continue
		}
		_ = s.db.Where(model.RateHistory{UserID: uid, Currency: cur, Date: day}).
			Assign(model.RateHistory{PerUSD: v}).
			FirstOrCreate(&model.RateHistory{}).Error
	}
}

// ratesAsOf returns the rate map in effect on `day` — the most recent recorded
// rate on-or-before that day, falling back to the user's current rate when no
// history exists (e.g. legacy days before this feature).
func (s *Server) ratesAsOf(uid uint, day time.Time) calc.Rates {
	day = day.UTC().Truncate(24 * time.Hour)
	r := calc.Rates{"USD": 1}
	for _, cur := range editableCurrencies {
		var row model.RateHistory
		if err := s.db.Where("user_id = ? AND currency = ? AND date <= ?", uid, cur, day).
			Order("date desc").First(&row).Error; err == nil && row.PerUSD > 0 {
			r[cur] = row.PerUSD
		}
	}
	cur, err := s.userRates(uid)
	if err == nil {
		for k, v := range cur {
			if r[k] == 0 {
				r[k] = v
			}
		}
	}
	return r
}

// maybeSyncRates refreshes rates if the user opted in and it's been a day.
// Called from the worker; safe to run often.
func (s *Server) maybeSyncRates(uid uint) {
	var u model.User
	if err := s.db.First(&u, uid).Error; err != nil {
		return
	}
	if !u.AutoRates || time.Since(u.RatesSyncedAt) < rateSyncInterval {
		return
	}
	if err := s.syncRates(uid, "Курс обновлён автоматически"); err != nil {
		log.Printf("worker: rate sync user %d: %v", uid, err)
	}
}

// refreshRates pulls the current market rate on demand (a button in Settings),
// regardless of the auto-sync toggle, and returns the updated rate map.
func (s *Server) refreshRates(c echo.Context) error {
	uid := c.Get(ctxUserID).(uint)
	if err := s.syncRates(uid, "Курс обновлён с биржи"); err != nil {
		return echo.NewHTTPError(http.StatusBadGateway, "не удалось получить курс с биржи")
	}
	r, err := s.userRates(uid)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "db error")
	}
	return c.JSON(http.StatusOK, r)
}
