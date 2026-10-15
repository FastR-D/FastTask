package integrationclient

import (
	"context"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
)

// The caller supplies the version already shown by a read. Updates preserve
// every byte outside an explicit unique replacement or append operation.
type DocumentWriteCommand struct {
	ProjectID   string `json:"project_id"`
	Path        string `json:"path"`
	Operation   string `json:"operation" enum:"replace,append,create"`
	BaseVersion int    `json:"base_version"`
	Before      string `json:"before,omitempty"`
	Text        string `json:"text"`
}
type DocumentChange struct {
	Before    string `json:"before"`
	After     string `json:"after"`
	Operation string `json:"operation"`
}
type DocumentWriteResult struct {
	Saved       bool           `json:"saved"`
	Verified    bool           `json:"verified"`
	Status      string         `json:"status"`
	Resource    Resource       `json:"resource"`
	BaseVersion int            `json:"base_version"`
	Version     string         `json:"version,omitempty"`
	Draft       string         `json:"draft"`
	Change      DocumentChange `json:"change"`
	Diagnostic  *Diagnostic    `json:"diagnostic,omitempty"`
}

func (c Client) WriteDocumentText(ctx context.Context, config Config, command DocumentWriteCommand) DocumentWriteResult {
	result := DocumentWriteResult{Status: "not_saved", Draft: command.Text, BaseVersion: command.BaseVersion, Change: DocumentChange{Before: command.Before, After: command.Text, Operation: command.Operation}, Resource: Resource{ID: command.ProjectID, Title: path.Base(command.Path), Path: command.Path, Kind: "file"}}
	p, problem := researchPrepared(config, "fastwrite")
	if problem != nil {
		result.Diagnostic = problem
		return result
	}
	if !safeID(command.ProjectID) || !safeFilePath(command.Path) || len(command.Text) > 24<<10 || len(command.Before) > 24<<10 || (command.Operation != "replace" && command.Operation != "append" && command.Operation != "create") || (command.Operation != "create" && command.BaseVersion < 1) || (command.Operation == "replace" && command.Before == "") || (command.Operation != "replace" && command.Before != "") || (command.Operation == "create" && command.BaseVersion != 0) {
		result.Diagnostic = diagnostic("INPUT_INVALID", "写入范围或版本无效", "请提供明确项目、相对路径、原版本，以及唯一替换原文或追加/新建范围。每段不超过 24 KiB。", "先读取目标正文和版本，再按用户要求指定修改范围。", 0, false)
		return result
	}
	if problem := c.checkWritingIdentity(ctx, p); problem != nil {
		result.Diagnostic = problem
		return result
	}
	projects, problem := c.writingProjects(ctx, p)
	if problem != nil {
		result.Diagnostic = problem
		return result
	}
	found := false
	for _, project := range projects {
		id, _ := resourceID(project.ID)
		if id == command.ProjectID {
			found = true
		}
	}
	if !found {
		result.Diagnostic = diagnostic("RESOURCE_NOT_FOUND", "当前账号没有该写作项目", "不会对未出现在本人授权列表中的项目写入。", "先选择本人可访问的项目。", 404, false)
		return result
	}
	result.Resource.URL = p.webEndpoint("/projects/"+command.ProjectID, url.Values{"path": {command.Path}})
	current, problem := c.readWrite(ctx, p, command.ProjectID, command.Path)
	var draft string
	if command.Operation == "create" {
		if problem == nil {
			result.Diagnostic = diagnostic("FILE_EXISTS", "目标文件已经存在", "没有覆盖当前正文。", "读取文件并明确修改范围后再保存。", 409, false)
			return result
		}
		if problem.HTTPStatus != 404 {
			result.Diagnostic = problem
			return result
		}
		draft = command.Text
	} else {
		if problem != nil {
			result.Diagnostic = problem
			return result
		}
		if current.Truncated {
			result.Diagnostic = diagnostic("CONTENT_TOO_LARGE", "文件超过安全修改上限", "读取的正文只是节选，无法保护未读取的内容。", "请在 FastWrite 内编辑此文件，或选择较小的独立文件。", 0, false)
			return result
		}
		if current.Version != strconv.Itoa(command.BaseVersion) {
			result.Status = "version_conflict"
			result.Diagnostic = researchStatus(409, []byte(`{"code":"version_conflict"}`))
			return result
		}
		if command.Operation == "replace" {
			if strings.Count(current.Text, command.Before) != 1 {
				result.Diagnostic = diagnostic("RANGE_AMBIGUOUS", "替换原文没有唯一匹配", "没有修改正文；零处或多处匹配都不能推测用户要替换的范围。", "重新读取正文，提供只出现一次的完整原文片段。", 409, false)
				return result
			}
			draft = strings.Replace(current.Text, command.Before, command.Text, 1)
		} else {
			draft = current.Text + command.Text
		}
	}
	method, route, query := http.MethodPut, "projects/"+command.ProjectID+"/file", url.Values{"path": {command.Path}}
	body := map[string]any{"content": draft, "baseVersion": command.BaseVersion}
	if command.Operation == "create" {
		method, route, query = http.MethodPost, "projects/"+command.ProjectID+"/files", nil
		body = map[string]any{"path": command.Path, "content": draft}
	}
	_, problem = c.researchJSON(ctx, p, method, route, query, body, nil, c.timeout())
	if problem != nil && (problem.HTTPStatus == 409 || problem.HTTPStatus == 401 || problem.HTTPStatus == 403 || problem.HTTPStatus == 400 || problem.HTTPStatus == 404 || problem.HTTPStatus == 415) {
		result.Status = "not_saved"
		if problem.HTTPStatus == 409 {
			result.Status = "version_conflict"
		}
		result.Diagnostic = problem
		return result
	}
	// A timeout may happen after the upstream committed. Never retry a mutation;
	// perform one bounded read independently of the timed-out request instead.
	verified, readProblem := c.readWrite(context.WithoutCancel(ctx), p, command.ProjectID, command.Path)
	version, versionErr := strconv.Atoi(verified.Version)
	if readProblem == nil && !verified.Truncated && verified.Text == draft && versionErr == nil && version > command.BaseVersion {
		result.Saved, result.Verified, result.Status, result.Version = true, true, "saved_and_verified", verified.Version
		if problem != nil {
			result.Status = "saved_verified_after_uncertain_response"
		}
		return result
	}
	result.Status = "save_unconfirmed"
	result.Diagnostic = diagnostic("SAVE_UNCONFIRMED", "暂未核实正文保存结果", "保存响应或读回核对未完成，不能宣称写入成功；草稿与修改范围已保留，没有重复写入。", "读取目标文件核对现状后决定下一步；不要直接重复创建或刷新版本覆盖。", 0, false)
	return result
}
