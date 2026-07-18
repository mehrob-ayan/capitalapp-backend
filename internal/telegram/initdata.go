// Package telegram validates the initData string that a Telegram Mini App
// passes to the backend, per https://core.telegram.org/bots/webapps#validating-data-received-via-the-mini-app
package telegram

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

type User struct {
	ID           int64  `json:"id"`
	Username     string `json:"username"`
	FirstName    string `json:"first_name"`
	LastName     string `json:"last_name"`
	LanguageCode string `json:"language_code"`
}

type InitData struct {
	User     User
	AuthDate time.Time
}

var (
	ErrNoHash  = errors.New("init data: missing hash")
	ErrBadHash = errors.New("init data: signature mismatch")
	ErrExpired = errors.New("init data: expired")
	ErrNoUser  = errors.New("init data: missing user")
)

// Validate checks the HMAC signature of rawInitData against botToken and, if
// maxAge > 0, rejects data older than maxAge. On success it returns the parsed user.
func Validate(rawInitData, botToken string, maxAge time.Duration) (*InitData, error) {
	values, err := url.ParseQuery(rawInitData)
	if err != nil {
		return nil, err
	}

	hash := values.Get("hash")
	if hash == "" {
		return nil, ErrNoHash
	}
	values.Del("hash")

	// data_check_string: keys sorted alphabetically, "key=value" joined by newlines.
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(values.Get(k))
	}

	secret := hmacSHA256([]byte("WebAppData"), []byte(botToken))
	computed := hmacSHA256(secret, []byte(b.String()))
	if !hmac.Equal([]byte(hex.EncodeToString(computed)), []byte(hash)) {
		return nil, ErrBadHash
	}

	authDate := time.Time{}
	if ad := values.Get("auth_date"); ad != "" {
		if sec, err := strconv.ParseInt(ad, 10, 64); err == nil {
			authDate = time.Unix(sec, 0)
		}
	}
	if maxAge > 0 && !authDate.IsZero() && time.Since(authDate) > maxAge {
		return nil, ErrExpired
	}

	userJSON := values.Get("user")
	if userJSON == "" {
		return nil, ErrNoUser
	}
	var u User
	if err := json.Unmarshal([]byte(userJSON), &u); err != nil {
		return nil, err
	}

	return &InitData{User: u, AuthDate: authDate}, nil
}

func hmacSHA256(key, msg []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(msg)
	return h.Sum(nil)
}
