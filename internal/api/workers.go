package api

import (
	"encoding/json"
	"log"
	"time"

	"capitalapp/internal/calc"
	"capitalapp/internal/model"
)

// This service runs only on the user's own machine, which sleeps at night and
// may be off for days. Interest accrual and capitalization are computed on read
// (deterministic from an anchor date), so downtime never corrupts them. The one
// thing that accumulates per-day is history snapshots — the catch-up worker
// backfills any days missed while the machine was asleep/off.

const (
	workerInterval  = time.Hour
	maxBackfillDays = 400 // guard against pathological gaps / clock jumps
)

// runWorkers backfills on startup (covers being off for days), then ticks
// hourly. After the machine wakes from sleep the next tick backfills every day
// that was missed in one pass. Meant to be started in its own goroutine.
func (s *Server) runWorkers() {
	s.catchUp()
	t := time.NewTicker(workerInterval)
	defer t.Stop()
	for range t.C {
		s.catchUp()
	}
}

// catchUp backfills snapshots for every user. Idempotent: safe to run any number
// of times. Isolated so one user's error (or panic) can't stop the others.
func (s *Server) catchUp() {
	var ids []uint
	if err := s.db.Model(&model.User{}).Pluck("id", &ids).Error; err != nil {
		log.Printf("worker: list users: %v", err)
		return
	}
	for _, uid := range ids {
		s.catchUpUser(uid)
	}
}

func (s *Server) catchUpUser(uid uint) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("worker: recovered while backfilling user %d: %v", uid, r)
		}
	}()
	if err := s.backfillSnapshots(uid); err != nil {
		log.Printf("worker: backfill user %d: %v", uid, err)
	}
	s.maybeSyncRates(uid)
}

// backfillSnapshots writes a snapshot for each day from the day after the last
// snapshot up to today. Each day's net worth is recomputed as of that day, which
// is exact: nothing could have been edited while the machine was unavailable.
func (s *Server) backfillSnapshots(uid uint) error {
	rates, err := s.userRates(uid)
	if err != nil {
		return err
	}

	today := time.Now().UTC().Truncate(24 * time.Hour)

	start := today
	var last model.Snapshot
	if err := s.db.Where("user_id = ?", uid).Order("date desc").First(&last).Error; err == nil {
		start = last.Date.UTC().Truncate(24*time.Hour).AddDate(0, 0, 1)
	}
	if gap := int(today.Sub(start).Hours() / 24); gap > maxBackfillDays {
		start = today.AddDate(0, 0, -maxBackfillDays)
		log.Printf("worker: user %d backfill capped at %d days", uid, maxBackfillDays)
	}

	// Remember today's rates so future backfills can value missed days at the
	// rate that was actually in effect, not whatever it is when we catch up.
	s.recordRateHistory(uid, rates, today)

	// Fill any days missed while off, each valued at end-of-day and its own rate.
	for day := start; day.Before(today); day = day.AddDate(0, 0, 1) {
		sn, err := s.computeDayTotals(uid, day.AddDate(0, 0, 1).Add(-time.Second))
		if err != nil {
			return err
		}
		s.upsertSnapshotDay(uid, day, sn)
	}
	// Always refresh today with a fresh, live valuation.
	sn, err := s.computeDayTotals(uid, time.Now())
	if err != nil {
		return err
	}
	s.upsertSnapshotDay(uid, today, sn)
	return nil
}

// computeDayTotals values net worth as of asOf in every supported currency,
// using the rate that applied on that day (#13). Storing all currencies per day
// keeps history honest: сомони holdings show a flat сомони line, and FX movement
// shows only in the USD view. Also records the per-kind composition (#10).
func (s *Server) computeDayTotals(uid uint, asOf time.Time) (model.Snapshot, error) {
	rates := s.ratesAsOf(uid, asOf)
	var sn model.Snapshot
	for _, cur := range currencyList {
		a, l, err := s.totalsAsOf(uid, cur, rates, asOf)
		if err != nil {
			return sn, err
		}
		switch cur {
		case "USD":
			sn.NetWorthUSD, sn.AssetsUSD, sn.LiabilitiesUSD = a-l, a, l
		case "TJS":
			sn.NetWorthTJS, sn.AssetsTJS, sn.LiabilitiesTJS = a-l, a, l
		case "UZS":
			sn.NetWorthUZS, sn.AssetsUZS, sn.LiabilitiesUZS = a-l, a, l
		}
	}
	if comp, err := json.Marshal(s.compositionUSD(uid, rates, asOf)); err == nil {
		sn.CompositionUSD = string(comp)
	}
	return sn, nil
}

// compositionUSD is the per-kind breakdown of net-worth assets in USD on asOf —
// non-flagged assets by kind, plus vested options. Feeds the composition chart.
func (s *Server) compositionUSD(uid uint, rates calc.Rates, asOf time.Time) map[string]float64 {
	var assets []model.Asset
	if err := s.db.Where("user_id = ?", uid).Find(&assets).Error; err != nil {
		return nil
	}
	m := map[string]float64{}
	for _, a := range assets {
		if a.ExcludeFromNetWorth || a.Kind == model.KindDebt {
			continue
		}
		mt := calc.Compute(a, "USD", rates, asOf)
		m[string(a.Kind)] += mt.ValueBase
	}
	if v, _ := s.vestedOptions(uid, "USD", rates, asOf); v > 0 {
		m["options"] = v
	}
	return m
}

// totalsAsOf sums the user's assets and liabilities, in the given currency, as
// of a moment in time.
func (s *Server) totalsAsOf(uid uint, base string, rates calc.Rates, asOf time.Time) (assets, liab float64, err error) {
	var list []model.Asset
	if err = s.db.Where("user_id = ?", uid).Find(&list).Error; err != nil {
		return 0, 0, err
	}
	for _, a := range list {
		if a.ExcludeFromNetWorth {
			continue // shown in totals, but out of net worth (history/goals)
		}
		m := calc.Compute(a, base, rates, asOf)
		assets += m.ValueBase
		liab += m.LiabilityBase
	}
	// Crystallized option grants have become real shares by asOf, so they count
	// as assets. Grants still vesting (or not yet received) are excluded.
	vested, _ := s.vestedOptions(uid, base, rates, asOf)
	assets += vested
	return assets, liab, nil
}
