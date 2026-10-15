package integrationclient

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
)

// Personal conclusions stay ahead of an automatically generated report so a
// long report cannot silently crowd the user's own reading work out of a tool result.
func (c Client) readPaperContent(ctx context.Context, p *prepared, id string) (Content, *Diagnostic) {
	content := Content{Children: []Resource{}}
	data, problem := c.get(ctx, p, "papers/"+id, nil, false)
	if problem != nil {
		return content, problem
	}
	var paper readPaper
	if json.Unmarshal(data, &paper) != nil || paper.Title == nil {
		return content, protocolMismatch(200)
	}
	actual, valid := resourceID(paper.ID)
	if !valid || actual != id {
		return content, protocolMismatch(200)
	}
	summary, _ := truncateText(paper.Summary)
	content.Resource = Resource{ID: id, Title: *paper.Title, Kind: "paper", URL: p.webEndpoint("/", url.Values{"paper": {id}}), Summary: summary}
	content.Version = versionString(paper.ActiveVersion)
	content.Evidence = &SourceEvidence{Kind: "paper_notes_report", Version: content.Version, State: "notes_report_only"}
	parts := []string{}
	if strings.TrimSpace(paper.Summary) != "" {
		parts = append(parts, "## 个人总结\n\n"+paper.Summary)
	}
	data, problem = c.get(ctx, p, "workspace-notes", url.Values{"scope": {"paper"}, "scope_id": {id}}, false)
	if problem == nil {
		var notes struct {
			Items []struct {
				Title    string          `json:"title"`
				Content  string          `json:"content"`
				ScopeID  string          `json:"scope_id"`
				Evidence json.RawMessage `json:"evidence"`
			} `json:"items"`
		}
		if json.Unmarshal(data, &notes) != nil || notes.Items == nil {
			return Content{Children: []Resource{}}, protocolMismatch(200)
		}
		for _, note := range notes.Items {
			if note.ScopeID != "" && note.ScopeID != id {
				return Content{Children: []Resource{}}, protocolMismatch(200)
			}
			if strings.TrimSpace(note.Content) == "" {
				continue
			}
			text := "## 阅读笔记 " + note.Title + "\n\n" + note.Content
			if len(note.Evidence) > 0 && string(note.Evidence) != "null" && string(note.Evidence) != "[]" {
				text += "\n\n来源证据：" + string(note.Evidence)
			}
			parts = append(parts, text)
		}
	} else {
		if problem.HTTPStatus == 401 || problem.HTTPStatus == 403 {
			return Content{Children: []Resource{}}, problem
		}
		content.Warning = problem
	}
	markdown, problem := c.get(ctx, p, "papers/"+id+"/export", nil, true)
	if problem != nil {
		if problem.HTTPStatus == 401 || problem.HTTPStatus == 403 || len(parts) == 0 {
			return Content{Children: []Resource{}}, problem
		}
		content.Warning = problem
	} else if len(markdown) > 0 {
		parts = append(parts, "## 阅读报告\n\n"+string(markdown))
	}
	content.Text, content.Truncated = truncateText(strings.Join(parts, "\n\n"))
	return content, nil
}
