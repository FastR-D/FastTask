// Package integrationclient implements a small, fixed set of verified
// external content protocols. It is not a general HTTP proxy.
package integrationclient

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

const (
	maxResponseBytes = 1 << 20
	maxTextBytes     = 64 << 10
)

type Config struct {
	System           string `json:"system"`
	APIURL           string `json:"api_url"`
	WebURL           string `json:"web_url"`
	Profile          string `json:"profile"`
	AuthKind         string `json:"auth_kind"`
	Credential       string `json:"-"`
	PersonalInstance bool   `json:"personal_instance"`
}

type Client struct{ Timeout time.Duration }

type Diagnostic struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	Reason     string `json:"reason"`
	Action     string `json:"action"`
	HTTPStatus int    `json:"http_status,omitempty"`
	Retryable  bool   `json:"retryable"`
}

type Resource struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Kind    string `json:"kind"`
	URL     string `json:"url"`
	Summary string `json:"summary"`
	Path    string `json:"path,omitempty"`
}

type Page struct {
	Items   []Resource  `json:"items"`
	Limit   int         `json:"limit"`
	Offset  int         `json:"offset"`
	HasMore bool        `json:"has_more"`
	Warning *Diagnostic `json:"warning,omitempty"`
}

type Content struct {
	Resource  Resource        `json:"resource"`
	Text      string          `json:"text"`
	Children  []Resource      `json:"children"`
	Version   string          `json:"version"`
	Truncated bool            `json:"truncated"`
	Warning   *Diagnostic     `json:"warning,omitempty"`
	Evidence  *SourceEvidence `json:"evidence,omitempty"`
}

type prepared struct {
	config  Config
	base    *url.URL
	profile string
	cookie  string
}

func diagnostic(code, message, reason, action string, status int, retryable bool) *Diagnostic {
	return &Diagnostic{Code: code, Message: message, Reason: reason, Action: action, HTTPStatus: status, Retryable: retryable}
}

func badConfig(reason string) *Diagnostic {
	return diagnostic("CONFIG_INVALID", "连接配置无效", reason, "请检查系统、适配协议、地址和鉴权设置。", 0, false)
}

func protocolMismatch(status int) *Diagnostic {
	return diagnostic("CONTRACT_MISMATCH", "接口响应与适配协议不一致", "服务返回的内容不是该适配器约定的数据结构，不能据此读取个人材料。", "请核对部署版本和适配协议；不要把网页地址或健康检查成功当作内容授权。", status, false)
}

