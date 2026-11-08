package report

import (
	"fmt"
	"html"
	"strconv"
	"strings"
)

// RenderText renders r as the plain-text mail body: a ~60-column layout
// with a totals block, the per-category table, the top-merchants table,
// and — when r.Prior is set — a vs-prior-period delta block. It returns
// an error when a money field is not canonical 2-decimal (see
// checkReport); Generate's output always renders.
func RenderText(r Report) (string, error) {
	if err := checkReport(r); err != nil {
		return "", err
	}
	const title = "Personal Finance Report"
	var b strings.Builder
	b.WriteString(title + "\n")
	b.WriteString(strings.Repeat("=", len(title)) + "\n")
	b.WriteString(r.Label + "\n")
	b.WriteString(fmt.Sprintf("from %s to %s\n", dateStr(r.From), dateStr(r.To)))

	b.WriteString("\nTotals\n")
	b.WriteString(fmt.Sprintf("  %-8s%s\n", "Income", plusMoney(r.Income)))
	b.WriteString(fmt.Sprintf("  %-8s%s\n", "Spend", negMoney(r.Spend)))
	b.WriteString(fmt.Sprintf("  %-8s%s\n", "Net", signedMoney(r.Net)))

	b.WriteString("\nBy category\n")
	if len(r.ByCategory) == 0 {
		b.WriteString("(none)\n")
	} else {
		rows := make([][]string, 0, len(r.ByCategory))
		for _, l := range r.ByCategory {
			rows = append(rows, []string{
				displayName(l),
				l.Spend,
				l.Income,
				signedMoney(l.Net),
				budgetCell(l),
				budgetStatus(l),
			})
		}
		b.WriteString(textTable(
			[]string{"Category", "Spend", "Income", "Net", "Budget", "Status"},
			rows, 1,
		))
	}

	b.WriteString("\nTop merchants\n")
	if len(r.TopMerchants) == 0 {
		b.WriteString("(none)\n")
	} else {
		rows := make([][]string, 0, len(r.TopMerchants))
		for _, m := range r.TopMerchants {
			rows = append(rows, []string{m.Name, strconv.Itoa(m.Count), m.Total})
		}
		b.WriteString(textTable([]string{"Merchant", "Count", "Total"}, rows, 1))
	}

	if p := r.Prior; p != nil {
		b.WriteString(fmt.Sprintf("\nvs %s\n", p.Label))
		for _, row := range priorRows(r) {
			word, delta := signedDelta(row.cur, row.prior)
			b.WriteString(fmt.Sprintf("  %-10s%-6s%s\n", row.name, word, delta))
		}
	}

	return b.String(), nil
}

