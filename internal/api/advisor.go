package api

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"capitalapp/internal/calc"
	"capitalapp/internal/model"

	"github.com/labstack/echo/v4"
)

// The advisor shells out to the locally-installed Claude Code CLI in headless
// mode (`claude -p`). It reuses the user's own logged-in Claude Code — no
// separate API key. Everything runs on the user's machine; only their own
// financial data is sent to Anthropic for the analysis.

const advisorRole = "Ты — независимый сторонний финансовый аудитор личных финансов. " +
	"Анализируй СТРОГО по данным ниже, ничего не выдумывай и не запрашивай. Не используй никакие инструменты — только текстовый разбор. " +
	"Пиши по-русски, кратко и по делу, с конкретными цифрами из данных. Формат — Markdown (заголовки ##, списки -, **жирный**). " +
	"В конце добавь строкой курсивом: _Это информационный разбор, а не лицензированная финансовая консультация._"

// helperRole is the friendly in-app assistant that greets the user and reports
// what changed, spotting inconsistencies across the whole project.
const helperRole = "Ты — дружелюбный помощник по имени Капиталик, который живёт внутри приложения «Мой капитал» и присматривает за порядком в финансах хозяина. " +
	"Пиши от первого лица, тепло и по-человечески, будто здороваешься, когда хозяин зашёл. Не используй инструменты — только текст. " +
	"Опирайся СТРОГО на данные ниже, ничего не выдумывай. Markdown, максимум ~160 слов. " +
	"Структура: короткое приветствие; что изменилось за сегодня; замеченные странности и несоответствия (если их нет — так и скажи, что всё чисто); 1–2 полезных замечания. Без дисклеймеров и воды."

// resolveClaudeBin finds the claude CLI: explicit env override, then PATH, then
// the usual install locations.
func resolveClaudeBin() string {
	if p := os.Getenv("ADVISOR_CLAUDE_BIN"); p != "" {
		return p
	}
	if p, err := exec.LookPath("claude"); err == nil {
		return p
	}
	home, _ := os.UserHomeDir()
	for _, p := range []string{
		home + "/.local/bin/claude", home + "/.claude/local/claude",
		"/opt/homebrew/bin/claude", "/usr/local/bin/claude",
	} {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}

// runAdvisor pipes a prompt to `claude -p` and returns the model's answer.
func (s *Server) runAdvisor(prompt, role string) (string, error) {
	bin := resolveClaudeBin()
	if bin == "" {
		return "", fmt.Errorf("claude-cli-not-found")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, "-p", "--output-format", "text", "--append-system-prompt", role)
	cmd.Stdin = strings.NewReader(prompt)
	// Ensure HOME is set so the CLI finds the user's credentials under ~/.claude.
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if ctx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("timeout")
	}
	if strings.Contains(text, "OAuth") || strings.Contains(text, "authenticate") || strings.Contains(strings.ToLower(text), "not logged in") {
		return "", fmt.Errorf("not-authenticated")
	}
	if err != nil {
		if text == "" {
			text = err.Error()
		}
		return "", fmt.Errorf("cli-error: %s", text)
	}
	if text == "" {
		return "", fmt.Errorf("empty-response")
	}
	return text, nil
}

type advisorResp struct {
	Markdown    string `json:"markdown"`
	GeneratedAt string `json:"generatedAt"`
}

func advisorError(err error) *echo.HTTPError {
	switch err.Error() {
	case "claude-cli-not-found":
		return echo.NewHTTPError(http.StatusServiceUnavailable, "Claude Code не найден. Установи CLI или задай ADVISOR_CLAUDE_BIN.")
	case "not-authenticated":
		return echo.NewHTTPError(http.StatusServiceUnavailable, "Claude Code не авторизован. Открой терминал, выполни «claude» и войди — потом попробуй снова.")
	case "timeout":
		return echo.NewHTTPError(http.StatusGatewayTimeout, "Аудитор не успел ответить. Попробуй ещё раз.")
	default:
		return echo.NewHTTPError(http.StatusBadGateway, "Аудитор недоступен: "+err.Error())
	}
}

