package application

import (
	"context"

	"github.com/FastR-D/FastTask/internal/agent"
)

// Navigation uses the user's configured web address, never target-system
// credentials or an external API probe. A prepared link is not a saved result.
func workspaceTool(app *App) Tool {
	return funcTool{def: agent.ToolDefinition{
		Name:        "open_tool_workspace",
		Description: "Prepare a direct webpage link for FastWrite, FastRead, FastNews, FastPPT or FastLab. Use this first when the user wants to use/open/jump to a module, including writing or reading there. No target API credential, health check or content request is needed. Return the actual link and let the user open the corresponding workspace; preparing a link does not prove a document was saved, a paper read, or a task executed.",
		Parameters:  objectSchema(map[string]any{"system": stringProp("Module webpage to open.", IntegrationSystems...)}, "system"),
	}, fn: func(ctx context.Context, tc ToolContext, args map[string]any) (ToolResult, error) {
		system := stringArg(args, "system")
		if !validIntegrationSystem(system) {
			return toolError("请选择已支持的模块网页。"), nil
		}
		connections, err := app.IntegrationConnections(ctx, tc.UserID, false)
		if err != nil {
			return toolError("暂时无法读取模块网页地址，请稍后重试。"), nil
		}
		labels := map[string]string{"fastwrite": "FastWrite", "fastread": "FastRead", "fastnews": "FastNews", "fastppt": "FastPPT", "fastlab": "FastLab"}
		for _, connection := range connections {
			if connection.System != system {
				continue
			}
			if connection.WebURL == "" || validateIntegrationURL(connection.WebURL) != nil {
				return toolError("%s 的网页地址尚未配置或无效，请联系管理员配置模块网页地址；打开网页不需要复制授权令牌。", labels[system]), nil
			}
			return ToolResult{Result: map[string]any{"action": "open_workspace", "system": system, "url": connection.WebURL, "label": labels[system], "status": "ready_to_open", "note": "在对应模块网页继续操作；登录由该网页自己的正常登录流程处理。入口已准备，不代表正文已保存、全文已读或实验已执行。"}}, nil
		}
		return toolError("未找到该模块的网页地址。"), nil
	}}
}