func parseBase(value string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed == nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.Opaque != "" {
		return nil, errors.New("invalid address")
	}
	if parsed.RawPath != "" || strings.ContainsAny(parsed.Path, "\\\x00\r\n") {
		return nil, errors.New("invalid address path")
	}
	for _, segment := range strings.Split(parsed.Path, "/") {
		if segment == "." || segment == ".." {
			return nil, errors.New("invalid address path")
		}
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	if parsed.Port() != "" {
		port, err := strconv.Atoi(parsed.Port())
		if err != nil || port < 1 || port > 65535 {
			return nil, errors.New("invalid port")
		}
	}
	return parsed, nil
}

// ValidateAddress checks the same address syntax used by live requests, without
// making a connection or requiring a credential to have been entered yet.
func ValidateAddress(value string) error {
	_, err := parseBase(value)
	return err
}

func loopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func prepare(config Config) (*prepared, *Diagnostic) {
	return prepareWithAuthorization(config, true)
}

func prepareWithAuthorization(config Config, requireAuthorization bool) (*prepared, *Diagnostic) {
	config.System = strings.ToLower(strings.TrimSpace(config.System))
	config.Profile = strings.ToLower(strings.TrimSpace(config.Profile))
	config.AuthKind = strings.ToLower(strings.TrimSpace(config.AuthKind))
	profiles := map[string]string{"fastwrite": "fastwrite", "fastread": "fastread-web", "fastnews": "fastnews-local", "fastppt": "fastppt-studio", "fastlab": "fastlabs"}
	profile, supported := profiles[config.System]
	if !supported {
		return nil, diagnostic("API_NOT_ADAPTED", "尚未适配此系统的内容接口", "本服务尚未确认该系统的内容读取协议；这不表示对方一定没有 API。", "请先确认目标实例的公开接口、版本和鉴权协议，再添加专用适配器。", 0, false)
	}
	if config.Profile == "fastnews-panel" && config.System == "fastnews" {
		profile = "fastnews-panel"
	}
	if config.Profile != "" && config.Profile != "auto" && config.Profile != profile {
		return nil, badConfig("所选适配协议与系统不匹配。")
	}
	address := strings.TrimSpace(config.APIURL)
	if address == "" {
		address = strings.TrimSpace(config.WebURL)
	}
	if address == "" {
		return nil, diagnostic("CONFIG_MISSING", "尚未配置系统地址", "没有可用于读取内容的 API 地址或网页地址。", "请填写目标实例的实际地址；不会自动猜测端口。", 0, false)
	}
	base, err := parseBase(address)
	if err != nil {
		return nil, badConfig("地址必须是没有用户名、密码、查询参数或片段的 HTTP 或 HTTPS 地址。")
	}
	if config.WebURL != "" {
		if _, err := parseBase(config.WebURL); err != nil {
			return nil, badConfig("网页地址必须是没有用户名、密码、查询参数或片段的 HTTP 或 HTTPS 地址。")
		}
	}
	for _, char := range config.Credential {
		if char < 32 || char == 127 {
			return nil, badConfig("凭据只能填写值，不能包含换行或控制字符。")
		}
	}
	p := &prepared{config: config, base: base, profile: profile}
	if profile == "fastppt-studio" || profile == "fastlabs" {
		if !config.PersonalInstance || !loopback(base.Hostname()) || config.Credential != "" {
			return nil, diagnostic("BOUNDARY_UNSUPPORTED", "该工具仅支持本机个人实例", "当前接口没有已确认的多用户读取授权，不能把访问令牌当成资源隔离能力。", "请由管理员配置同机回环地址并明确确认个人实例；远程部署需另行提供受控访问接口。", 0, false)
		}
		if config.AuthKind == "cookie" {
			return nil, diagnostic("AUTH_MODE_UNSUPPORTED", "该只读接口未适配 Cookie 授权", "本机个人工具接口不使用科研账号会话。", "选择 Bearer 模式、保持凭据为空，并确认本机个人实例。", 0, false)
		}
		p.config.AuthKind = ""
	}
	if config.AuthKind != "" && config.AuthKind != "bearer" && config.AuthKind != "cookie" {
		return nil, diagnostic("AUTH_MODE_UNSUPPORTED", "不支持此鉴权方式", "适配器仅接受已确认的 Bearer 凭据或指定会话 Cookie。", "请使用对应系统签发的凭据，不要转用 FastTask 的登录令牌。", 0, false)
	}
	if config.AuthKind == "cookie" {
		switch profile {
		case "fastread-web":
			p.cookie = "fastread_session"
		case "fastnews-panel":
			p.cookie = "fr_session"
		case "fastnews-local":
			p.cookie = "fastnews_session"
		default:
			return nil, diagnostic("AUTH_MODE_UNSUPPORTED", "尚未确认该系统的 Cookie 协议", "FastWrite 的 Cookie 登录协议尚未适配，不能猜测会话名称。", "请配置明确支持的 Bearer 凭据，或使用已确认的本机个人实例。", 0, false)
		}
		for _, char := range config.Credential {
			if char < 33 || char > 126 || strings.ContainsRune("\";,\\", char) {
				return nil, badConfig("Cookie 凭据只能填写单个会话值，不能包含 Cookie 名称或额外 Cookie。")
			}
		}
	}
	if config.Credential == "" {
		if requireAuthorization && (!config.PersonalInstance || !loopback(base.Hostname())) {
			return nil, diagnostic("AUTH_REQUIRED", "需要授权后才能读取个人内容", "匿名读取仅适用于用户明确确认的本机个人实例；远程实例不能以健康检查成功代替授权。", "请添加该系统的有效凭据，或确认当前地址是本机个人实例。", 0, false)
		}
	} else if config.AuthKind == "" {
		return nil, badConfig("已填写凭据，但没有选择 Bearer 或 Cookie 鉴权方式。")
	}
	return p, nil
}

func (p *prepared) endpoint(route string, query url.Values) string {
	target := *p.base
	if route == "/healthz" {
		target.Path = "/healthz"
		target.RawQuery = query.Encode()
		return target.String()
	}
	prefix := strings.TrimSuffix(target.Path, "/api")
	target.Path = prefix + "/api/" + strings.TrimPrefix(route, "/")
	target.RawQuery = query.Encode()
	return target.String()
}

// webEndpoint is a human navigation link, never an authenticated API URL. A
// configured web origin can differ from the API origin (notably FastRead).
func (p *prepared) webEndpoint(route string, query url.Values) string {
	base := p.base
	if p.config.WebURL != "" {
		parsed, err := parseBase(p.config.WebURL)
		if err != nil {
			return ""
		}
		base = parsed
	}
	target := *base
	target.Path = strings.TrimSuffix(target.Path, "/api") + route
	if target.Path == "" {
		target.Path = "/"
	}
	target.RawQuery = query.Encode()
	return target.String()
}

func (c Client) timeout() time.Duration {
	if c.Timeout <= 0 || c.Timeout > 10*time.Second {
		return 10 * time.Second
	}
	return c.Timeout
}

func transportDiagnostic(err error) *Diagnostic {
	var dns *net.DNSError
	var netErr net.Error
	var cert *tls.CertificateVerificationError
	var unknown x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	var record tls.RecordHeaderError
	switch {
	case errors.Is(err, context.Canceled):
		return diagnostic("REQUEST_CANCELLED", "读取已取消", "当前请求已被取消，没有确认内容读取成功。", "需要时可重新发起读取。", 0, false)
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()):
		return diagnostic("NETWORK_TIMEOUT", "连接或读取超时", "目标服务未在限定时间内响应。", "请检查目标服务和网络状态后重试。", 0, true)
	case errors.As(err, &dns):
		return diagnostic("DNS_ERROR", "无法解析服务地址", "配置的主机名无法解析，尚未连接到目标服务。", "请核对主机名和 DNS 配置。", 0, true)
	case errors.Is(err, syscall.ECONNREFUSED):
		return diagnostic("CONNECTION_REFUSED", "目标服务拒绝连接", "目标端口没有接受连接，可能未启动或未开放。", "请启动目标服务并核对实际端口。", 0, true)
	case errors.As(err, &cert) || errors.As(err, &unknown) || errors.As(err, &hostname) || errors.As(err, &invalid) || errors.As(err, &record):
		return diagnostic("TLS_ERROR", "安全连接校验失败", "TLS 握手或证书校验失败，未忽略证书错误。", "请检查 HTTPS 地址和证书信任配置。", 0, false)
	default:
		return diagnostic("NETWORK_ERROR", "无法读取目标服务", "网络连接未完成；诊断不会回显地址、请求参数或凭据。", "请检查网络及目标服务，确认连接配置后重试。", 0, true)
	}
}

