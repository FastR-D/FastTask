param(
    [switch]$Remove,
    [string]$PythonPath = "python",
    [string]$DependencyPath = "",
    # Must match start.ps1 and the page opened in the browser, including any non-default port.
    [string]$Origin = "http://127.0.0.1:10000",
    [ValidateRange(1, 65535)]
    [int]$Port = 19743,
    [string]$StateDirectory = ""
)
$ErrorActionPreference = "Stop"
$PanelStartup = [Environment]::GetFolderPath("Startup")
$PanelShortcut = Join-Path $PanelStartup "FastTask Panel.lnk"
if ($Remove) {
    if (Test-Path -LiteralPath $PanelShortcut) { Remove-Item -LiteralPath $PanelShortcut }
    Write-Host "已移除当前用户登录自启。已运行的本地助手仍可在任务管理器中结束。"
    exit
}
# Install only for the current user's logon; no administrator or system service required.
$PanelScript = Join-Path $PSScriptRoot "start.ps1"
foreach ($PanelValue in @($PanelScript, $PythonPath, $DependencyPath, $Origin, $StateDirectory)) {
    if ($PanelValue.Contains('"')) { throw "参数不能含双引号" }
}
$PanelShell = New-Object -ComObject WScript.Shell
$PanelLink = $PanelShell.CreateShortcut($PanelShortcut)
$PanelLink.TargetPath = "$env:SystemRoot\System32\WindowsPowerShell\v1.0\powershell.exe"
$PanelLink.Arguments = '-NoProfile -WindowStyle Hidden -ExecutionPolicy Bypass -File "' + $PanelScript + '" -PythonPath "' + $PythonPath + '" -Origin "' + $Origin + '"'
$PanelLink.Arguments += ' -Port ' + $Port
if ($DependencyPath) { $PanelLink.Arguments += ' -DependencyPath "' + $DependencyPath + '"' }
if ($StateDirectory) { $PanelLink.Arguments += ' -StateDirectory "' + $StateDirectory + '"' }
$PanelLink.WorkingDirectory = $PSScriptRoot
$PanelLink.WindowStyle = 7
$PanelLink.Description = "FastTask Windows 工牌助手：检测今日任务变化并自动更新墨水屏"
$PanelLink.Save()
Write-Host "已安装当前用户登录自启。请使用相同 Origin 运行 start.ps1，再在 FastTask 页面连接设备，自动同步任务变化。"
