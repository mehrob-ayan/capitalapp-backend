// Package telegrambot runs a long-polling Telegram bot that turns group
// messages into household transactions. Long polling means the Mac only needs
// outbound internet (no public endpoint), and Telegram queues updates ~24h, so
// a sleeping/off Mac catches up on wake automatically.
package telegrambot

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"capitalapp/internal/model"

	"gorm.io/gorm"
)

var quickAmounts = []float64{10000, 20000, 50000, 100000}

type Bot struct {
	token     string
	db        *gorm.DB
	http      *http.Client
	offset    int64
	household uint

	mu      sync.Mutex
	pending map[int64]draft // telegram user id -> awaiting manual amount
}

type draft struct {
	typ    string // "e" | "i"
	catIdx int
}

// Run blocks, polling Telegram until the process exits. Start it in a goroutine.
func Run(db *gorm.DB, token string) {
	b := &Bot{
		token:   token,
		db:      db,
		http:    &http.Client{Timeout: 45 * time.Second},
		pending: map[int64]draft{},
	}
	b.household = resolveHousehold(db)
	log.Printf("telegram bot: started (household user %d)", b.household)
	for {
		updates, err := b.getUpdates()
		if err != nil {
			log.Printf("telegram bot: getUpdates: %v", err)
			time.Sleep(3 * time.Second)
			continue
		}
		for _, u := range updates {
			b.offset = u.UpdateID + 1
			b.handle(u)
		}
	}
}

// ---- Telegram API types (minimal) ----

type Update struct {
	UpdateID      int64          `json:"update_id"`
	Message       *Message       `json:"message"`
	CallbackQuery *CallbackQuery `json:"callback_query"`
}
type Message struct {
	MessageID int64  `json:"message_id"`
	From      *User  `json:"from"`
	Chat      Chat   `json:"chat"`
	Text      string `json:"text"`
}
type User struct {
	ID        int64  `json:"id"`
	FirstName string `json:"first_name"`
	Username  string `json:"username"`
}
type Chat struct {
	ID int64 `json:"id"`
}
type CallbackQuery struct {
	ID      string   `json:"id"`
	From    User     `json:"from"`
	Message *Message `json:"message"`
	Data    string   `json:"data"`
}
type button struct {
	Text string `json:"text"`
	Data string `json:"callback_data"`
}
type keyboard struct {
	Inline [][]button `json:"inline_keyboard"`
}

// ---- API calls ----

func (b *Bot) api(method string) string {
	return "https://api.telegram.org/bot" + b.token + "/" + method
}

