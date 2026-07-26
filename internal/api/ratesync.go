package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"capitalapp/internal/calc"
	"capitalapp/internal/model"
)

// ratesEndpoint is a free, no-key FX source that covers TJS and UZS.
// It returns "1 USD = N <currency>"; we store the inverse (value of 1 unit in USD).
const ratesEndpoint = "https://open.er-api.com/v6/latest/USD"

const rateSyncInterval = 23 * time.Hour

// syncRates fetches fresh rates and upserts the user's editable currencies.
// A failure (offline, API down) is non-fatal: existing manual rates stay.
func (s *Server) syncRates(uid uint) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ratesEndpoint, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("rates api status %d", resp.StatusCode)
	}

	var body struct {
		Result string             `json:"result"`
		Rates  map[string]float64 `json:"rates"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return err
	}
	if body.Result != "success" {
		return fmt.Errorf("rates api result %q", body.Result)
	}

	before, _ := s.netWorthUSD(uid)
	oldR, _ := s.userRates(uid)
	for _, cur := range editableCurrencies {
		perUSDBase, ok := body.Rates[cur] // units of `cur` per 1 USD
		if !ok || perUSDBase <= 0 {
			continue
		}
		perUSD := 1 / perUSDBase
		if err := s.db.Where(model.Rate{UserID: uid, Currency: cur}).
			Assign(model.Rate{PerUSD: perUSD}).
			FirstOrCreate(&model.Rate{}).Error; err != nil {
			return err
		}
	}
	if newR, err := s.userRates(uid); err == nil {
		s.recordRateHistory(uid, newR, time.Now())
		if detail := rateChangeDetail(oldR, newR); detail != "" {
			s.logActivity(uid, "rate_changed", "Курс обновлён автоматически", detail, 0, "", before)
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
	if err := s.syncRates(uid); err != nil {
		log.Printf("worker: rate sync user %d: %v", uid, err)
	}
}