// RenderHTML renders r as a single self-contained HTML document: inline
// CSS only (no <style> elements, no external assets), max-width 560px,
// a system font stack, no images. It is shaped to be the html part of
// the multipart/alternative mail step 8.3 sends through the 8.1 mailer.
// User-derived strings (category names, merchant names) go through
// html.EscapeString. It errors on a non-canonical money field (see
// checkReport); Generate's output always renders.
func RenderHTML(r Report) (string, error) {
	if err := checkReport(r); err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("<!DOCTYPE html>\n<html>\n<head>\n")
	b.WriteString("<meta charset=\"utf-8\">\n")
	b.WriteString("<meta name=\"viewport\" content=\"width=device-width, initial-scale=1\">\n")
	b.WriteString(fmt.Sprintf("<title>Personal Finance Report - %s</title>\n", r.Label))
	b.WriteString("</head>\n")
	b.WriteString("<body style=\"margin:0;padding:24px 12px;background:#f6f8fa;\">\n")
	b.WriteString("<div style=\"max-width:560px;margin:0 auto;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,Helvetica,Arial,sans-serif;font-size:14px;line-height:1.5;color:#24292f;\">\n")

	b.WriteString("<h1 style=\"margin:0 0 4px;font-size:20px;font-weight:600;\">Personal Finance Report</h1>\n")
	b.WriteString(fmt.Sprintf("<p style=\"margin:0 0 16px;color:#57606a;\">%s, from %s to %s</p>\n",
		r.Label, dateStr(r.From), dateStr(r.To)))

	// Totals as three side-by-side boxes: income green, spend red,
	// net by sign.
	b.WriteString("<table role=\"presentation\" style=\"width:100%;border-collapse:collapse;\">\n<tr>\n")
	totalsBox(&b, "Income", plusMoney(r.Income), "#1a7f37", true)
	totalsBox(&b, "Spend", negMoney(r.Spend), "#cf222e", true)
	netColor := "#1a7f37"
	if centsOf(r.Net) < 0 {
		netColor = "#cf222e"
	}
	totalsBox(&b, "Net", signedMoney(r.Net), netColor, false)
	b.WriteString("</tr>\n</table>\n")

	heading(&b, "By category")
	if len(r.ByCategory) == 0 {
		noneLine(&b)
	} else {
		b.WriteString("<table style=\"width:100%;border-collapse:collapse;font-size:13px;\">\n")
		b.WriteString("<tr>")
		thL(&b, "Category")
		thR(&b, "Spend")
		thR(&b, "Income")
		thR(&b, "Net")
		thR(&b, "Budget")
		thR(&b, "Status")
		b.WriteString("</tr>\n")
		for _, l := range r.ByCategory {
			b.WriteString("<tr>")
			tdL(&b, html.EscapeString(displayName(l)))
			tdR(&b, l.Spend)
			tdR(&b, l.Income)
			tdR(&b, signedMoney(l.Net))
			tdR(&b, budgetCell(l))
			tdL(&b, statusHTML(l))
			b.WriteString("</tr>\n")
		}
		b.WriteString("</table>\n")
	}

	heading(&b, "Top merchants")
	if len(r.TopMerchants) == 0 {
		noneLine(&b)
	} else {
		b.WriteString("<table style=\"width:100%;border-collapse:collapse;font-size:13px;\">\n")
		b.WriteString("<tr>")
		thL(&b, "Merchant")
		thR(&b, "Count")
		thR(&b, "Total")
		b.WriteString("</tr>\n")
		for _, m := range r.TopMerchants {
			b.WriteString("<tr>")
			tdL(&b, html.EscapeString(m.Name))
			tdR(&b, strconv.Itoa(m.Count))
			tdR(&b, m.Total)
			b.WriteString("</tr>\n")
		}
		b.WriteString("</table>\n")
	}

	if p := r.Prior; p != nil {
		b.WriteString("<div style=\"margin-top:20px;background:#ffffff;border:1px solid #d0d7de;border-radius:6px;padding:12px 14px;\">\n")
		b.WriteString(fmt.Sprintf("<div style=\"font-size:13px;font-weight:600;margin-bottom:8px;\">vs %s</div>\n", p.Label))
		b.WriteString("<table style=\"width:100%;border-collapse:collapse;font-size:13px;\">\n")
		for _, row := range priorRows(r) {
			word, delta := signedDelta(row.cur, row.prior)
			b.WriteString("<tr>")
			b.WriteString("<td style=\"padding:2px 0;\">")
			b.WriteString(row.name)
			b.WriteString("</td>\n")
			b.WriteString(fmt.Sprintf("<td style=\"padding:2px 0;width:48px;color:#57606a;\">%s</td>\n", word))
			b.WriteString(fmt.Sprintf("<td style=\"padding:2px 0;text-align:right;font-weight:600;\">%s</td>\n", delta))
			b.WriteString("</tr>\n")
		}
		b.WriteString("</table>\n</div>\n")
	}

	b.WriteString("<p style=\"margin:24px 0 0;text-align:center;font-size:12px;color:#57606a;\">Personal Finance Tracker</p>\n")
	b.WriteString("</div>\n</body>\n</html>\n")
	return b.String(), nil
}

