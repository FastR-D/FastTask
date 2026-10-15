package integrationclient

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Research requests have fixed routes, never a model-supplied proxy URL. Keep
// the target's credential on its origin and never follow authentication redirects.
func (c Client) researchJSON(ctx context.Context, p *prepared, method, route string, query url.Values, body any, headers http.Header, timeout time.Duration) ([]byte, *Diagnostic) {
	var encoded []byte
	if body != nil {
		encoded, _ = json.Marshal(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, p.endpoint(route, query), bytes.NewReader(encoded))
	if err != nil {
		return nil, badConfig("无法构造目标系统请求。")
	}
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	for name, values := range headers {
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
	if p.cookie != "" {
		request.AddCookie(&http.Cookie{Name: p.cookie, Value: p.config.Credential})
	} else if p.config.Credential != "" {
		request.Header.Set("Authorization", "Bearer "+p.config.Credential)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	defer transport.CloseIdleConnections()
	client := &http.Client{Timeout: timeout, Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return nil, transportDiagnostic(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return nil, transportDiagnostic(err)
	}
	if len(data) > maxResponseBytes {
		return nil, diagnostic("RESPONSE_TOO_LARGE", "接口响应超过读取上限", "单次响应超过一兆字节，已停止读取。", "请缩小读取范围。", response.StatusCode, false)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, researchStatus(response.StatusCode, data)
	}
	if strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "text/html") || bytes.HasPrefix(bytes.TrimSpace(data), []byte("<")) {
		return nil, htmlDiagnostic(response.StatusCode, data)
	}
	if !json.Valid(data) {
		return nil, protocolMismatch(response.StatusCode)
	}
	return data, nil
}

func researchStatus(status int, data []byte) *Diagnostic {
	var remote struct {
		Code  string          `json:"code"`
		Error json.RawMessage `json:"error"`
	}
	_ = json.Unmarshal(data, &remote)
	if remote.Code == "" {
		_ = json.Unmarshal(remote.Error, &remote.Code)
	}
	if remote.Code == "" {
		var nested struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(remote.Error, &nested)
		remote.Code = nested.Code
	}
	switch remote.Code {
	case "authentication_required":
		return diagnostic("AUTH_REQUIRED", "FastWrite 需要登录授权", "当前请求没有 FastWrite 签发的会话凭据。", "请登录 FastWrite 并在连接设置中填写其本人凭据。", status, false)
	case "session_invalid":
		return diagnostic("SESSION_INVALID", "FastWrite 会话已失效", "当前 FastWrite 会话无效或已过期。", "请重新登录 FastWrite 并更新连接凭据。", status, false)
	case "version_conflict":
		return diagnostic("VERSION_CONFLICT", "正文已被其他操作更新", "保存使用的原版本已过期；没有覆盖当前正文。", "保留草稿和修改范围，重新读取后由用户决定如何合并。", status, false)
	case "file_exists":
		return diagnostic("FILE_EXISTS", "目标文件已经存在", "创建请求没有覆盖现有文件。", "读取现有正文并确认修改范围后再保存。", status, false)
	}
	return statusDiagnostic(status)
}

func researchPrepared(config Config, system string) (*prepared, *Diagnostic) {
	p, problem := prepare(config)
	if problem != nil {
		return nil, problem
	}
	if p.config.System != system {
		return nil, badConfig("工具操作与目标系统不匹配。")
	}
	if p.config.Credential == "" {
		return nil, diagnostic("AUTH_REQUIRED", "需要目标系统本人授权", "本操作必须使用目标系统签发的有效会话，个人实例确认不能替代身份授权。", "请登录目标系统并配置其本人会话。", 0, false)
	}
	if system == "fastread" && (p.cookie != "fastread_session" || p.config.AuthKind != "cookie") {
		return nil, diagnostic("AUTH_MODE_UNSUPPORTED", "FastRead 需要自己的 Cookie 会话", "FastRead 检索、导入和全文阅读使用 fastread_session 与匹配的 CSRF。", "在 FastRead 登录后配置 Cookie 会话，不要使用其他系统的 Bearer。", 0, false)
	}
	return p, nil
}

func (c Client) fastReadCSRF(ctx context.Context, p *prepared) (string, *Diagnostic) {
	data, problem := c.get(ctx, p, "auth/me", nil, false)
	if problem != nil {
		return "", problem
	}
	var auth struct {
		CSRF      string `json:"csrf"`
		Workspace string `json:"workspace_id"`
	}
	if json.Unmarshal(data, &auth) != nil || auth.CSRF == "" || auth.Workspace == "" || strings.ContainsAny(auth.CSRF, "\r\n") {
		return "", protocolMismatch(200)
	}
	return auth.CSRF, nil
}

type SearchPaper struct {
	Title            string          `json:"title"`
	Authors          []string        `json:"authors,omitempty"`
	Year             json.RawMessage `json:"year,omitempty"`
	DOI              string          `json:"doi,omitempty"`
	URL              string          `json:"url,omitempty"`
	SourceURL        string          `json:"source_url,omitempty"`
	PDFURL           string          `json:"pdf_url,omitempty"`
	Abstract         string          `json:"abstract,omitempty"`
	Source           string          `json:"source,omitempty"`
	DiscoverySources []string        `json:"discovery_sources"`
	IdentityWarning  string          `json:"identity_warning,omitempty"`
}
type SearchSource struct {
	Available   *bool           `json:"available"`
	Reason      string          `json:"reason,omitempty"`
	ResultCount int             `json:"result_count,omitempty"`
	RetryAfter  json.RawMessage `json:"retry_after,omitempty"`
}
type PublicSearchResult struct {
	Papers          []SearchPaper           `json:"papers"`
	Sources         map[string]SearchSource `json:"sources"`
	Status          string                  `json:"status"`
	ReadingState    string                  `json:"reading_state"`
	ReceivedPapers  int                     `json:"received_papers"`
	OmittedPapers   int                     `json:"omitted_papers"`
	OutputTruncated bool                    `json:"output_truncated"`
}

func (c Client) SearchPublicPapers(ctx context.Context, config Config, query string, limit int) (PublicSearchResult, *Diagnostic) {
	result := PublicSearchResult{Papers: []SearchPaper{}, ReadingState: "metadata_only"}
	p, problem := researchPrepared(config, "fastread")
	if problem != nil {
		return result, problem
	}
	query = strings.TrimSpace(query)
	if query == "" || len([]rune(query)) > 300 || limit < 1 || limit > 50 {
		return result, badConfig("公共检索需要 1 至 300 字查询与 1 至 50 项结果上限。")
	}
	csrf, problem := c.fastReadCSRF(ctx, p)
	if problem != nil {
		return result, problem
	}
	// FastRead waits up to 22 seconds for its independent public sources.
	data, problem := c.researchJSON(ctx, p, http.MethodPost, "search", nil, map[string]any{"query": query, "limit": limit}, http.Header{"X-Csrf-Token": {csrf}}, 30*time.Second)
	if problem != nil {
		return result, problem
	}
	if json.Unmarshal(data, &result) != nil || result.Papers == nil || len(result.Papers) > limit || len(result.Sources) == 0 || len(result.Sources) > 3 {
		return result, protocolMismatch(200)
	}
	available, failed := 0, 0
	for name, source := range result.Sources {
		if name != "arxiv" && name != "crossref" && name != "openalex" {
			return result, protocolMismatch(200)
		}
		if source.Available == nil {
			return result, protocolMismatch(200)
		}
		if *source.Available {
			available++
		} else {
			failed++
		}
	}
	for i := range result.Papers {
		paper := &result.Papers[i]
		if strings.TrimSpace(paper.Title) == "" || len(paper.DiscoverySources) == 0 {
			return result, protocolMismatch(200)
		}
		if len(paper.DiscoverySources) > 3 {
			return result, protocolMismatch(200)
		}
		for _, name := range paper.DiscoverySources {
			if _, valid := result.Sources[name]; !valid {
				return result, protocolMismatch(200)
			}
		}
		paper.URL, paper.SourceURL, paper.PDFURL = safeResourceURL(paper.URL), safeResourceURL(paper.SourceURL), safeResourceURL(paper.PDFURL)
		paper.Title, _ = truncateText(paper.Title)
		paper.Abstract, _ = truncateText(paper.Abstract)
	}
	switch {
	case available == 0:
		result.Status = "sources_unavailable"
	case failed > 0:
		result.Status = "partial_sources"
	case len(result.Papers) == 0:
		result.Status = "zero_results"
	default:
		result.Status = "results"
	}
	result.ReadingState = "metadata_only"
	result.ReceivedPapers = len(result.Papers)
	result.OmittedPapers, result.OutputTruncated = 0, false
	return result, nil
}

type PaperImportJob struct {
	ID         string             `json:"id"`
	Kind       string             `json:"kind"`
	ResourceID string             `json:"resource_id"`
	VersionID  string             `json:"version_id"`
	State      string             `json:"state"`
	ErrorCode  string             `json:"error_code,omitempty"`
	Error      string             `json:"error,omitempty"`
	Result     *PaperImportResult `json:"result,omitempty"`
}
type PaperImportResult struct {
	PaperID      string `json:"paper_id"`
	VersionID    string `json:"version_id"`
	Deduplicated bool   `json:"deduplicated,omitempty"`
}

func parseImportJob(data []byte) (PaperImportJob, *Diagnostic) {
	var job PaperImportJob
	if json.Unmarshal(data, &job) != nil || !safeID(job.ID) || job.Kind != "import_url" || (job.State != "queued" && job.State != "running" && job.State != "succeeded" && job.State != "failed" && job.State != "needs_attention" && job.State != "cancelled") {
		return job, protocolMismatch(200)
	}
	if job.Result != nil {
		if (job.Result.PaperID != "" && !safeID(job.Result.PaperID)) || (job.Result.VersionID != "" && !safeID(job.Result.VersionID)) {
			return PaperImportJob{}, protocolMismatch(200)
		}
		if job.ResourceID == "" {
			job.ResourceID = job.Result.PaperID
		}
		if job.VersionID == "" {
			job.VersionID = job.Result.VersionID
		}
	}
	if (job.ResourceID != "" && !safeID(job.ResourceID)) || (job.VersionID != "" && !safeID(job.VersionID)) {
		return PaperImportJob{}, protocolMismatch(200)
	}
	if len([]rune(job.ErrorCode)) > 100 {
		job.ErrorCode = string([]rune(job.ErrorCode)[:100])
	}
	if len([]rune(job.Error)) > 1000 {
		job.Error = string([]rune(job.Error)[:1000])
	}
	return job, nil
}
func (c Client) ImportPublicPaper(ctx context.Context, config Config, sourceURL, key string) (PaperImportJob, *Diagnostic) {
	p, problem := researchPrepared(config, "fastread")
	if problem != nil {
		return PaperImportJob{}, problem
	}
	if safeResourceURL(sourceURL) == "" || key == "" || len(key) > 160 || strings.ContainsAny(key, "\r\n") {
		return PaperImportJob{}, badConfig("导入需要公开论文 HTTP/HTTPS 地址与稳定幂等键。")
	}
	csrf, problem := c.fastReadCSRF(ctx, p)
	if problem != nil {
		return PaperImportJob{}, problem
	}
	// Never request paid report generation or substitute a user's model settings.
	data, problem := c.researchJSON(ctx, p, http.MethodPost, "imports/url", nil, map[string]any{"url": sourceURL, "organize": false}, http.Header{"X-Csrf-Token": {csrf}, "Idempotency-Key": {key}}, c.timeout())
	if problem != nil {
		return PaperImportJob{}, problem
	}
	return parseImportJob(data)
}
func (c Client) PaperImportJob(ctx context.Context, config Config, id string) (PaperImportJob, *Diagnostic) {
	p, problem := researchPrepared(config, "fastread")
	if problem != nil {
		return PaperImportJob{}, problem
	}
	if !safeID(id) {
		return PaperImportJob{}, badConfig("导入作业标识无效。")
	}
	data, problem := c.get(ctx, p, "jobs/"+id, nil, false)
	if problem != nil {
		return PaperImportJob{}, problem
	}
	job, problem := parseImportJob(data)
	if problem == nil && job.ID != id {
		problem = protocolMismatch(200)
	}
	return job, problem
}

// WaitPaperImportJob is bounded polling, not a background worker. Return the
// last queued/running state when the wait budget expires so it stays honest.
func (c Client) WaitPaperImportJob(ctx context.Context, config Config, id string, seconds int) (PaperImportJob, *Diagnostic) {
	if seconds < 0 || seconds > 20 {
		return PaperImportJob{}, badConfig("作业等待时间需要在 0 至 20 秒之间。")
	}
	if seconds == 0 {
		return c.PaperImportJob(ctx, config, id)
	}
	waitCtx, cancel := context.WithTimeout(ctx, time.Duration(seconds)*time.Second)
	defer cancel()
	job, problem := c.PaperImportJob(waitCtx, config, id)
	if problem != nil {
		return job, problem
	}
	for job.State == "queued" || job.State == "running" {
		timer := time.NewTimer(time.Second)
		select {
		case <-waitCtx.Done():
			timer.Stop()
			if ctx.Err() != nil {
				return job, transportDiagnostic(ctx.Err())
			}
			return job, nil
		case <-timer.C:
		}
		latest, latestProblem := c.PaperImportJob(waitCtx, config, id)
		if latestProblem != nil {
			if waitCtx.Err() != nil && ctx.Err() == nil {
				return job, nil
			}
			return latest, latestProblem
		}
		job = latest
	}
	return job, nil
}

type SourceEvidence struct {
	Kind       string `json:"kind"`
	PageNumber int    `json:"page_number,omitempty"`
	Version    string `json:"version"`
	PageCount  int    `json:"page_count,omitempty"`
	State      string `json:"state"`
}

func (c Client) ReadPaperPage(ctx context.Context, config Config, id string, number int, version string) (Content, *Diagnostic) {
	p, problem := researchPrepared(config, "fastread")
	if problem != nil {
		return Content{}, problem
	}
	if !safeID(id) || number < 1 || number > 100000 || (version != "" && !safeID(version)) {
		return Content{}, badConfig("论文、页码或版本标识无效。")
	}
	data, problem := c.get(ctx, p, "papers/"+id, nil, false)
	if problem != nil {
		return Content{}, problem
	}
	var detail struct {
		ID            json.RawMessage `json:"id"`
		Title         *string         `json:"title"`
		ActiveVersion string          `json:"active_version"`
		Versions      []struct {
			ID        string `json:"id"`
			PageCount int    `json:"page_count"`
		} `json:"versions"`
	}
	if json.Unmarshal(data, &detail) != nil || detail.Title == nil {
		return Content{}, protocolMismatch(200)
	}
	actual, valid := resourceID(detail.ID)
	if !valid || actual != id {
		return Content{}, protocolMismatch(200)
	}
	if version == "" {
		version = detail.ActiveVersion
	}
	if !safeID(version) {
		return Content{}, diagnostic("FULL_TEXT_UNAVAILABLE", "论文尚无可读全文", "资料库中只有元数据，未取得有效正文版本。", "导入可访问的公开 PDF，并等待导入作业成功。", 409, false)
	}
	count := 0
	for _, v := range detail.Versions {
		if v.ID == version {
			count = v.PageCount
		}
	}
	data, problem = c.get(ctx, p, "papers/"+id+"/pages/"+strconv.Itoa(number), url.Values{"version": {version}}, false)
	if problem != nil {
		return Content{}, problem
	}
	var page struct {
		Number  int     `json:"number"`
		Text    *string `json:"text"`
		Version string  `json:"version_id"`
	}
	if json.Unmarshal(data, &page) != nil || page.Number != number || page.Version != version || page.Text == nil {
		return Content{}, protocolMismatch(200)
	}
	text, truncated := truncateText(*page.Text)
	state := "page_text_read"
	if truncated {
		state = "page_text_excerpt"
	}
	if strings.TrimSpace(text) == "" {
		state = "page_text_empty"
	}
	return Content{Resource: Resource{ID: id, Title: *detail.Title, Kind: "paper_page", Path: "pages/" + strconv.Itoa(number) + "@" + version, URL: p.webEndpoint("/", url.Values{"paper": {id}})}, Text: text, Version: version, Children: []Resource{}, Truncated: truncated, Evidence: &SourceEvidence{Kind: "paper_full_text_page", PageNumber: number, Version: version, PageCount: count, State: state}}, nil
}
