package integrationclient

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Outcome is a source record, not a declaration that a FastTask task is done.
// Attribution deliberately describes the source scope when its API lacks authors.
type Outcome struct {
	ID              string `json:"id"`
	Source          string `json:"source"`
	ResourceID      string `json:"resource_id"`
	Kind            string `json:"kind"`
	Title           string `json:"title"`
	Summary         string `json:"summary"`
	OccurredAt      string `json:"occurred_at,omitempty"`
	SourceUpdatedAt string `json:"source_updated_at,omitempty"`
	Status          string `json:"status"`
	URL             string `json:"url"`
	Version         string `json:"version"`
	Attribution     string `json:"attribution"`
}

type OutcomePage struct {
	System     string      `json:"system"`
	Status     string      `json:"status"`
	Items      []Outcome   `json:"items"`
	HasMore    bool        `json:"has_more"`
	NextOffset int         `json:"next_offset"`
	SyncedAt   string      `json:"synced_at"`
	Warnings   []string    `json:"warnings"`
	Diagnostic *Diagnostic `json:"diagnostic,omitempty"`
}

func OutcomeUnavailable(system string, problem *Diagnostic) OutcomePage {
	status := "unavailable"
	if problem.Code == "CONFIG_MISSING" || problem.Code == "AUTH_REQUIRED" {
		status = "not_configured"
	}
	if problem.Code == "API_NOT_ADAPTED" {
		status = "unsupported"
	}
	return OutcomePage{System: system, Status: status, Items: []Outcome{}, Warnings: []string{}, Diagnostic: problem}
}