// auditCredit builds a factual brief for one debt and asks the advisor to review it.
func (s *Server) auditCredit(c echo.Context) error {
	uid := c.Get(ctxUserID).(uint)
	debt, err := s.findAsset(uid, c.Param("id"))
	if err != nil || debt.Kind != model.KindDebt {
		return echo.NewHTTPError(http.StatusNotFound, "debt not found")
	}
	rates, _ := s.userRates(uid)
	m := calc.Compute(debt, debt.Currency, rates, time.Now())

	var b strings.Builder
	fmt.Fprintf(&b, "Проведи аудит одного кредита. Структура ответа: краткий итог одной строкой; дисциплина платежей; что даст досрочное погашение; риски; 1–3 конкретных совета.\n\n")
	fmt.Fprintf(&b, "КРЕДИТ: %s\n", debt.Name)
	fmt.Fprintf(&b, "Схема: %s\n", schemeHuman(debt.DebtScheme))
	fmt.Fprintf(&b, "Ставка: %g%% годовых\n", debt.RatePercent)
	if m.Loan != nil {
		l := m.Loan
		fmt.Fprintf(&b, "Остаток долга: %s\n", fmtAmount(l.Outstanding, debt.Currency))
		if l.MonthlyPayment > 0 {
			fmt.Fprintf(&b, "Платёж в месяц: %s (из них проценты %s, тело %s)\n",
				fmtAmount(l.MonthlyPayment, debt.Currency), fmtAmount(l.InterestPart, debt.Currency), fmtAmount(l.PrincipalPart, debt.Currency))
		}
		if l.RemainingMonths > 0 {
			fmt.Fprintf(&b, "Осталось платить: %d мес\n", l.RemainingMonths)
		}
		if l.TotalInterest > 0 {
			fmt.Fprintf(&b, "Переплата всего: %s\n", fmtAmount(l.TotalInterest, debt.Currency))
		}
		if debt.RatePercent > 0 {
			fmt.Fprintf(&b, "Проценты капают примерно на %s в день\n", fmtAmount(l.Outstanding*debt.RatePercent/100/365, debt.Currency))
		}
	}

	var entries []model.AccountEntry
	s.db.Where("user_id = ? AND linked_debt_id = ?", uid, debt.ID).Order("date").Find(&entries)
	if len(entries) == 0 {
		b.WriteString("\nИстория платежей: пока нет.\n")
	} else {
		b.WriteString("\nИстория платежей (дата — сумма в валюте долга — заметка):\n")
		for _, e := range entries {
			amt := e.DebtAmount
			if amt == 0 {
				amt = e.Amount
			}
			fmt.Fprintf(&b, "- %s — %s — %s\n", e.Date.Format("2006-01-02"), fmtAmount(amt, debt.Currency), e.Note)
		}
	}

	md, err := s.runAdvisor(b.String(), advisorRole)
	if err != nil {
		return advisorError(err)
	}
	return c.JSON(http.StatusOK, advisorResp{Markdown: md, GeneratedAt: time.Now().Format(time.RFC3339)})
}

// auditPortfolio briefs the advisor on the whole financial picture.
func (s *Server) auditPortfolio(c echo.Context) error {
	uid := c.Get(ctxUserID).(uint)
	base, err := s.userBase(uid)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "user not found")
	}
	rates, _ := s.userRates(uid)
	var assets []model.Asset
	s.db.Where("user_id = ?", uid).Find(&assets)
	now := time.Now()

	var totalAssets, totalLiab, flow float64
	var debts, deposits, accounts, others strings.Builder
	for _, a := range assets {
		m := calc.Compute(a, base, rates, now)
		totalAssets += m.ValueBase
		totalLiab += m.LiabilityBase
		flow += m.MonthlyFlowBase
		switch a.Kind {
		case model.KindDebt:
			l := m.Loan
			line := fmt.Sprintf("- %s: остаток %s", a.Name, fmtAmount(func() float64 { if l != nil { return l.Outstanding }; return a.Value }(), a.Currency))
			if a.RatePercent > 0 {
				line += fmt.Sprintf(", ставка %g%%", a.RatePercent)
			} else {
				line += ", без %"
			}
			if l != nil && l.MonthlyPayment > 0 {
				line += fmt.Sprintf(", платёж %s, осталось %d мес", fmtAmount(l.MonthlyPayment, a.Currency), l.RemainingMonths)
			}
			if l != nil && l.TotalInterest > 0 {
				line += fmt.Sprintf(", переплата %s", fmtAmount(l.TotalInterest, a.Currency))
			}
			debts.WriteString(line + "\n")
		case model.KindDeposit:
			fmt.Fprintf(&deposits, "- %s: %s, ставка %g%%\n", a.Name, fmtAmount(m.AccruedValue, a.Currency), a.RatePercent)
		case model.KindCash:
			fmt.Fprintf(&accounts, "- %s: %s\n", a.Name, fmtAmount(a.Value, a.Currency))
		default:
			fmt.Fprintf(&others, "- %s (%s): %s\n", a.Name, kindLabels[a.Kind], fmtAmount(a.Value, a.Currency))
		}
	}

	var b strings.Builder
	b.WriteString("Проведи аудит всех личных финансов. Структура: общая картина; долги (в каком порядке гасить и почему); ликвидность/подушка; вклады; денежный поток; топ-3 действия на ближайший месяц.\n\n")
	fmt.Fprintf(&b, "Валюта итога: %s\n", base)
	fmt.Fprintf(&b, "Чистый капитал: %s (активы %s, обязательства %s)\n",
		fmtAmount(totalAssets-totalLiab, base), fmtAmount(totalAssets, base), fmtAmount(totalLiab, base))
	fmt.Fprintf(&b, "Денежный поток в месяц (доход − платежи по кредитам): %s\n", fmtAmount(flow, base))
	writeSection(&b, "ДОЛГИ", debts.String())
	writeSection(&b, "ВКЛАДЫ", deposits.String())
	writeSection(&b, "СЧЕТА / НАЛИЧНЫЕ", accounts.String())
	writeSection(&b, "ПРОЧИЕ АКТИВЫ", others.String())

	md, err := s.runAdvisor(b.String(), advisorRole)
	if err != nil {
		return advisorError(err)
	}
	return c.JSON(http.StatusOK, advisorResp{Markdown: md, GeneratedAt: time.Now().Format(time.RFC3339)})
}

