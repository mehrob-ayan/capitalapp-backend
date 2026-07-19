package db

import (
	"capitalapp/internal/model"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func Connect(url string) (*gorm.DB, error) {
	return gorm.Open(postgres.Open(url), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Warn),
	})
}

// Migrate keeps the schema in sync. New models get appended here as domains land.
func Migrate(db *gorm.DB) error {
	return db.AutoMigrate(
		&model.User{},
		&model.Asset{},
		&model.Rate{},
		&model.Snapshot{},
		&model.Goal{},
		&model.AssetValue{},
		&model.Transaction{},
	)
}