// Outcomes pages source resources, then collects a bounded set of their saved
// records. It never runs a model, generates a report, or performs a source write.
// Missing dates are retained for all-time browsing only; synchronization time
// must never turn an old document into this week's accomplishment.
func (c Client) Outcomes(ctx context.Context, config Config, start, end *time.Time, limit, offset int) OutcomePage {
	p, problem := prepare(config)
	if problem != nil {
		return OutcomeUnavailable(config.System, problem)
	}
	if limit < 1 {
		limit = 5
	}
	if limit > 10 {
		limit = 10
	}
	if offset < 0 || offset > 1000000 {
		return OutcomeUnavailable(config.System, badConfig("分页范围无效。"))
	}
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	if problem = c.checkWritingIdentity(ctx, p); problem != nil {
		return OutcomeUnavailable(config.System, problem)
	}
	resources, problem := c.list(ctx, p, "", limit, offset)
	if problem != nil {
		return OutcomeUnavailable(config.System, problem)
	}
	result := OutcomePage{System: config.System, Status: "ready", Items: []Outcome{}, Warnings: []string{}, HasMore: resources.HasMore, NextOffset: offset + len(resources.Items), SyncedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	if resources.Warning != nil {
		result.Warnings = append(result.Warnings, resources.Warning.Message)
	}
	if p.profile == "fastread-web" {
		result.Warnings = append(result.Warnings, "按论文读取已保存笔记、当前文档版本批注和阅读报告；不包含知识库笔记。工作区记录不代表全部由本人创作。")
	}
	if p.profile == "fastwrite" {
		result.Warnings = append(result.Warnings, "写作版本与审阅报告属于有权访问的项目记录，不代表个人独立贡献或论文最终完成。")
	}
	seen := map[string]bool{}
	unknown := false
	for i, resource := range resources.Items {
		if ctx.Err() != nil {
			result.HasMore = true
			result.NextOffset = offset + i
			result.Warnings = append(result.Warnings, "读取超时，本批尚有资源未核对，请继续读取。")
			break
		}
		var items []Outcome
		var warnings []string
		switch p.profile {
		case "fastread-web":
			items, warnings, problem = c.paperOutcomes(ctx, p, resource)
		case "fastwrite":
			items, warnings, problem = c.writingOutcomes(ctx, p, resource)
		default:
			items, warnings, problem = c.materialOutcomes(ctx, p, resource)
		}
		if problem != nil {
			// Authentication loss invalidates this response as a whole, even if an
			// earlier request succeeded before the source revoked authorization.
			if problem.HTTPStatus == 401 || problem.HTTPStatus == 403 {
				return OutcomeUnavailable(config.System, problem)
			}
			result.Warnings = append(result.Warnings, resource.Title+"："+problem.Message)
			result.Status = "partial"
			continue
		}
		result.Warnings = append(result.Warnings, warnings...)
		for _, item := range items {
			if item.OccurredAt == "" && start != nil {
				unknown = true
				continue
			}
			if start != nil {
				stamp, err := time.Parse(time.RFC3339Nano, item.OccurredAt)
				if err != nil || stamp.Before(*start) || !stamp.Before(*end) {
					continue
				}
			}
			key := item.ID + "\x00" + item.Version
			if seen[key] {
				continue
			}
			seen[key] = true
			result.Items = append(result.Items, item)
		}
	}
	if unknown {
		result.Warnings = append(result.Warnings, "有来源记录缺少可靠发生时间，已从本周结果排除；可在全部成果中查看。")
	}
	sort.SliceStable(result.Items, func(i, j int) bool {
		if result.Items[i].OccurredAt == result.Items[j].OccurredAt {
			return result.Items[i].ID < result.Items[j].ID
		}
		return result.Items[i].OccurredAt > result.Items[j].OccurredAt
	})
	// Coverage caveats remain warnings; actual read failures/truncation flag partial.
	for _, warning := range result.Warnings {
		if strings.Contains(warning, "无法") || strings.Contains(warning, "上限") || strings.Contains(warning, "超时") || strings.Contains(warning, "未核对") || strings.Contains(warning, "不一致") {
			result.Status = "partial"
		}
	}
	result.SyncedAt = time.Now().UTC().Format(time.RFC3339Nano)
	return result
}

func sourceTime(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		if parsed, err := time.Parse(time.RFC3339Nano, text); err == nil {
			return parsed.UTC().Format(time.RFC3339Nano)
		}
		return ""
	}
	var number float64
	if json.Unmarshal(raw, &number) == nil && number > 0 && number < 32503680000 {
		return time.Unix(int64(number), int64((number-float64(int64(number)))*1e9)).UTC().Format(time.RFC3339Nano)
	}
	return ""
}
func isoRaw(value string) json.RawMessage { data, _ := json.Marshal(value); return data }
func excerpt(value string) string {
	chars := []rune(strings.TrimSpace(value))
	if len(chars) > 1000 {
		return string(chars[:1000]) + "…"
	}
	return string(chars)
}
func outcome(p *prepared, r Resource, id, kind, title, summary, stamp, version, status, attribution string) Outcome {
	instance := sha256.Sum256([]byte(p.base.String()))
	return Outcome{ID: p.config.System + ":" + hex.EncodeToString(instance[:6]) + ":" + r.ID + ":" + kind + ":" + id, Source: p.config.System, ResourceID: r.ID, Kind: kind, Title: title, Summary: excerpt(summary), OccurredAt: stamp, SourceUpdatedAt: stamp, Status: status, URL: r.URL, Version: version, Attribution: attribution}
}
func cappedWarnings(count int, label string) []string {
	if count > 100 {
		return []string{label + "超过本批 100 条上限，更多内容请打开源产品查看。"}
	}
	return nil
}

