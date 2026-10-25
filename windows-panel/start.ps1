param(
    [switch]$Install,
    [string]$PythonPath = "python",
    [string]$DependencyPath = "",
    # Exact FastTask page origin: HTTPS cloud domain, or loopback HTTP in local development.
    [string]$Origin = "http://127.0.0.1:10000",
    [int]$Port = 19743,
    [string]$StateDirectory = ""
)
$ErrorActionPreference = "Stop"
$PanelRoot = $PSScriptRoot
$PanelPython = Join-Path $PanelRoot ".venv\Scripts\python.exe"
if ($Install) {
    if (-not (Test-Path -LiteralPath $PanelPython)) {
        & $PythonPath -m venv (Join-Path $PanelRoot ".venv")
        if ($LASTEXITCODE -ne 0) { throw "Python 虚拟环境创建失败" }
    }
    & $PanelPython -m pip install -r (Join-Path $PanelRoot "requirements.txt")
    if ($LASTEXITCODE -ne 0) { throw "依赖安装失败，请检查网络" }
}
if (-not (Test-Path -LiteralPath $PanelPython)) { $PanelPython = $PythonPath }
if ($DependencyPath) {
    $env:PYTHONPATH = $DependencyPath + $(if ($env:PYTHONPATH) { ";" + $env:PYTHONPATH } else { "" })
}
$env:PYTHONUTF8 = "1"
$PanelArguments = @((Join-Path $PanelRoot "panel_helper.py"), "--origin", $Origin, "--port", "$Port")
if ($StateDirectory) { $PanelArguments += @("--state-dir", $StateDirectory) }
& $PanelPython @PanelArguments
exit $LASTEXITCODE