func writeSection(b *strings.Builder, title, body string) {
	if strings.TrimSpace(body) == "" {
		return
	}
	fmt.Fprintf(b, "\n%s:\n%s", title, body)
}

func schemeHuman(s string) string {
	switch s {
	case "accruing":
		return "проценты капают на остаток ежедневно, платёж вносится вручную"
	case "annuity":
		return "аннуитет (равный платёж по графику)"
	case "differentiated":
		return "дифференцированный (равное тело, платёж убывает)"
	case "interestfree":
		return "без процентов (рассрочка/частный долг)"
	default:
		return "без процентов"
	}
}

// consistencyFlags runs cheap deterministic checks so the helper reliably
// surfaces real data problems even if the model wouldn't infer them.
func (s *Server) consistencyFlags(uid uint, rates calc.Rates, assets []model.Asset) []string {
	var flags []string
	for _, a := range assets {
		if a.IsAccount && a.Value < -0.01 {
			flags = append(flags, fmt.Sprintf("Счёт «%s» ушёл в минус: %s", a.Name, fmtAmount(a.Value, a.Currency)))
		}
		if a.Kind == model.KindLent && a.Value < -0.01 {
			flags = append(flags, fmt.Sprintf("«%s»: долг тебе стал отрицательным (%s) — вернули больше, чем давали?", a.Name, fmtAmount(a.Value, a.Currency)))
		}
		if a.Currency != "USD" && rates[a.Currency] == 0 {
			flags = append(flags, fmt.Sprintf("Нет курса для %s (актив «%s») — оценка в капитале может быть неверной", a.Currency, a.Name))
		}
	}
	// Salary configured but a payday this month has passed without a posting.
	var u model.User
	if s.db.First(&u, uid).Error == nil && u.MonthlyIncome > 0 && u.SalaryAccountID != nil {
		now := time.Now()
		y, m := now.Year(), int(now.Month())
		adv := prevFriday(time.Date(y, time.Month(m), 15, 12, 0, 0, 0, time.UTC))
		rem := prevFriday(time.Date(y, time.Month(m), daysInMonth(y, m), 12, 0, 0, 0, time.UTC))
		check := func(key, label string, day time.Time) {
			if day.After(now) {
				return
			}
			var n int64
			s.db.Model(&model.AccountEntry{}).Where("user_id = ? AND pay_key = ?", uid, key).Count(&n)
			if n == 0 {
				flags = append(flags, fmt.Sprintf("%s за %02d.%04d ещё не начислен на зарплатный счёт", label, m, y))
			}
		}
		check(fmt.Sprintf("%04d-%02d-adv", y, m), "Аванс", adv)
		check(fmt.Sprintf("%04d-%02d-rem", y, m), "Остаток зарплаты", rem)
	}
	return flags
}