// checkReport validates every money field of r before rendering: the
// renderers compute deltas through centsOf (panic-based), so a
// malformed field would crash the mail pipeline; this turns that into
// the documented error return instead. Generate's output always passes.
func checkReport(r Report) error {
	top := []struct {
		name string
		s    string
	}{
		{"Income", r.Income},
		{"Spend", r.Spend},
		{"Net", r.Net},
	}
	for _, f := range top {
		if err := checkMoney(f.name, f.s); err != nil {
			return err
		}
	}
	for i, l := range r.ByCategory {
		prefix := fmt.Sprintf("ByCategory[%d]", i)
		for _, f := range []struct {
			name string
			s    string
		}{
			{"Spend", l.Spend},
			{"Income", l.Income},
			{"Net", l.Net},
		} {
			if err := checkMoney(prefix+"."+f.name, f.s); err != nil {
				return err
			}
		}
		if l.BudgetTarget != "" {
			if err := checkMoney(prefix+".BudgetTarget", l.BudgetTarget); err != nil {
				return err
			}
		}
	}
	for i, m := range r.TopMerchants {
		if err := checkMoney(fmt.Sprintf("TopMerchants[%d].Total", i), m.Total); err != nil {
			return err
		}
	}
	if r.Prior != nil {
		for _, f := range []struct {
			name string
			s    string
		}{
			{"Income", r.Prior.Income},
			{"Spend", r.Prior.Spend},
			{"Net", r.Prior.Net},
		} {
			if err := checkMoney("Prior."+f.name, f.s); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkMoney reports a "report: ..." error when s is not a valid
// 2-decimal money string for the named field.
func checkMoney(field, s string) error {
	_, err := parse2dec(s)
	if err != nil {
		return fmt.Errorf("report: %s %q: %s", field, s, strings.TrimPrefix(err.Error(), "report: "))
	}
	return nil
}

// textTable renders a table with 2-space column gaps; columns at or
// after rightFrom are right-aligned, earlier ones left-aligned. Each
// column's width is the max of its header and cell lengths; the row
// under the header is dashes.
func textTable(headers []string, rows [][]string, rightFrom int) string {
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = len(h)
	}
	for _, row := range rows {
		for i, cell := range row {
			if len(cell) > widths[i] {
				widths[i] = len(cell)
			}
		}
	}
	var b strings.Builder
	writeRow := func(cells []string) {
		var row strings.Builder
		for i, c := range cells {
			if i > 0 {
				row.WriteString("  ")
			}
			if i >= rightFrom {
				row.WriteString(c)
				row.WriteString(strings.Repeat(" ", widths[i]-len(c)))
			} else {
				row.WriteString(strings.Repeat(" ", widths[i]-len(c)))
				row.WriteString(c)
			}
		}
		// Trim trailing padding so the mail body carries no ragged
		// end-of-line spaces.
		b.WriteString(strings.TrimRight(row.String(), " "))
		b.WriteString("\n")
	}
	writeRow(headers)
	sep := make([]string, len(headers))
	for i := range sep {
		sep[i] = strings.Repeat("-", widths[i])
	}
	writeRow(sep)
	for _, row := range rows {
		writeRow(row)
	}
	return b.String()
}

// displayName is the user-facing category name: the uncategorized group
// (CategoryID "") renders as "Uncategorized"; a known id with no name
// renders as "Unknown". (Generate keeps the raw names; this is the
// render layer's display rule.)
func displayName(l CategoryLine) string {
	if l.CategoryID == "" {
		return "Uncategorized"
	}
	if l.Name == "" {
		return "Unknown"
	}
	return l.Name
}

// budgetCell is the Budget column value: the target or "-" when the
// category has no target.
func budgetCell(l CategoryLine) string {
	if l.BudgetTarget == "" {
		return "-"
	}
	return l.BudgetTarget
}

// budgetStatus is the Status column value: "-" with no target,
// "within budget" at or under the target, "over by X" above it (X the
// canonical overage, Spend - BudgetTarget).
func budgetStatus(l CategoryLine) string {
	if l.WithinBudget == nil {
		return "-"
	}
	if *l.WithinBudget {
		return "within budget"
	}
	return "over by " + fmt2dec(centsOf(l.Spend)-centsOf(l.BudgetTarget))
}

// statusHTML is the per-category over/under marker for the HTML table:
// red bold "over by X", green "within budget", neutral "-" otherwise.
func statusHTML(l CategoryLine) string {
	if l.WithinBudget == nil {
		return "<span style=\"color:#57606a;\">-</span>"
	}
	if *l.WithinBudget {
		return "<span style=\"color:#1a7f37;\">within budget</span>"
	}
	return "<span style=\"color:#cf222e;font-weight:600;\">over by " +
		fmt2dec(centsOf(l.Spend)-centsOf(l.BudgetTarget)) + "</span>"
}

// priorRows is the (name, current, prior) money rows of the
// vs-prior-period block, in the rendering order.
func priorRows(r Report) []struct {
	name  string
	cur   string
	prior string
} {
	return []struct {
		name  string
		cur   string
		prior string
	}{
		{"Income", r.Income, r.Prior.Income},
		{"Spend", r.Spend, r.Prior.Spend},
		{"Net", r.Net, r.Prior.Net},
	}
}

// plusMoney renders a non-negative magnitude with a "+" prefix; zero
// renders as plain "0.00" (no sign).
func plusMoney(s string) string {
	if centsOf(s) == 0 {
		return "0.00"
	}
	return "+" + s
}

// negMoney renders a non-negative magnitude as a negative amount
// (spend's display convention); zero renders as plain "0.00".
func negMoney(s string) string {
	if centsOf(s) == 0 {
		return "0.00"
	}
	return "-" + s
}

// signedMoney renders a possibly-negative amount with an explicit sign;
// zero renders as plain "0.00".
func signedMoney(s string) string {
	switch c := centsOf(s); {
	case c > 0:
		return "+" + s
	case c < 0:
		return s // parse2dec keeps the sign, fmt2dec renders "-"
	default:
		return "0.00"
	}
}

// signedDelta compares current and prior (both magnitudes or signed
// nets, per the row) and returns the direction word ("up", "down",
// "flat") plus the canonical signed delta current - prior.
func signedDelta(cur, prior string) (word, delta string) {
	switch d := centsOf(cur) - centsOf(prior); {
	case d > 0:
		return "up", "+" + fmt2dec(d)
	case d < 0:
		return "down", fmt2dec(d)
	default:
		return "flat", "0.00"
	}
}

func totalsBox(b *strings.Builder, label, value, color string, gap bool) {
	style := "width:33%;padding:0 8px 0 0"
	if !gap {
		style = "width:33%;padding:0"
	}
	b.WriteString("<td style=\"" + style + "\">\n")
	b.WriteString("<div style=\"background:#ffffff;border:1px solid #d0d7de;border-radius:6px;padding:10px 12px;\">\n")
	b.WriteString("<div style=\"font-size:12px;color:#57606a;\">")
	b.WriteString(label)
	b.WriteString("</div>\n")
	b.WriteString("<div style=\"font-size:16px;font-weight:600;color:" + color + ";\">")
	b.WriteString(value)
	b.WriteString("</div>\n</div></td>\n")
}

func heading(b *strings.Builder, s string) {
	b.WriteString("<h2 style=\"margin:20px 0 8px;font-size:15px;font-weight:600;border-bottom:1px solid #d0d7de;padding-bottom:4px;\">")
	b.WriteString(s)
	b.WriteString("</h2>\n")
}

func noneLine(b *strings.Builder) {
	b.WriteString("<p style=\"margin:8px 0;color:#57606a;\">(none)</p>\n")
}

func thL(b *strings.Builder, s string) {
	b.WriteString("<th style=\"border-bottom:1px solid #d0d7de;padding:6px 4px;text-align:left;font-weight:600;\">")
	b.WriteString(s)
	b.WriteString("</th>")
}

func thR(b *strings.Builder, s string) {
	b.WriteString("<th style=\"border-bottom:1px solid #d0d7de;padding:6px 4px;text-align:right;font-weight:600;\">")
	b.WriteString(s)
	b.WriteString("</th>")
}

func tdL(b *strings.Builder, s string) {
	b.WriteString("<td style=\"border-bottom:1px solid #eaeef2;padding:6px 4px;text-align:left;\">")
	b.WriteString(s)
	b.WriteString("</td>")
}

func tdR(b *strings.Builder, s string) {
	b.WriteString("<td style=\"border-bottom:1px solid #eaeef2;padding:6px 4px;text-align:right;\">")
	b.WriteString(s)
	b.WriteString("</td>")
}
