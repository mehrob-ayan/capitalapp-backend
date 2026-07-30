package api

import (
	"net/http"
	"sort"
	"time"

	"capitalapp/internal/model"

	"github.com/labstack/echo/v4"
)

// synthIDBase offsets account-derived (synthetic) transaction IDs so they never
// collide with real Transaction IDs. These rows aren't stored — they mirror
// account movements into the expenses view at read time.
const synthIDBase = 1_000_000_000

// accountCatSource maps an account movement to an expenses category + a source
// tag the UI uses to mark it read-only (edited from the account ledger only).
func accountCatSource(e model.AccountEntry) (category, source string) {
	switch e.Source {
	case "salary_auto":
		return "Зарплата", "salary_auto"
	case "debt_payment":
		return "Кредиты и долги", "debt_payment"
	case "lent_repayment":
		return "Возврат долга", "lent_repayment"
	default: // manual entry on an account
		if e.Kind == "income" {
			return "Пополнение счёта", "account"
		}
		return "Прочее", "account"
	}
}

// accountCashRows turns account movements in [start,end) into read-only
// pseudo-transactions so real cash flow (salary, debt payments, top-ups) shows
// up in the expenses module. Opening balances are skipped — they're initial
// state, not income.
func (s *Server) accountCashRows(uid uint, start, end time.Time) []model.Transaction {
	var entries []model.AccountEntry
	s.db.Where("user_id = ? AND date >= ? AND date < ? AND source <> 'opening'", uid, start, end).Find(&entries)
	if len(entries) == 0 {
		return nil
	}
	accCur := map[uint]string{}
	var accs []model.Asset
	s.db.Where("user_id = ? AND is_account = ?", uid, true).Find(&accs)
	for _, a := range accs {
		accCur[a.ID] = a.Currency
	}
	rows := make([]model.Transaction, 0, len(entries))
	for _, e := range entries {
		typ := "expense"
		if e.Kind == "income" {
			typ = "income"
		}
		cat, src := accountCatSource(e)
		rows = append(rows, model.Transaction{
			ID: synthIDBase + e.ID, UserID: uid, Date: e.Date, Type: typ, Category: cat,
			Amount: round2(e.Amount), Currency: accCur[e.AccountID], Note: e.Note, Source: src,
		})
	}
	return rows
}

func sortTxDesc(txs []model.Transaction) {
	sort.SliceStable(txs, func(i, j int) bool {
		if !txs[i].Date.Equal(txs[j].Date) {
			return txs[i].Date.After(txs[j].Date)
		}
		return txs[i].ID > txs[j].ID
	})
}

func (s *Server) expenseCategories(c echo.Context) error {
	return c.JSON(http.StatusOK, echo.Map{
		"expense": model.ExpenseCategories,
		"income":  model.IncomeCategories,
	})
}

type categorySlice struct {
	Category string  `json:"category"`
	Amount   float64 `json:"amount"`
	Percent  float64 `json:"percent"`
}

type monthTrend struct {
	Month   string  `json:"month"`
	Income  float64 `json:"income"`
	Expense float64 `json:"expense"`
}

type expensesResp struct {
	Month        string              `json:"month"`
	Currency     string              `json:"currency"`
	Income       float64             `json:"income"`
	Expense      float64             `json:"expense"`
	Balance      float64             `json:"balance"`
	ByCategory   []categorySlice     `json:"byCategory"`
	Trend        []monthTrend        `json:"trend"`
	People       []string            `json:"people"`
	Transactions []model.Transaction `json:"transactions"`
}