func (c Client) paperOutcomes(ctx context.Context, p *prepared, r Resource) ([]Outcome, []string, *Diagnostic) {
	items := []Outcome{}
	warnings := []string{}
	data, problem := c.get(ctx, p, "papers/"+r.ID, nil, false)
	if problem != nil {
		return nil, nil, problem
	}
	var paper readPaper
	if json.Unmarshal(data, &paper) != nil || paper.Title == nil {
		return nil, nil, protocolMismatch(200)
	}
	id, valid := resourceID(paper.ID)
	if !valid || id != r.ID {
		return nil, nil, protocolMismatch(200)
	}
	active := versionString(paper.ActiveVersion)
	if strings.TrimSpace(paper.Summary) != "" {
		h := sha256.Sum256([]byte(paper.Summary))
		items = append(items, outcome(p, r, "summary", "material", r.Title+" · 阅读总结", paper.Summary, "", hex.EncodeToString(h[:8]), "time_unknown", "工作区已保存总结；原接口没有保存时间"))
	}
	data, problem = c.get(ctx, p, "workspace-notes", url.Values{"scope": {"paper"}, "scope_id": {r.ID}}, false)
	if problem != nil {
		if problem.HTTPStatus == 401 || problem.HTTPStatus == 403 {
			return nil, nil, problem
		}
		warnings = append(warnings, "阅读笔记无法读取："+problem.Message)
	} else {
		var notes struct {
			Items []struct {
				ID            string          `json:"id"`
				ScopeID       string          `json:"scope_id"`
				Title         string          `json:"title"`
				Content       string          `json:"content"`
				SourceMessage string          `json:"source_message_id"`
				Created       json.RawMessage `json:"created"`
				Updated       json.RawMessage `json:"updated"`
			} `json:"items"`
		}
		if json.Unmarshal(data, &notes) != nil || notes.Items == nil {
			return nil, nil, protocolMismatch(200)
		}
		warnings = append(warnings, cappedWarnings(len(notes.Items), "阅读笔记")...)
		for i, n := range notes.Items {
			if i >= 100 {
				break
			}
			if !safeID(n.ID) || n.ScopeID != r.ID {
				return nil, nil, protocolMismatch(200)
			}
			if strings.TrimSpace(n.Content) == "" {
				continue
			}
			stamp := sourceTime(n.Updated)
			if stamp == "" {
				stamp = sourceTime(n.Created)
			}
			attribution := "工作区保存的笔记；作者未单独记录"
			if n.SourceMessage != "" {
				attribution = "工作区保存的 AI 回答笔记；作者未单独记录"
			}
			items = append(items, outcome(p, r, n.ID, "note", n.Title+" · "+r.Title, n.Content, stamp, stamp, "saved", attribution))
		}
	}
	data, problem = c.get(ctx, p, "papers/"+r.ID+"/reports", nil, false)
	if problem != nil {
		if problem.HTTPStatus == 401 || problem.HTTPStatus == 403 {
			return nil, nil, problem
		}
		warnings = append(warnings, "阅读报告无法读取："+problem.Message)
	} else {
		var reports struct {
			Items []struct {
				ID      string          `json:"id"`
				Version string          `json:"version_id"`
				Created json.RawMessage `json:"created"`
			} `json:"items"`
		}
		if json.Unmarshal(data, &reports) != nil || reports.Items == nil {
			return nil, nil, protocolMismatch(200)
		}
		warnings = append(warnings, cappedWarnings(len(reports.Items), "阅读报告")...)
		for i, report := range reports.Items {
			if i >= 100 {
				break
			}
			if !safeID(report.ID) || !safeID(report.Version) {
				return nil, nil, protocolMismatch(200)
			}
			status := "generated"
			if active != "" && active != report.Version {
				status = "stale"
			}
			items = append(items, outcome(p, r, report.ID, "report", r.Title+" · 阅读报告", "已保存的 AI 阅读报告；生成报告不代表已读完论文。", sourceTime(report.Created), report.Version, status, "AI 生成，来自当前授权工作区"))
		}
	}
	if safeID(active) {
		data, problem = c.get(ctx, p, "papers/"+r.ID+"/annotations", url.Values{"version": {active}}, false)
		if problem != nil {
			if problem.HTTPStatus == 401 || problem.HTTPStatus == 403 {
				return nil, nil, problem
			}
			warnings = append(warnings, "文献批注无法读取："+problem.Message)
		} else {
			var annotations struct {
				Items []struct {
					ID      string          `json:"id"`
					PaperID string          `json:"paper_id"`
					Version string          `json:"version_id"`
					Page    int             `json:"page"`
					Quote   string          `json:"quote"`
					Note    string          `json:"note"`
					Created json.RawMessage `json:"created"`
					Updated json.RawMessage `json:"updated"`
				} `json:"items"`
			}
			if json.Unmarshal(data, &annotations) != nil || annotations.Items == nil {
				return nil, nil, protocolMismatch(200)
			}
			warnings = append(warnings, cappedWarnings(len(annotations.Items), "文献批注")...)
			for i, a := range annotations.Items {
				if i >= 100 {
					break
				}
				if !safeID(a.ID) || a.PaperID != r.ID || a.Version != active {
					return nil, nil, protocolMismatch(200)
				}
				stamp := sourceTime(a.Updated)
				if stamp == "" {
					stamp = sourceTime(a.Created)
				}
				items = append(items, outcome(p, r, a.ID, "annotation", fmt.Sprintf("%s · 第 %d 页批注", r.Title, a.Page), a.Quote+"\n"+a.Note, stamp, active+":"+stamp, "saved", "工作区摘录与批注；作者未单独记录"))
			}
		}
	}
	return items, warnings, nil
}

