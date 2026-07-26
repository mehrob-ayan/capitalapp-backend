package api

import (
	"net/http"
	"time"

	"capitalapp/internal/model"

	"github.com/labstack/echo/v4"
)

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
