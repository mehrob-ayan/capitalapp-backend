package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

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
	return s.db.Model(&model.User{}).Where("id = ?", uid).
		Update("rates_synced_at", time.Now()).Error
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