func (c Client) writingOutcomes(ctx context.Context, p *prepared, r Resource) ([]Outcome, []string, *Diagnostic) {
	items := []Outcome{}
	warnings := []string{}
	historyByOID := map[string]Outcome{}
	mergedCheckpoints := map[string]bool{}
	retractedCheckpoints := map[string]bool{}
	data, problem := c.get(ctx, p, "projects/"+r.ID+"/history-page", url.Values{"limit": {"100"}}, false)
	if problem != nil {
		if problem.HTTPStatus == 401 || problem.HTTPStatus == 403 {
			return nil, nil, problem
		}
		warnings = append(warnings, "写作版本无法读取："+problem.Message)
	} else {
		var history struct {
			Commits []struct {
				OID     string `json:"oid"`
				Message string `json:"message"`
				Created string `json:"createdAt"`
				Source  string `json:"source"`
			} `json:"commits"`
			NextCursor *string `json:"nextCursor"`
		}
		if json.Unmarshal(data, &history) != nil || history.Commits == nil {
			return nil, nil, protocolMismatch(200)
		}
		if history.NextCursor != nil && *history.NextCursor != "" {
			warnings = append(warnings, "写作版本超过本批 100 条上限，更多历史请打开 FastWrite 查看。")
		}
		for i, h := range history.Commits {
			if i >= 100 {
				break
			}
			if !safeID(h.OID) {
				return nil, nil, protocolMismatch(200)
			}
			kind, status := "version", "checkpoint"
			if h.Source == "automatic" {
				kind, status = "activity", "automatic"
			}
			record := outcome(p, r, h.OID, kind, r.Title+" · 写作版本", h.Message, sourceTime(isoRaw(h.Created)), h.OID, status, "项目历史记录；可能包含协作者的修改")
			historyByOID[h.OID] = record
			items = append(items, record)
		}
	}
	data, problem = c.get(ctx, p, "projects/"+r.ID+"/reviews", nil, false)
	if problem != nil {
		if problem.HTTPStatus == 401 || problem.HTTPStatus == 403 {
			return nil, nil, problem
		}
		warnings = append(warnings, "审阅报告无法读取："+problem.Message)
	} else {
		var reports []struct {
			ID         string `json:"id"`
			ProjectID  string `json:"projectId"`
			Assessment string `json:"overallAssessment"`
			Created    string `json:"createdAt"`
			Version    int    `json:"createdFromProjectVersion"`
			Stale      bool   `json:"stale"`
			Coverage   *struct {
				Failed      int `json:"failed"`
				Skipped     int `json:"skipped"`
				Unavailable int `json:"unavailable"`
			} `json:"coverage"`
		}
		if json.Unmarshal(data, &reports) != nil || reports == nil {
			return nil, nil, protocolMismatch(200)
		}
		warnings = append(warnings, cappedWarnings(len(reports), "审阅报告")...)
		for i, report := range reports {
			if i >= 100 {
				break
			}
			if !safeID(report.ID) || report.ProjectID != r.ID {
				return nil, nil, protocolMismatch(200)
			}
			status := "generated"
			if report.Coverage != nil && (report.Coverage.Failed+report.Coverage.Skipped+report.Coverage.Unavailable) > 0 {
				status = "partial"
			}
			if report.Stale {
				status = "stale"
			}
			items = append(items, outcome(p, r, report.ID, "review", r.Title+" · 审阅报告", report.Assessment, sourceTime(isoRaw(report.Created)), strconv.Itoa(report.Version), status, "AI 审阅报告；不等同于论文验收通过"))
		}
	}
	// Provenance is already project-authorized by FastWrite. Read only summaries;
	// proposed/rejected/rolled-back changes never become accomplished work.
	data, problem = c.get(ctx, p, "projects/"+r.ID+"/provenance", nil, false)
	if problem != nil {
		if problem.HTTPStatus == 401 || problem.HTTPStatus == 403 {
			return nil, nil, problem
		}
		warnings = append(warnings, "写作采纳记录无法读取："+problem.Message)
	} else {
		var provenance struct {
			Project struct {
				ID string `json:"id"`
			} `json:"project"`
			Changes []struct {
				ID       string   `json:"id"`
				Status   string   `json:"status"`
				Summary  string   `json:"summary"`
				Updated  string   `json:"updatedAt"`
				Finished string   `json:"reviewFinishedAt"`
				Applied  []string `json:"appliedCheckpointOids"`
			} `json:"changeSets"`
		}
		if json.Unmarshal(data, &provenance) != nil || provenance.Project.ID != r.ID || provenance.Changes == nil {
			return nil, nil, protocolMismatch(200)
		}
		warnings = append(warnings, cappedWarnings(len(provenance.Changes), "写作采纳记录")...)
		for i, change := range provenance.Changes {
			if i >= 100 {
				break
			}
			if !safeID(change.ID) {
				return nil, nil, protocolMismatch(200)
			}
			for _, oid := range change.Applied {
				if !safeID(oid) {
					return nil, nil, protocolMismatch(200)
				}
				if change.Status == "rolled-back" {
					retractedCheckpoints[oid] = true
				}
			}
			if change.Status != "accepted" && change.Status != "partially-accepted" {
				continue
			}
			stamp := sourceTime(isoRaw(change.Finished))
			if stamp == "" {
				stamp = sourceTime(isoRaw(change.Updated))
			}
			// An accepted change and its applied Git checkpoints describe the
			// same work. Keep one step card carrying all version references,
			// rather than counting every checkpoint again as an accomplishment.
			linkedTime := ""
			for _, oid := range change.Applied {
				mergedCheckpoints[oid] = true
				if historyByOID[oid].OccurredAt > linkedTime {
					linkedTime = historyByOID[oid].OccurredAt
				}
			}
			if stamp == "" {
				stamp = linkedTime
			}
			items = append(items, outcome(p, r, change.ID, "step", r.Title+" · 已采纳写作修改", change.Summary, stamp, strings.Join(change.Applied, ","), strings.ReplaceAll(change.Status, "-", "_"), "项目中已采纳的 AI 修改；保留源项目状态"))
		}
	}
	merged := make([]Outcome, 0, len(items))
	for _, item := range items {
		if item.Kind == "version" || item.Kind == "activity" {
			if mergedCheckpoints[item.Version] {
				continue
			}
			if retractedCheckpoints[item.Version] {
				item.Status = "retracted"
				item.Attribution = "该历史版本对应的写作修改已回滚；不计入有效成果"
			}
		}
		merged = append(merged, item)
	}
	return merged, warnings, nil
}