func statusDiagnostic(status int) *Diagnostic {
	switch {
	case status >= 300 && status < 400:
		return diagnostic("SSO_REQUIRED", "服务要求跳转或重新登录", "目标接口返回了跳转；适配器没有跟随跳转或把凭据发送到其他地址。", "请完成该系统登录并更新凭据，同时确认配置的是实际 API 地址。", status, false)
	case status == 401:
		return diagnostic("AUTH_REQUIRED", "授权凭据无效或已过期", "内容接口拒绝了当前身份，健康状态不能证明有读取权限。", "请重新登录目标系统并更新该连接的凭据。", status, false)
	case status == 403:
		return diagnostic("ACCESS_DENIED", "没有读取该内容的权限", "目标系统明确拒绝了当前身份的读取请求。", "请检查资源所属账号和目标系统授权范围。", status, false)
	case status == 404:
		return diagnostic("API_NOT_FOUND", "接口或资源不存在", "目标地址未找到适配器请求的接口或资源；不能据此断言该系统没有 API。", "请核对部署版本、适配协议以及资源是否仍存在。", status, false)
	case status == 405:
		return diagnostic("METHOD_NOT_ALLOWED", "接口不支持此只读请求", "目标接口不接受约定的 GET 请求，适配器不会尝试写入方法。", "请确认接口版本和只读访问协议。", status, false)
	case status == 429:
		return diagnostic("RATE_LIMITED", "目标服务暂时限流", "目标服务要求降低请求频率。", "请稍后重试，避免连续重复请求。", status, true)
	case status >= 500:
		return diagnostic("UPSTREAM_UNAVAILABLE", "目标服务暂时异常", "目标服务返回了服务器错误，尚未确认内容可读取。", "请检查目标服务运行状态并稍后重试。", status, true)
	case status == 409:
		return diagnostic("RESOURCE_NOT_READY", "内容暂时不能导出或读取", "目标资源当前状态不满足读取条件。", "请在目标系统确认资源版本和处理状态后重试。", status, false)
	case status == 415:
		return diagnostic("BINARY_CONTENT_UNSUPPORTED", "当前文件不能作为文本读取", "目标接口返回不支持的内容类型，可能选择了图片、PDF 或其他二进制文件。", "请选择正文、笔记或报告文本；原文件可在来源工具中查看。", status, false)
	default:
		return protocolMismatch(status)
	}
}

