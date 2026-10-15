package integrationclient

import (
	"context"
	"encoding/json"
	"strings"
)

func newsInboxBoundary() *Diagnostic {
	return diagnostic("READ_SIDE_EFFECT_UNSUPPORTED", "当前模式不自动读取资讯信箱", "FastNews 的现有信箱 GET 接口可能生成当天资讯并调用模型，不能保证只是读取已有内容。当前只读取研究印象和关注作者。", "资讯可在 FastNews 中查看；若使用 FastResearch 已存收件箱，可切换 fastnews-panel 协议。", 0, false)
}

type newsProfile struct {
	UserID     string          `json:"userId"`
	Authors    json.RawMessage `json:"authors"`
	CustomTags json.RawMessage `json:"customTags"`
	Impression struct {
		Text      string `json:"text"`
		UpdatedAt string `json:"updatedAt"`
	} `json:"impression"`
}

func (c Client) localNewsProfile(ctx context.Context, p *prepared) (*newsProfile, *Diagnostic) {
	data, problem := c.get(ctx, p, "auth/me", nil, false)
	if problem != nil {
		return nil, problem
	}
	var account struct {
		ID     string `json:"user_id"`
		Source string `json:"source"`
	}
	if json.Unmarshal(data, &account) != nil || !safeID(account.ID) || (account.Source != "local" && account.Source != "fastcas") {
		return nil, protocolMismatch(200)
	}
	// Unlike content/inbox, content/me only reads existing personal records.
	data, problem = c.get(ctx, p, "content/me", nil, false)
	if problem != nil {
		return nil, problem
	}
	var profile newsProfile
	if json.Unmarshal(data, &profile) != nil || profile.UserID != account.ID || len(profile.Authors) == 0 || len(profile.CustomTags) == 0 || !strings.HasPrefix(strings.TrimSpace(string(profile.Authors)), "[") || !strings.HasPrefix(strings.TrimSpace(string(profile.CustomTags)), "[") {
		return nil, protocolMismatch(200)
	}
	return &profile, nil
}

func (c Client) listNewsProfile(ctx context.Context, p *prepared, query string, limit, offset int) (Page, *Diagnostic) {
	profile, problem := c.localNewsProfile(ctx, p)
	if problem != nil {
		return Page{Items: []Resource{}}, problem
	}
	items := []Resource{{ID: "research-impression", Title: "个人研究印象", Kind: "research-profile", Summary: profile.Impression.Text}, {ID: "followed-authors", Title: "关注作者与研究标签", Kind: "research-profile", Summary: "已保存的作者与研究方向，不会触发资讯生成。"}}
	page := localPage(items, query, limit, offset)
	page.Warning = newsInboxBoundary()
	return page, nil
}

func (c Client) readNewsProfile(ctx context.Context, p *prepared, id string) (Content, *Diagnostic) {
	content := Content{Children: []Resource{}}
	if id != "research-impression" && id != "followed-authors" {
		return content, newsInboxBoundary()
	}
	profile, problem := c.localNewsProfile(ctx, p)
	if problem != nil {
		return content, problem
	}
	if id == "research-impression" {
		content.Resource = Resource{ID: id, Title: "个人研究印象", Kind: "research-profile"}
		content.Text, content.Truncated = truncateText(profile.Impression.Text)
		content.Version = profile.Impression.UpdatedAt
	} else {
		content.Resource = Resource{ID: id, Title: "关注作者与研究标签", Kind: "research-profile"}
		var builder strings.Builder
		builder.WriteString("## 关注作者\n\n")
		builder.Write(profile.Authors)
		builder.WriteString("\n\n## 研究标签\n\n")
		builder.Write(profile.CustomTags)
		content.Text, content.Truncated = truncateText(builder.String())
	}
	return content, nil
}