func (c Client) materialOutcomes(ctx context.Context, p *prepared, r Resource) ([]Outcome, []string, *Diagnostic) {
	var content Content
	var problem *Diagnostic
	if p.profile == "fastnews-panel" {
		content, problem = c.Read(ctx, p.config, r.ID, "")
	} else {
		content, problem = c.readExtra(ctx, p, r.ID, "")
	}
	if problem != nil {
		return nil, nil, problem
	}
	if strings.TrimSpace(content.Text) == "" {
		return []Outcome{}, nil, nil
	}
	stamp := ""
	// Only these adapters explicitly define Version as the source update time.
	// A deck revision/hash must not accidentally be treated as a timestamp.
	if p.profile == "fastlabs" || p.profile == "fastnews-local" {
		stamp = sourceTime(isoRaw(content.Version))
	}
	item := outcome(p, r, r.ID, "material", r.Title, content.Text, stamp, content.Version, "saved", "来源产品已保存的材料；不自动认定任务完成")
	if content.Resource.URL != "" {
		item.URL = content.Resource.URL
	}
	warnings := []string{"此来源展示已保存材料，不将项目存在、关注作者或页面访问当作完成成果。"}
	if content.Warning != nil {
		warnings = append(warnings, content.Warning.Message)
	}
	if content.Truncated {
		warnings = append(warnings, "材料超过摘要上限，完整内容请打开来源产品查看。")
	}
	return []Outcome{item}, warnings, nil
}