func htmlDiagnostic(status int, data []byte) *Diagnostic {
	lower := bytes.ToLower(data)
	if bytes.Contains(lower, []byte("password")) || bytes.Contains(lower, []byte("login")) || bytes.Contains(lower, []byte("登录")) || bytes.Contains(lower, []byte("single sign")) {
		return diagnostic("SSO_REQUIRED", "收到登录页面而非内容接口", "目标服务返回了登录网页，现有凭据没有完成内容授权。", "请登录目标系统并更新会话凭据，确认实际 API 地址。", status, false)
	}
	return diagnostic("HTML_RESPONSE", "收到网页而非接口数据", "请求返回了 HTML，可能命中了网页入口或反向代理回退页。", "请核对 API 地址和适配协议；网页能打开不代表内容接口可用。", status, false)
}

func (c Client) get(ctx context.Context, p *prepared, route string, query url.Values, markdown bool) ([]byte, *Diagnostic) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, p.endpoint(route, query), nil)
	if err != nil {
		return nil, badConfig("无法构造该适配协议的只读请求。")
	}
	if markdown {
		request.Header.Set("Accept", "text/markdown")
	} else {
		request.Header.Set("Accept", "application/json")
	}
	if p.config.Credential != "" {
		if p.cookie != "" {
			request.AddCookie(&http.Cookie{Name: p.cookie, Value: p.config.Credential})
		} else {
			request.Header.Set("Authorization", "Bearer "+p.config.Credential)
		}
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if p.profile == "fastppt-studio" || p.profile == "fastlabs" {
		transport.Proxy = nil
		transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, err
			}
			for _, address := range addresses {
				if !address.IP.IsLoopback() {
					return nil, errors.New("local-only address did not resolve to loopback")
				}
			}
			if len(addresses) == 0 {
				return nil, errors.New("local-only address has no loopback result")
			}
			return (&net.Dialer{Timeout: c.timeout()}).DialContext(ctx, network, net.JoinHostPort(addresses[0].IP.String(), port))
		}
	}
	defer transport.CloseIdleConnections()
	httpClient := &http.Client{Timeout: c.timeout(), Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := httpClient.Do(request)
	if err != nil {
		return nil, transportDiagnostic(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return nil, transportDiagnostic(err)
	}
	if len(data) > maxResponseBytes {
		return nil, diagnostic("RESPONSE_TOO_LARGE", "接口响应超过读取上限", "单次响应超过一兆字节，适配器已停止读取。", "请缩小资源范围或在目标系统打开完整内容。", 200, false)
	}
	if response.StatusCode != http.StatusOK {
		return nil, researchStatus(response.StatusCode, data)
	}
	trimmed := bytes.TrimSpace(data)
	contentType := strings.ToLower(response.Header.Get("Content-Type"))
	if strings.Contains(contentType, "text/html") || bytes.HasPrefix(bytes.ToLower(trimmed), []byte("<!doctype html")) || bytes.HasPrefix(bytes.ToLower(trimmed), []byte("<html")) {
		return nil, htmlDiagnostic(200, data)
	}
	if markdown {
		if !strings.HasPrefix(contentType, "text/markdown") || !utf8.Valid(data) {
			return nil, protocolMismatch(200)
		}
	} else if !json.Valid(data) || (contentType != "" && !strings.Contains(contentType, "application/json")) {
		return nil, protocolMismatch(200)
	}
	return data, nil
}

// Diagnose verifies both health and the actual authorized content-list contract.
// A valid empty list is successful, without claiming that it proves ownership.
func (c Client) Diagnose(ctx context.Context, config Config) Diagnostic {
	// Public reachability is checked without forwarding credentials. Reporting
	// success still requires authorization and a real content-list response.
	healthConfig := config
	healthConfig.Credential, healthConfig.AuthKind, healthConfig.PersonalInstance = "", "", true
	p, problem := prepareWithAuthorization(healthConfig, false)
	if problem != nil {
		return *problem
	}
	healthRoute, extra := extraHealthProfile(p.profile)
	if !extra {
		healthRoute = "health"
	}
	data, problem := c.get(ctx, p, healthRoute, nil, false)
	if problem != nil {
		return *problem
	}
	var health struct {
		Status string `json:"status"`
		OK     *bool  `json:"ok"`
	}
	if err := json.Unmarshal(data, &health); err != nil || (extra && !extraHealthValid(p.profile, data)) || (p.profile == "fastwrite" && health.Status != "ok") || (p.profile == "fastread-web" && health.Status != "healthy") || (p.profile == "fastnews-panel" && (health.OK == nil || !*health.OK)) {
		return *protocolMismatch(200)
	}
	p, problem = prepare(config)
	if problem != nil {
		return *problem
	}
	if problem := c.checkWritingIdentity(ctx, p); problem != nil {
		return *problem
	}
	page, problem := c.list(ctx, p, "", 1, 0)
	if problem != nil {
		return *problem
	}
	result := diagnostic("OK", "内容接口已连接", "健康检查和实际内容列表均符合所选协议；空列表不代表拥有材料或额外权限。", "可以搜索并读取当前账号已获授权的材料。", 200, false)
	if page.Warning != nil {
		result.Reason += " " + page.Warning.Reason
		result.Action = page.Warning.Action
	}
	return *result
}

func (c Client) List(ctx context.Context, config Config, query string, limit, offset int) (Page, *Diagnostic) {
	p, problem := prepare(config)
	if problem != nil {
		return Page{Items: []Resource{}}, problem
	}
	if limit <= 0 {
		limit = 20
	}
	if problem := c.checkWritingIdentity(ctx, p); problem != nil {
		return Page{Items: []Resource{}}, problem
	}
	if limit > 100 {
		limit = 100
	}
	if offset < 0 || offset > 1000000 || len(query) > 1000 || strings.ContainsAny(query, "\x00\r\n") {
		return Page{Items: []Resource{}}, diagnostic("INPUT_INVALID", "查询范围无效", "偏移量或查询长度超出了只读接口支持的范围。", "请缩短查询内容并使用有效分页参数。", 0, false)
	}
	return c.list(ctx, p, strings.TrimSpace(query), limit, offset)
}

func resourceID(raw json.RawMessage) (string, bool) {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		var number json.Number
		if err := json.Unmarshal(raw, &number); err != nil {
			return "", false
		}
		value = number.String()
	}
	return value, safeID(value)
}

