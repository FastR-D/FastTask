package domain

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

const (
	LensResearchRisk = "research_risk"
	CoordThreshold   = 50
	CoordMin         = 0
	CoordMax         = 100
)

const (
	QuadrantCriticalRisk = "A"
	QuadrantMainProgress = "B"
	QuadrantTimeHole     = "C"
	QuadrantDrain        = "D"
)

const (
	CoordSourceAgent   = "agent"
	CoordSourceUser    = "user"
	CoordSourceDefault = "default"
)

const StalledProgressDays = 14

var (
	ErrCoord      = errors.New("coordinate out of range")
	ErrLens       = errors.New("unknown lens")
	ErrValidation = errors.New("validation failed")
)

var quadrantLabels = map[string]string{
	QuadrantCriticalRisk: "关键风险区",
	QuadrantMainProgress: "主推进区",
	QuadrantTimeHole:     "时间黑洞",
	QuadrantDrain:        "消耗区",
}

// ValidLens 目前只认 research_risk；新增透镜时在此扩展。
func ValidLens(lens string) bool { return lens == LensResearchRisk }

// ValidateCoord 校验 x、y 落在 [0,100]，越界返回 ErrCoord。
func ValidateCoord(x, y int) error {
	if x < CoordMin || x > CoordMax || y < CoordMin || y > CoordMax {
		return ErrCoord
	}
	return nil
}

// Quadrant 返回 "A" / "B" / "C" / "D"。
// x 是不确定性，y 是贡献度，边界值 50 归入高侧（>= 50 为高）。
func Quadrant(x, y int) string {
	highX, highY := x >= CoordThreshold, y >= CoordThreshold
	switch {
	case highX && highY:
		return QuadrantCriticalRisk
	case !highX && highY:
		return QuadrantMainProgress
	case highX && !highY:
		return QuadrantTimeHole
	default:
		return QuadrantDrain
	}
}

// QuadrantLabel 返回中文标签，供后端组装响应，前端不硬编码。
func QuadrantLabel(quadrant string) string { return quadrantLabels[quadrant] }

// Quadrants 返回稳定的四区顺序，响应恒定输出 A、B、C、D 四项。
func Quadrants() []string {
	return []string{QuadrantCriticalRisk, QuadrantMainProgress, QuadrantTimeHole, QuadrantDrain}
}

// CurrentISOWeek 返回 t 在 loc 下的 ISO 周标识，格式 "2006-W01"。
func CurrentISOWeek(t time.Time, loc *time.Location) string {
	if loc == nil {
		loc = time.UTC
	}
	year, week := t.In(loc).ISOWeek()
	return fmt.Sprintf("%04d-W%02d", year, week)
}

// ISOWeekRange 把 "2026-W37" 解析成该 ISO 周在 loc 时区下的半开区间。
// startUTC 是周一 00:00 本地时间对应的 UTC 瞬时，endUTC 是下周一 00:00 对应的 UTC 瞬时（不含）。
// startDate / endDate 是周一与周日的本地日期字符串。
func ISOWeekRange(week string, loc *time.Location) (startUTC, endUTC time.Time, startDate, endDate string, err error) {
	if loc == nil {
		loc = time.UTC
	}
	trimmed := strings.TrimSpace(week)
	yearText, weekText, found := strings.Cut(trimmed, "-W")
	if !found {
		return time.Time{}, time.Time{}, "", "", ErrValidation
	}
	year, parseErr := strconv.Atoi(yearText)
	number, numberErr := strconv.Atoi(weekText)
	if parseErr != nil || numberErr != nil || year < 1 || number < 1 || number > 53 {
		return time.Time{}, time.Time{}, "", "", ErrValidation
	}
	januaryFourth := time.Date(year, time.January, 4, 0, 0, 0, 0, loc)
	weekday := int(januaryFourth.Weekday())
	if weekday == 0 {
		weekday = 7
	}
	monday := januaryFourth.AddDate(0, 0, -(weekday-1)).AddDate(0, 0, (number-1)*7)
	if isoYear, isoWeek := monday.ISOWeek(); isoYear != year || isoWeek != number {
		return time.Time{}, time.Time{}, "", "", ErrValidation
	}
	sunday := monday.AddDate(0, 0, 6)
	return monday.UTC(), monday.AddDate(0, 0, 7).UTC(), monday.Format("2006-01-02"), sunday.Format("2006-01-02"), nil
}

// LocalDaysBetween 按本地日期相减计算天数，不按 24 小时整除。
func LocalDaysBetween(from, to time.Time, loc *time.Location) int {
	if loc == nil {
		loc = time.UTC
	}
	if from.IsZero() {
		return 0
	}
	start := time.Date(from.In(loc).Year(), from.In(loc).Month(), from.In(loc).Day(), 0, 0, 0, 0, loc)
	end := time.Date(to.In(loc).Year(), to.In(loc).Month(), to.In(loc).Day(), 0, 0, 0, 0, loc)
	days := int(math.Round(end.Sub(start).Hours() / 24))
	if days < 0 {
		return 0
	}
	return days
}

type StalledTask struct {
	TaskID            string `json:"task_id"`
	Title             string `json:"title"`
	GoalID            string `json:"goal_id"`
	Quadrant          string `json:"quadrant"`
	DaysSinceProgress int    `json:"days_since_progress"`
}

// WeeklySummaryInput 是已经算好的确定性指标，总结句只能来自这里，不得编造。
type WeeklySummaryInput struct {
	TotalMinutes       int
	QuadrantMinutes    map[string]int
	QuadrantDelta      map[string]int
	EvidenceTotal      int
	MinimumActionCount int
	MinimumActionShare float64
	Stalled            []StalledTask
}

// WeeklySummary 按固定优先级返回 (rule, text)，命中第一条即停止。
func WeeklySummary(input WeeklySummaryInput) (string, string) {
	minutes := func(quadrant string) int { return input.QuadrantMinutes[quadrant] }
	if input.TotalMinutes == 0 && len(input.Stalled) == 0 {
		return "empty", "本周没有记录到有效专注时间。"
	}
	if len(input.Stalled) > 0 {
		stalled := input.Stalled[0]
		return "stalled_risk", fmt.Sprintf("你标为关键风险的「%s」已 %d 天没有实质推进。", stalled.Title, stalled.DaysSinceProgress)
	}
	if input.TotalMinutes > 0 && float64(minutes(QuadrantDrain))/float64(input.TotalMinutes) > 0.5 {
		share := float64(minutes(QuadrantDrain)) / float64(input.TotalMinutes)
		return "drain_dominant", fmt.Sprintf("本周 %d%% 的专注时间落在低不确定、低贡献的工作上。", int(math.Round(share*100)))
	}
	if input.MinimumActionShare > 0.5 {
		return "minimum_action_heavy", fmt.Sprintf("本周 %d 个核心项中有 %d 个只完成了最小行动。", input.EvidenceTotal, input.MinimumActionCount)
	}
	if delta := input.QuadrantDelta[QuadrantCriticalRisk]; delta > 0 {
		return "risk_improving", fmt.Sprintf("本周在关键风险区投入 %d 分钟，比上周多 %d 分钟。", minutes(QuadrantCriticalRisk), delta)
	}
	return "neutral", fmt.Sprintf("本周有效专注 %d 分钟，四区分布：关键风险 %d、主推进 %d、时间黑洞 %d、消耗 %d 分钟。",
		input.TotalMinutes, minutes(QuadrantCriticalRisk), minutes(QuadrantMainProgress), minutes(QuadrantTimeHole), minutes(QuadrantDrain))
}