func (b *Bot) getUpdates() ([]Update, error) {
	v := url.Values{}
	v.Set("timeout", "30")
	v.Set("offset", strconv.FormatInt(b.offset, 10))
	v.Set("allowed_updates", `["message","callback_query"]`)
	resp, err := b.http.Get(b.api("getUpdates") + "?" + v.Encode())
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		OK     bool     `json:"ok"`
		Result []Update `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if !out.OK {
		return nil, errors.New("telegram getUpdates not ok")
	}
	return out.Result, nil
}

func (b *Bot) post(method string, payload map[string]any) {
	buf, _ := json.Marshal(payload)
	resp, err := b.http.Post(b.api(method), "application/json", bytes.NewReader(buf))
	if err != nil {
		log.Printf("telegram bot: %s: %v", method, err)
		return
	}
	resp.Body.Close()
}

func (b *Bot) send(chatID int64, text string, kb *keyboard) {
	p := map[string]any{"chat_id": chatID, "text": text}
	if kb != nil {
		p["reply_markup"] = kb
	}
	b.post("sendMessage", p)
}

func (b *Bot) edit(chatID, msgID int64, text string, kb *keyboard) {
	p := map[string]any{"chat_id": chatID, "message_id": msgID, "text": text}
	if kb != nil {
		p["reply_markup"] = kb
	}
	b.post("editMessageText", p)
}

func (b *Bot) answer(cbID string) {
	b.post("answerCallbackQuery", map[string]any{"callback_query_id": cbID})
}

// ---- routing ----

func (b *Bot) handle(u Update) {
	switch {
	case u.CallbackQuery != nil:
		b.onCallback(u.CallbackQuery)
	case u.Message != nil && u.Message.Text != "":
		b.onMessage(u.Message)
	}
}

func (b *Bot) onMessage(m *Message) {
	text := strings.TrimSpace(m.Text)
	cmd := strings.ToLower(strings.SplitN(text, "@", 2)[0])
	if cmd == "/add" || cmd == "/start" {
		b.send(m.Chat.ID, "Что добавить?", typeKeyboard())
		return
	}

	// Awaiting a manually-typed amount from this user?
	if m.From != nil {
		b.mu.Lock()
		d, waiting := b.pending[m.From.ID]
		if waiting {
			delete(b.pending, m.From.ID)
		}
		b.mu.Unlock()
		if waiting {
			amt, err := parseAmount(text)
			if err != nil {
				b.mu.Lock()
				b.pending[m.From.ID] = d
				b.mu.Unlock()
				b.send(m.Chat.ID, "Не понял сумму — напишите число, напр. 20000.", nil)
				return
			}
			cat := catName(d.typ, d.catIdx)
			b.record(d.typ, cat, amt, "", personOf(m.From), m.Chat.ID, 0)
			b.send(m.Chat.ID, confirm(d.typ, cat, amt, personOf(m.From)), nil)
			return
		}
	}

	// Free-text shortcut, e.g. "кофе 20000" or "+зп 5000000".
	amt, catText, ok := parseFreeText(text)
	if !ok {
		return // ordinary chatter — ignore
	}
	if b.exists(m.Chat.ID, m.MessageID) {
		return // already imported (dedup)
	}
	typ := "e"
	if strings.HasPrefix(strings.TrimSpace(text), "+") {
		typ = "i"
	}
	cat, note := matchCategory(typ, catText)
	b.record(typ, cat, amt, note, personOf(m.From), m.Chat.ID, m.MessageID)
	b.send(m.Chat.ID, confirm(typ, cat, amt, personOf(m.From)), nil)
}

func (b *Bot) onCallback(cq *CallbackQuery) {
	defer b.answer(cq.ID)
	if cq.Message == nil {
		return
	}
	chatID, msgID := cq.Message.Chat.ID, cq.Message.MessageID
	parts := strings.Split(cq.Data, ":")

	switch parts[0] {
	case "t": // t:<typ> — type chosen, show categories
		if len(parts) < 2 {
			return
		}
		b.edit(chatID, msgID, "Категория:", categoryKeyboard(parts[1]))
	case "c": // c:<typ>:<idx> — category chosen, show amounts
		if len(parts) < 3 {
			return
		}
		idx, _ := strconv.Atoi(parts[2])
		b.edit(chatID, msgID, catName(parts[1], idx)+" — сумма:", amountKeyboard(parts[1], idx))
	case "a": // a:<typ>:<idx>:<amount> — done
		if len(parts) < 4 {
			return
		}
		idx, _ := strconv.Atoi(parts[2])
		amt, _ := strconv.ParseFloat(parts[3], 64)
		cat := catName(parts[1], idx)
		b.record(parts[1], cat, amt, "", personOf(&cq.From), chatID, 0)
		b.edit(chatID, msgID, confirm(parts[1], cat, amt, personOf(&cq.From)), nil)
	case "m": // m:<typ>:<idx> — manual amount entry
		if len(parts) < 3 {
			return
		}
		idx, _ := strconv.Atoi(parts[2])
		b.mu.Lock()
		b.pending[cq.From.ID] = draft{typ: parts[1], catIdx: idx}
		b.mu.Unlock()
		b.edit(chatID, msgID, catName(parts[1], idx)+" — введите сумму числом:", nil)
	}
}

// ---- keyboards ----

func typeKeyboard() *keyboard {
	return &keyboard{Inline: [][]button{{
		{Text: "🔴 Расход", Data: "t:e"},
		{Text: "🟢 Доход", Data: "t:i"},
	}}}
}

func categoryKeyboard(typ string) *keyboard {
	cats := categories(typ)
	rows := [][]button{}
	for i := 0; i < len(cats); i += 2 {
		row := []button{{Text: cats[i], Data: fmt.Sprintf("c:%s:%d", typ, i)}}
		if i+1 < len(cats) {
			row = append(row, button{Text: cats[i+1], Data: fmt.Sprintf("c:%s:%d", typ, i+1)})
		}
		rows = append(rows, row)
	}
	return &keyboard{Inline: rows}
}

func amountKeyboard(typ string, idx int) *keyboard {
	rows := [][]button{}
	row := []button{}
	for _, a := range quickAmounts {
		row = append(row, button{Text: groupInt(a), Data: fmt.Sprintf("a:%s:%d:%.0f", typ, idx, a)})
		if len(row) == 2 {
			rows = append(rows, row)
			row = []button{}
		}
	}
	if len(row) > 0 {
		rows = append(rows, row)
	}
	rows = append(rows, []button{{Text: "✏️ Ввести вручную", Data: fmt.Sprintf("m:%s:%d", typ, idx)}})
	return &keyboard{Inline: rows}
}

// ---- data helpers ----

func categories(typ string) []string {
	if typ == "i" {
		return model.IncomeCategories
	}
	return model.ExpenseCategories
}

func catName(typ string, idx int) string {
	cs := categories(typ)
	if idx >= 0 && idx < len(cs) {
		return cs[idx]
	}
	return "Прочее"
}

func (b *Bot) record(typ, cat string, amt float64, note, person string, chatID, msgID int64) {
	t := model.Transaction{
		UserID: b.household, Date: time.Now(),
		Type:     typeWord(typ),
		Category: cat, Amount: amt, Currency: "UZS", Person: person, Note: note,
		Source: "bot", TelegramChatID: chatID, TelegramMsgID: msgID,
	}
	if err := b.db.Create(&t).Error; err != nil {
		log.Printf("telegram bot: save transaction: %v", err)
	}
}

func (b *Bot) exists(chatID, msgID int64) bool {
	if msgID == 0 {
		return false
	}
	var n int64
	b.db.Model(&model.Transaction{}).
		Where("telegram_chat_id = ? AND telegram_msg_id = ?", chatID, msgID).Count(&n)
	return n > 0
}

func resolveHousehold(db *gorm.DB) uint {
	var u model.User
	if err := db.Order("id").First(&u).Error; err == nil {
		return u.ID
	}
	// No user yet — create the local household account (same as dev login).
	u = model.User{TelegramID: 1, FirstName: "Home", BaseCurrency: "USD"}
	db.Where(model.User{TelegramID: 1}).FirstOrCreate(&u)
	return u.ID
}

func typeWord(typ string) string {
	if typ == "i" {
		return "income"
	}
	return "expense"
}

func personOf(u *User) string {
	if u == nil {
		return ""
	}
	if u.FirstName != "" {
		return u.FirstName
	}
	return u.Username
}

func confirm(typ, cat string, amt float64, person string) string {
	label := "🔴 Расход"
	if typ == "i" {
		label = "🟢 Доход"
	}
	who := ""
	if person != "" {
		who = " · " + person
	}
	return fmt.Sprintf("✅ %s · %s · %s сум%s", label, cat, groupInt(amt), who)
}

// ---- parsing ----

var errBadAmount = errors.New("bad amount")

func parseAmount(s string) (float64, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, " ", "")
	s = strings.ReplaceAll(s, " ", "")
	mult := 1.0
	switch {
	case strings.HasSuffix(s, "млн"):
		mult, s = 1_000_000, strings.TrimSuffix(s, "млн")
	case strings.HasSuffix(s, "тыс"):
		mult, s = 1000, strings.TrimSuffix(s, "тыс")
	case strings.HasSuffix(s, "к"):
		mult, s = 1000, strings.TrimSuffix(s, "к")
	case strings.HasSuffix(s, "k"):
		mult, s = 1000, strings.TrimSuffix(s, "k")
	}
	s = strings.ReplaceAll(s, ",", ".")
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v <= 0 {
		return 0, errBadAmount
	}
	return v * mult, nil
}

// parseFreeText pulls an amount and a category text out of a line like
// "кофе 20000". Returns ok=false if there's no amount (ordinary chatter).
func parseFreeText(text string) (float64, string, bool) {
	text = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(text), "+"))
	fields := strings.Fields(text)
	amtIdx, amt := -1, 0.0
	for i := len(fields) - 1; i >= 0; i-- {
		if v, err := parseAmount(fields[i]); err == nil {
			amt, amtIdx = v, i
			break
		}
	}
	if amtIdx < 0 {
		return 0, "", false
	}
	parts := make([]string, 0, len(fields))
	for i, f := range fields {
		if i != amtIdx {
			parts = append(parts, f)
		}
	}
	return amt, strings.Join(parts, " "), true
}

// synonyms maps common words to a fixed category (case-insensitive substring).
var synonyms = map[string]string{
	"кофе": "Кафе/Кофе", "кафе": "Кафе/Кофе", "обед": "Кафе/Кофе", "ужин": "Кафе/Кофе", "ресторан": "Кафе/Кофе",
	"продукт": "Продукты", "еда": "Продукты", "магазин": "Продукты",
	"такси": "Транспорт", "автобус": "Транспорт", "метро": "Транспорт", "бензин": "Транспорт",
	"комм": "Дом/Коммуналка", "аренда": "Дом/Коммуналка", "свет": "Дом/Коммуналка",
	"аптек": "Здоровье", "врач": "Здоровье", "здоров": "Здоровье",
	"интернет": "Связь", "связь": "Связь", "телефон": "Связь",
	"зп": "Зарплата", "зарплат": "Зарплата", "оклад": "Зарплата",
}

// matchCategory maps free-text to a fixed category. Returns the category and,
// when it couldn't be matched, the original text as a note so nothing is lost.
func matchCategory(typ, text string) (cat, note string) {
	lc := strings.ToLower(strings.TrimSpace(text))
	if lc == "" {
		return "Прочее", ""
	}
	cats := categories(typ)
	// exact-ish: the fixed category name contains the text (or vice versa)
	for _, c := range cats {
		clc := strings.ToLower(c)
		if strings.Contains(clc, lc) || strings.Contains(lc, clc) {
			return c, ""
		}
	}
	for key, c := range synonyms {
		if strings.Contains(lc, key) && contains(cats, c) {
			return c, ""
		}
	}
	return "Прочее", text
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func groupInt(v float64) string {
	n := int64(v + 0.5)
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ' ')
		}
		out = append(out, c)
	}
	if neg {
		return "-" + string(out)
	}
	return string(out)
}