// dailyBrief returns the in-app helper's summary for today, cached once per day.
func (s *Server) dailyBrief(c echo.Context) error {
	uid := c.Get(ctxUserID).(uint)
	s.postSalary(uid) // make sure due salary is reflected before the brief
	base, err := s.userBase(uid)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "user not found")
	}
	today := time.Now().Truncate(24 * time.Hour)
	refresh := c.QueryParam("refresh") == "1"

	if !refresh {
		var cached model.DailyBrief
		if s.db.Where("user_id = ? AND date = ?", uid, today).First(&cached).Error == nil {
			return c.JSON(http.StatusOK, advisorResp{Markdown: cached.Markdown, GeneratedAt: cached.CreatedAt.Format(time.RFC3339)})
		}
	}

	rates, _ := s.userRates(uid)
	var assets []model.Asset
	s.db.Where("user_id = ?", uid).Find(&assets)
	prompt := s.buildBriefPrompt(uid, base, rates, assets)

	md, err := s.runAdvisor(prompt, helperRole)
	if err != nil {
		return advisorError(err)
	}
	s.db.Where(model.DailyBrief{UserID: uid, Date: today}).
		Assign(map[string]any{"markdown": md, "created_at": time.Now()}).
		FirstOrCreate(&model.DailyBrief{})
	return c.JSON(http.StatusOK, advisorResp{Markdown: md, GeneratedAt: time.Now().Format(time.RFC3339)})
}

func (s *Server) buildBriefPrompt(uid uint, base string, rates calc.Rates, assets []model.Asset) string {
	now := time.Now()
	var b strings.Builder
	fmt.Fprintf(&b, "Сегодня %s. Валюта итога: %s.\n", now.Format("2006-01-02"), base)

	// Net worth now + change vs the previous day's snapshot.
	na, nl, _ := s.totalsAsOf(uid, base, rates, now)
	fmt.Fprintf(&b, "Чистый капитал сейчас: %s.\n", fmtAmount(na-nl, base))
	var snaps []model.Snapshot
	s.db.Where("user_id = ?", uid).Order("date desc").Limit(2).Find(&snaps)
	if len(snaps) >= 1 {
		prev := snaps[len(snaps)-1]
		pn, _, _ := snapshotIn(prev, base, rates)
		fmt.Fprintf(&b, "Капитал на %s был %s (изменение с тех пор: %s).\n",
			prev.Date.Format("2006-01-02"), fmtAmount(pn, base), fmtAmount((na-nl)-pn, base))
	}

	// Today's activity log.
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	var acts []model.Activity
	s.db.Where("user_id = ? AND created_at >= ?", uid, start).Order("created_at").Find(&acts)
	if len(acts) == 0 {
		b.WriteString("\nЗа сегодня действий в приложении не было.\n")
	} else {
		b.WriteString("\nЧто произошло сегодня:\n")
		for _, a := range acts {
			line := "- " + a.Title
			if a.Detail != "" {
				line += " (" + a.Detail + ")"
			}
			b.WriteString(line + "\n")
		}
	}

	// Deterministic anomaly flags.
	if flags := s.consistencyFlags(uid, rates, assets); len(flags) > 0 {
		b.WriteString("\nЗамеченные системой флаги (обязательно упомяни, если уместно):\n")
		for _, f := range flags {
			b.WriteString("- " + f + "\n")
		}
	}

	// Compact whole-project state.
	var debts, deposits, accounts, lent strings.Builder
	for _, a := range assets {
		m := calc.Compute(a, base, rates, now)
		switch a.Kind {
		case model.KindDebt:
			out := a.Value
			if m.Loan != nil {
				out = m.Loan.Outstanding
			}
			fmt.Fprintf(&debts, "- %s: остаток %s, %s", a.Name, fmtAmount(out, a.Currency), schemeHuman(a.DebtScheme))
			var pays []model.AccountEntry
			s.db.Where("user_id = ? AND linked_debt_id = ?", uid, a.ID).Order("date desc").Limit(3).Find(&pays)
			if len(pays) > 0 {
				debts.WriteString("; недавние платежи:")
				for _, p := range pays {
					amt := p.DebtAmount
					if amt == 0 {
						amt = p.Amount
					}
					fmt.Fprintf(&debts, " %s(%s)", fmtAmount(amt, a.Currency), p.Date.Format("02.01"))
				}
			}
			debts.WriteString("\n")
		case model.KindDeposit:
			fmt.Fprintf(&deposits, "- %s: %s, ставка %g%%\n", a.Name, fmtAmount(m.AccruedValue, a.Currency), a.RatePercent)
		case model.KindCash:
			fmt.Fprintf(&accounts, "- %s: %s\n", a.Name, fmtAmount(a.Value, a.Currency))
		case model.KindLent:
			fmt.Fprintf(&lent, "- %s: ещё вернут %s\n", a.Name, fmtAmount(a.Value, a.Currency))
		}
	}
	writeSection(&b, "ДОЛГИ", debts.String())
	writeSection(&b, "ВКЛАДЫ", deposits.String())
	writeSection(&b, "СЧЕТА", accounts.String())
	writeSection(&b, "ДОЛГ МНЕ", lent.String())
	return b.String()
}