func safeID(value string) bool {
	if value == "" || len(value) > 256 || value == "." || value == ".." {
		return false
	}
	for _, char := range value {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '_' || char == '-' || char == '.') {
			return false
		}
	}
	return true
}

func safeFilePath(value string) bool {
	if value == "" || len(value) > 2048 || !utf8.ValidString(value) || strings.HasPrefix(value, "/") || strings.ContainsAny(value, "\\:%\x00\r\n") {
		return false
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

func truncateText(value string) (string, bool) {
	if len(value) <= maxTextBytes {
		return value, false
	}
	value = value[:maxTextBytes]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value, true
}

func localPage(items []Resource, query string, limit, offset int) Page {
	filtered := make([]Resource, 0, len(items))
	for _, item := range items {
		if query == "" || strings.Contains(strings.ToLower(item.Title+" "+item.Summary), strings.ToLower(query)) {
			filtered = append(filtered, item)
		}
	}
	page := Page{Items: []Resource{}, Limit: limit, Offset: offset}
	if offset < len(filtered) {
		end := offset + limit
		if end > len(filtered) {
			end = len(filtered)
		}
		page.Items, page.HasMore = filtered[offset:end], end < len(filtered)
	}
	return page
}

type writeProject struct {
	ID          json.RawMessage `json:"id"`
	Name        *string         `json:"name"`
	Description string          `json:"description"`
	Version     json.RawMessage `json:"version"`
}

func (c Client) writingProjects(ctx context.Context, p *prepared) ([]writeProject, *Diagnostic) {
	data, problem := c.get(ctx, p, "projects", nil, false)
	if problem != nil {
		return nil, problem
	}
	var projects []writeProject
	if !bytes.HasPrefix(bytes.TrimSpace(data), []byte("[")) || json.Unmarshal(data, &projects) != nil {
		return nil, protocolMismatch(200)
	}
	for _, project := range projects {
		if _, valid := resourceID(project.ID); !valid || project.Name == nil || strings.TrimSpace(*project.Name) == "" {
			return nil, protocolMismatch(200)
		}
	}
	return projects, nil
}

type readPaper struct {
	ID            json.RawMessage `json:"id"`
	Title         *string         `json:"title"`
	Summary       string          `json:"summary"`
	ActiveVersion json.RawMessage `json:"active_version"`
}

type panelInbox struct {
	Items []struct {
		ID      json.RawMessage `json:"id"`
		Title   *string         `json:"title"`
		TitleZH string          `json:"title_zh"`
		Kind    string          `json:"kind"`
		URL     string          `json:"url"`
		Summary string          `json:"summary"`
		Reason  string          `json:"reason"`
	} `json:"items"`
	Unread *int `json:"unread"`
}

func (c Client) list(ctx context.Context, p *prepared, query string, limit, offset int) (Page, *Diagnostic) {
	page := Page{Items: []Resource{}, Limit: limit, Offset: offset}
	switch p.profile {
	case "fastwrite":
		projects, problem := c.writingProjects(ctx, p)
		if problem != nil {
			return page, problem
		}
		resources := []Resource{}
		for _, project := range projects {
			id, valid := resourceID(project.ID)
			if !valid || project.Name == nil {
				return page, protocolMismatch(200)
			}
			summary, _ := truncateText(project.Description)
			resources = append(resources, Resource{ID: id, Title: *project.Name, Kind: "project", URL: p.webEndpoint("/projects/"+id, nil), Summary: summary})
		}
		return localPage(resources, query, limit, offset), nil
	case "fastread-web":
		params := url.Values{"limit": {strconv.Itoa(limit)}, "offset": {strconv.Itoa(offset)}}
		if query != "" {
			params.Set("q", query)
		}
		data, problem := c.get(ctx, p, "papers", params, false)
		if problem != nil {
			return page, problem
		}
		var result struct {
			Items  []readPaper `json:"items"`
			Total  *int        `json:"total"`
			Limit  *int        `json:"limit"`
			Offset *int        `json:"offset"`
		}
		if json.Unmarshal(data, &result) != nil || result.Items == nil || result.Total == nil || *result.Total < 0 || result.Limit == nil || *result.Limit != limit || result.Offset == nil || *result.Offset != offset || len(result.Items) > limit {
			return page, protocolMismatch(200)
		}
		for _, paper := range result.Items {
			id, valid := resourceID(paper.ID)
			if !valid || paper.Title == nil {
				return page, protocolMismatch(200)
			}
			summary, _ := truncateText(paper.Summary)
			page.Items = append(page.Items, Resource{ID: id, Title: *paper.Title, Kind: "paper", URL: p.webEndpoint("/", url.Values{"paper": {id}}), Summary: summary})
		}
		page.HasMore = offset+len(page.Items) < *result.Total
		return page, nil
	case "fastnews-panel":
		data, problem := c.get(ctx, p, "content/inbox", nil, false)
		if problem != nil {
			return page, problem
		}
		var inbox panelInbox
		if json.Unmarshal(data, &inbox) != nil || inbox.Items == nil || inbox.Unread == nil || *inbox.Unread < 0 {
			return page, protocolMismatch(200)
		}
		resources := []Resource{}
		for _, item := range inbox.Items {
			id, valid := resourceID(item.ID)
			if !valid || item.Title == nil || item.Kind == "" {
				return page, protocolMismatch(200)
			}
			title := *item.Title
			if item.TitleZH != "" {
				title = item.TitleZH
			}
			summary, _ := truncateText(item.Summary)
			resources = append(resources, Resource{ID: id, Title: title, Kind: item.Kind, URL: safeResourceURL(item.URL), Summary: summary})
		}
		return localPage(resources, query, limit, offset), nil
	}
	return c.listExtra(ctx, p, query, limit, offset)
}

func safeResourceURL(value string) string {
	parsed, err := url.Parse(value)
	if err != nil || parsed.User != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Hostname() == "" {
		return ""
	}
	return parsed.String()
}

func versionString(raw json.RawMessage) string {
	var value string
	if json.Unmarshal(raw, &value) == nil {
		return value
	}
	var number json.Number
	if json.Unmarshal(raw, &number) == nil {
		return number.String()
	}
	return ""
}

func (c Client) Read(ctx context.Context, config Config, id, filePath string) (Content, *Diagnostic) {
	content := Content{Children: []Resource{}}
	p, problem := prepare(config)
	if problem != nil {
		return content, problem
	}
	if problem := c.checkWritingIdentity(ctx, p); problem != nil {
		return content, problem
	}
	if p.profile == "fastread-web" && filePath != "" {
		parts := strings.Split(strings.TrimPrefix(filePath, "pages/"), "@")
		if strings.HasPrefix(filePath, "pages/") && len(parts) == 2 {
			number, err := strconv.Atoi(parts[0])
			if err == nil {
				return c.ReadPaperPage(ctx, config, id, number, parts[1])
			}
		}
		return content, badConfig("论文正文路径需要明确页码与版本。")
	}
	if !safeID(id) || (filePath != "" && (p.profile != "fastwrite" || !safeFilePath(filePath))) {
		return content, diagnostic("INPUT_INVALID", "资源标识或文件路径无效", "仅接受当前适配器的资源标识和相对文件路径，不允许路径穿越。", "请从资源列表选择内容或填写项目内的有效文件路径。", 0, false)
	}
	switch p.profile {
	case "fastwrite":
		return c.readWrite(ctx, p, id, filePath)
	case "fastread-web":
		return c.readPaperContent(ctx, p, id)
	case "fastnews-panel":
		// The verified Panel contract has no per-item read endpoint. Reading the
		// same authorized inbox is deliberate; never invent an item-detail path.
		data, problem := c.get(ctx, p, "content/inbox", nil, false)
		if problem != nil {
			return content, problem
		}
		var inbox panelInbox
		if json.Unmarshal(data, &inbox) != nil || inbox.Items == nil || inbox.Unread == nil {
			return content, protocolMismatch(200)
		}
		for _, item := range inbox.Items {
			actual, valid := resourceID(item.ID)
			if !valid || item.Title == nil || item.Kind == "" {
				return content, protocolMismatch(200)
			}
			if actual != id {
				continue
			}
			title := *item.Title
			if item.TitleZH != "" {
				title = item.TitleZH
			}
			summary, _ := truncateText(item.Summary)
			content.Resource = Resource{ID: id, Title: title, Kind: item.Kind, URL: safeResourceURL(item.URL), Summary: summary}
			content.Text, content.Truncated = truncateText(strings.TrimSpace(item.Summary + "\n\n" + item.Reason))
			return content, nil
		}
		return content, diagnostic("RESOURCE_NOT_FOUND", "收件箱中没有该材料", "当前授权的收件箱中未找到指定资源。", "请刷新材料列表，确认资源仍属于当前账号。", 404, false)
	}
	return c.readExtra(ctx, p, id, filePath)
}

func (c Client) checkWritingIdentity(ctx context.Context, p *prepared) *Diagnostic {
	if p.profile != "fastwrite" || p.config.Credential == "" {
		return nil
	}
	data, problem := c.get(ctx, p, "auth/me", nil, false)
	if problem != nil {
		return problem
	}
	var identity struct {
		ID json.RawMessage `json:"id"`
	}
	if json.Unmarshal(data, &identity) != nil {
		return protocolMismatch(200)
	}
	if _, valid := resourceID(identity.ID); !valid {
		return diagnostic("AUTH_REQUIRED", "写作身份尚未确认", "FastWrite 的匿名项目列表也可能返回空数组；当前凭据未取得有效账号身份。", "请重新登录 FastWrite 并更新访问凭据。", 401, false)
	}
	return nil
}

type writeNode struct {
	Type     string          `json:"type"`
	Path     string          `json:"path"`
	Name     string          `json:"name"`
	Kind     string          `json:"kind"`
	Version  json.RawMessage `json:"version"`
	Children []writeNode     `json:"children"`
}

func (c Client) readWrite(ctx context.Context, p *prepared, id, filePath string) (Content, *Diagnostic) {
	content := Content{Children: []Resource{}}
	if filePath == "" {
		projects, problem := c.writingProjects(ctx, p)
		if problem != nil {
			return content, problem
		}
		var selected *writeProject
		for i := range projects {
			projectID, _ := resourceID(projects[i].ID)
			if projectID == id {
				selected = &projects[i]
				break
			}
		}
		if selected == nil {
			return content, diagnostic("RESOURCE_NOT_FOUND", "项目已不在本人写作列表中", "当前授权的项目列表中没有所选项目；不会把内部标识当作项目标题。", "请刷新项目列表，确认项目仍存在且当前账号有访问权限。", 404, false)
		}
		data, problem := c.get(ctx, p, "projects/"+id+"/files", nil, false)
		if problem != nil {
			return content, problem
		}
		var nodes []writeNode
		if !bytes.HasPrefix(bytes.TrimSpace(data), []byte("[")) || json.Unmarshal(data, &nodes) != nil {
			return content, protocolMismatch(200)
		}
		var walk func([]writeNode, int) bool
		walk = func(nodes []writeNode, depth int) bool {
			if depth > 32 {
				return false
			}
			for _, node := range nodes {
				if !safeFilePath(node.Path) || (node.Type != "directory" && node.Type != "file") {
					return false
				}
				if node.Type == "directory" {
					if !walk(node.Children, depth+1) {
						return false
					}
					continue
				}
				name := node.Name
				if name == "" {
					name = path.Base(node.Path)
				}
				content.Children = append(content.Children, Resource{ID: id, Title: name, Kind: "file", Path: node.Path, URL: p.webEndpoint("/projects/"+id, nil)})
			}
			return true
		}
		if !walk(nodes, 0) {
			return Content{Children: []Resource{}}, protocolMismatch(200)
		}
		summary, _ := truncateText(selected.Description)
		content.Resource = Resource{ID: id, Title: *selected.Name, Kind: "project", URL: p.webEndpoint("/projects/"+id, nil), Summary: summary}
		return content, nil
	}
	data, problem := c.get(ctx, p, "projects/"+id+"/file", url.Values{"path": {filePath}}, false)
	if problem != nil {
		return content, problem
	}
	var result struct {
		File    *writeNode `json:"file"`
		Content *string    `json:"content"`
	}
	if json.Unmarshal(data, &result) != nil || result.File == nil || result.File.Path != filePath || result.Content == nil {
		return content, protocolMismatch(200)
	}
	content.Resource = Resource{ID: id, Title: path.Base(filePath), Kind: "file", Path: filePath, URL: p.webEndpoint("/projects/"+id, nil)}
	content.Text, content.Truncated = truncateText(*result.Content)
	content.Version = versionString(result.File.Version)
	return content, nil
}
