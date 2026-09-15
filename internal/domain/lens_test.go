package domain

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestValidateCoordRange(t *testing.T) {
	for _, pair := range [][2]int{{0, 0}, {100, 100}, {50, 49}, {0, 100}} {
		if err := ValidateCoord(pair[0], pair[1]); err != nil {
			t.Errorf("(%d,%d) rejected: %v", pair[0], pair[1], err)
		}
	}
	for _, pair := range [][2]int{{-1, 50}, {50, -1}, {101, 50}, {50, 101}, {101, 101}} {
		if !errors.Is(ValidateCoord(pair[0], pair[1]), ErrCoord) {
			t.Errorf("(%d,%d) should be rejected with ErrCoord", pair[0], pair[1])
		}
	}
}

func TestQuadrantBoundary(t *testing.T) {
	cases := []struct {
		x, y int
		want string
	}{
		{50, 50, "A"},
		{49, 50, "B"},
		{50, 49, "C"},
		{49, 49, "D"},
		{100, 100, "A"},
		{0, 100, "B"},
		{100, 0, "C"},
		{0, 0, "D"},
	}
	for _, testCase := range cases {
		if got := Quadrant(testCase.x, testCase.y); got != testCase.want {
			t.Errorf("Quadrant(%d,%d)=%s want %s", testCase.x, testCase.y, got, testCase.want)
		}
	}
}

func TestValidLens(t *testing.T) {
	if !ValidLens(LensResearchRisk) {
		t.Fatal("research_risk rejected")
	}
	for _, lens := range []string{"", "importance_urgency", "RESEARCH_RISK", "custom"} {
		if ValidLens(lens) {
			t.Errorf("lens %q accepted", lens)
		}
	}
}

func TestQuadrantLabels(t *testing.T) {
	want := map[string]string{"A": "关键风险区", "B": "主推进区", "C": "时间黑洞", "D": "消耗区"}
	for key, label := range want {
		if got := QuadrantLabel(key); got != label {
			t.Errorf("QuadrantLabel(%s)=%q want %q", key, got, label)
		}
	}
	if QuadrantLabel("E") != "" {
		t.Fatal("unknown quadrant returned a label")
	}
}

func TestISOWeekRangeAcrossYearBoundary(t *testing.T) {
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	start, end, startDate, endDate, err := ISOWeekRange("2026-W01", location)
	if err != nil {
		t.Fatal(err)
	}
	if startDate != "2025-12-29" || endDate != "2026-01-04" {
		t.Fatalf("2026-W01 dates = %s..%s", startDate, endDate)
	}
	if start.Year() != 2025 || start.Month() != time.December {
		t.Fatalf("2026-W01 monday = %s", start.Format(time.RFC3339))
	}
	if !end.Equal(start.AddDate(0, 0, 7)) {
		t.Fatalf("window is not seven days: %s .. %s", start, end)
	}
	local := start.In(location)
	if local.Hour() != 0 || local.Minute() != 0 || local.Weekday() != time.Monday {
		t.Fatalf("window does not start at local monday midnight: %s", local)
	}
	if got := CurrentISOWeek(start, location); got != "2026-W01" {
		t.Fatalf("CurrentISOWeek = %s", got)
	}
	if got := CurrentISOWeek(time.Date(2026, 9, 9, 12, 0, 0, 0, location), location); got != "2026-W37" {
		t.Fatalf("CurrentISOWeek = %s", got)
	}
	regular, _, regularStart, _, err := ISOWeekRange("2026-W37", location)
	if err != nil {
		t.Fatal(err)
	}
	if regularStart != "2026-09-07" || regular.In(location).Weekday() != time.Monday {
		t.Fatalf("2026-W37 start = %s (%s)", regularStart, regular)
	}
}

func TestISOWeekRangeRejectsBadInput(t *testing.T) {
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	for _, week := range []string{"", "2026-W99", "2026-W00", "2026W37", "abc-W01", "2026-W3x", "2025-W53", "W37", "2026-"} {
		if _, _, _, _, err := ISOWeekRange(week, location); !errors.Is(err, ErrValidation) {
			t.Errorf("week %q error = %v, want ErrValidation", week, err)
		}
	}
	for _, week := range []string{"2026-W52", "2026-W53", "2020-W53"} {
		if _, _, _, _, err := ISOWeekRange(week, location); err != nil {
			t.Errorf("week %q rejected: %v", week, err)
		}
	}
}

func TestSummaryRulePriority(t *testing.T) {
	input := WeeklySummaryInput{
		TotalMinutes:       600,
		QuadrantMinutes:    map[string]int{"A": 30, "B": 60, "C": 60, "D": 450},
		QuadrantDelta:      map[string]int{"A": 20, "B": 0, "C": 0, "D": 100},
		EvidenceTotal:      10,
		MinimumActionCount: 9,
		MinimumActionShare: 0.9,
		Stalled:            []StalledTask{{TaskID: "task_1", Title: "跑通基线实验", GoalID: "goal_1", Quadrant: "A", DaysSinceProgress: 19}},
	}
	rule, text := WeeklySummary(input)
	if rule != "stalled_risk" {
		t.Fatalf("rule = %s, want stalled_risk", rule)
	}
	if !strings.Contains(text, "跑通基线实验") || !strings.Contains(text, "19") {
		t.Fatalf("text = %q", text)
	}

	input.Stalled = nil
	rule, text = WeeklySummary(input)
	if rule != "drain_dominant" {
		t.Fatalf("rule = %s, want drain_dominant", rule)
	}
	if !strings.Contains(text, "75%") {
		t.Fatalf("text = %q", text)
	}

	input.QuadrantMinutes["D"] = 100
	input.TotalMinutes = 250
	rule, _ = WeeklySummary(input)
	if rule != "minimum_action_heavy" {
		t.Fatalf("rule = %s, want minimum_action_heavy", rule)
	}

	input.MinimumActionShare = 0.2
	rule, text = WeeklySummary(input)
	if rule != "risk_improving" {
		t.Fatalf("rule = %s, want risk_improving", rule)
	}
	if !strings.Contains(text, "20") {
		t.Fatalf("text = %q", text)
	}

	input.QuadrantDelta["A"] = -5
	rule, _ = WeeklySummary(input)
	if rule != "neutral" {
		t.Fatalf("rule = %s, want neutral", rule)
	}
}

func TestSummaryEmptyWeek(t *testing.T) {
	rule, text := WeeklySummary(WeeklySummaryInput{
		QuadrantMinutes: map[string]int{"A": 0, "B": 0, "C": 0, "D": 0},
		QuadrantDelta:   map[string]int{},
	})
	if rule != "empty" {
		t.Fatalf("rule = %s, want empty", rule)
	}
	if text != "本周没有记录到有效专注时间。" {
		t.Fatalf("text = %q", text)
	}
}

func TestLocalDaysBetweenUsesCalendarDates(t *testing.T) {
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	from := time.Date(2026, 9, 1, 23, 30, 0, 0, location)
	to := time.Date(2026, 9, 2, 0, 30, 0, 0, location)
	if got := LocalDaysBetween(from, to, location); got != 1 {
		t.Fatalf("LocalDaysBetween = %d, want 1", got)
	}
	if got := LocalDaysBetween(from, from.Add(29*time.Minute), location); got != 0 {
		t.Fatalf("same local date = %d, want 0", got)
	}
	if got := LocalDaysBetween(to, from, location); got != 0 {
		t.Fatalf("negative range = %d, want 0", got)
	}
	if got := LocalDaysBetween(time.Time{}, to, location); got != 0 {
		t.Fatalf("zero reference = %d, want 0", got)
	}
}
