package api

import (
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
	if start.After(today) {
		return nil // already up to date
	}
	if gap := int(today.Sub(start).Hours() / 24); gap > maxBackfillDays {
		start = today.AddDate(0, 0, -maxBackfillDays)
		log.Printf("worker: user %d backfill capped at %d days", uid, maxBackfillDays)
	}

	for day := start; !day.After(today); day = day.AddDate(0, 0, 1) {
		asOf := day.AddDate(0, 0, 1).Add(-time.Second) // end of that day
		if day.Equal(today) {
			asOf = time.Now()
		}
		assets, liab, err := s.totalsAsOf(uid, rates, asOf)
		if err != nil {
			return err
		}
		s.upsertSnapshotDay(uid, day, assets-liab, assets, liab)
	}
	return nil
}

// totalsAsOf sums the user's assets and liabilities in USD as of a moment in time.
func (s *Server) totalsAsOf(uid uint, rates calc.Rates, asOf time.Time) (assetsUSD, liabUSD float64, err error) {
	var assets []model.Asset
	if err = s.db.Where("user_id = ?", uid).Find(&assets).Error; err != nil {
		return 0, 0, err
	}
	for _, a := range assets {
		m := calc.Compute(a, "USD", rates, asOf)
		assetsUSD += m.ValueBase
		liabUSD += m.LiabilityBase
	}
	return assetsUSD, liabUSD, nil
}