// expenses returns the month's income/expense summary, category breakdown (for
// the donut) and the transaction list. ?month=YYYY-MM (default current),
// ?person=Name filters by who logged it.
func (s *Server) expenses(c echo.Context) error {
	uid := c.Get(ctxUserID).(uint)

	monthParam := c.QueryParam("month")
	start := monthStart(monthParam)
	end := start.AddDate(0, 1, 0)
	person := c.QueryParam("person")

	q := s.db.Where("user_id = ? AND date >= ? AND date < ?", uid, start, end)
	if person != "" {
		q = q.Where("person = ?", person)
	}
	var txs []model.Transaction
	if err := q.Order("date desc, id desc").Find(&txs).Error; err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "db error")
	}
	// Account movements (salary, debt payments, top-ups) belong to the whole
	// household, not a specific person — only fold them in on the unfiltered view.
	if person == "" {
		txs = append(txs, s.accountCashRows(uid, start, end)...)
		sortTxDesc(txs)
	}

	// Distinct people this month (ignores the person filter, for the chips).
	var people []string
	s.db.Model(&model.Transaction{}).
		Where("user_id = ? AND date >= ? AND date < ? AND person <> ''", uid, start, end).
		Distinct().Pluck("person", &people)

	var income, expense float64
	byCat := map[string]float64{}
	for _, t := range txs {
		if t.Type == "income" {
			income += t.Amount
		} else {
			expense += t.Amount
			byCat[t.Category] += t.Amount
		}
	}

	cats := make([]categorySlice, 0, len(byCat))
	for name, amt := range byCat {
		pct := 0.0
		if expense > 0 {
			pct = amt / expense * 100
		}
		cats = append(cats, categorySlice{Category: name, Amount: round2(amt), Percent: round2(pct)})
	}
	sortCategoriesDesc(cats)

	cur := "UZS"
	if len(txs) > 0 && txs[0].Currency != "" {
		cur = txs[0].Currency
	}

	return c.JSON(http.StatusOK, expensesResp{
		Month:        start.Format("2006-01"),
		Currency:     cur,
		Income:       round2(income),
		Expense:      round2(expense),
		Balance:      round2(income - expense),
		ByCategory:   cats,
		Trend:        s.monthlyTrend(uid, start, end),
		People:       people,
		Transactions: txs,
	})
}

type expenseInput struct {
	Type     string  `json:"type"`
	Category string  `json:"category"`
	Amount   float64 `json:"amount"`
	Currency string  `json:"currency"`
	Person   string  `json:"person"`
	Date     *string `json:"date"`
	Note     string  `json:"note"`
}

func (s *Server) createExpense(c echo.Context) error {
	uid := c.Get(ctxUserID).(uint)
	var in expenseInput
	if err := c.Bind(&in); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid body")
	}
	if in.Type != "income" && in.Type != "expense" {
		return echo.NewHTTPError(http.StatusBadRequest, "type must be income or expense")
	}
	if in.Amount <= 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "amount must be positive")
	}
	date := time.Now()
	if in.Date != nil && *in.Date != "" {
		if d, err := time.Parse("2006-01-02", *in.Date); err == nil {
			date = d
		}
	}
	cur := in.Currency
	if cur == "" {
		cur = "UZS"
	}
	t := model.Transaction{
		UserID: uid, Date: date, Type: in.Type, Category: in.Category,
		Amount: in.Amount, Currency: cur, Person: in.Person, Note: in.Note, Source: "manual",
	}
	if err := s.db.Create(&t).Error; err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "db error")
	}
	return c.JSON(http.StatusCreated, t)
}

func (s *Server) deleteExpense(c echo.Context) error {
	uid := c.Get(ctxUserID).(uint)
	id, err := parseID(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad id")
	}
	var t model.Transaction
	if err := s.db.Where("id = ? AND user_id = ?", id, uid).First(&t).Error; err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	if err := s.db.Delete(&model.Transaction{}, t.ID).Error; err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "db error")
	}
	return c.NoContent(http.StatusNoContent)
}

// monthlyTrend sums income/expense for the six months ending at the viewed one
// (raw amounts — the household module is single-currency). Feeds the trend chart.
func (s *Server) monthlyTrend(uid uint, viewedStart, viewedEnd time.Time) []monthTrend {
	trendStart := viewedStart.AddDate(0, -5, 0)
	var txs []model.Transaction
	s.db.Where("user_id = ? AND date >= ? AND date < ?", uid, trendStart, viewedEnd).Find(&txs)
	txs = append(txs, s.accountCashRows(uid, trendStart, viewedEnd)...)

	idx := map[string]int{}
	out := make([]monthTrend, 6)
	for i := 0; i < 6; i++ {
		mo := trendStart.AddDate(0, i, 0).Format("2006-01")
		out[i] = monthTrend{Month: mo}
		idx[mo] = i
	}
	for _, t := range txs {
		i, ok := idx[t.Date.Format("2006-01")]
		if !ok {
			continue
		}
		if t.Type == "income" {
			out[i].Income += t.Amount
		} else {
			out[i].Expense += t.Amount
		}
	}
	for i := range out {
		out[i].Income = round2(out[i].Income)
		out[i].Expense = round2(out[i].Expense)
	}
	return out
}

func monthStart(param string) time.Time {
	if param != "" {
		if t, err := time.Parse("2006-01", param); err == nil {
			return t
		}
	}
	now := time.Now()
	return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
}

func sortCategoriesDesc(cs []categorySlice) {
	for i := 1; i < len(cs); i++ {
		for j := i; j > 0 && cs[j].Amount > cs[j-1].Amount; j-- {
			cs[j], cs[j-1] = cs[j-1], cs[j]
		}
	}
}
