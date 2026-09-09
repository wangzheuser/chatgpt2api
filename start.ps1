param(
    [switch] $NoInstall,
    [string] $FrontendPort = "3000",
    [string] $BackendPort = "8000"
)

$ErrorActionPreference = "Stop"

$root = $PSScriptRoot
$children = @()
$exitCode = 0
$stopping = $false

# 本机 PowerShell 的 & 直接调用 node.exe 会静默失败，且外部注入的 NODE_OPTIONS
# （如 node-safe-delete-shim）会干扰 next dev 的文件清理，启动子进程前统一清除。
$env:NODE_OPTIONS = $null

function Update-CurrentPath {
    $paths = @()
    $machinePath = [Environment]::GetEnvironmentVariable("Path", [EnvironmentVariableTarget]::Machine)
    $userPath = [Environment]::GetEnvironmentVariable("Path", [EnvironmentVariableTarget]::User)

    if ($machinePath) { $paths += $machinePath -split ";" }
    if ($userPath) { $paths += $userPath -split ";" }
    if ($env:Path) { $paths += $env:Path -split ";" }

    $paths += Join-Path $env:USERPROFILE ".local\bin"
    $paths += Join-Path $env:USERPROFILE ".bun\bin"

    $env:Path = ($paths | Where-Object { $_ } | ForEach-Object { $_.Trim() } | Select-Object -Unique) -join ";"
}

function Test-Command {
    param([string] $Name)

    # 本机 PATH 存在异常条目，裸命令名（如 uv / node）可能解析失败，需带扩展名重试
    foreach ($candidate in @($Name, "$Name.exe", "$Name.cmd", "$Name.bat")) {
        if (Get-Command $candidate -ErrorAction SilentlyContinue) {
            return $true
        }
    }
    return $false
}

function Resolve-CommandPath {
    param([string] $Name)

    if (Test-Path $Name) {
        return $Name
    }

    foreach ($candidate in @($Name, "$Name.exe", "$Name.cmd", "$Name.bat")) {
        $command = Get-Command $candidate -ErrorAction SilentlyContinue
        if ($command) {
            if ($command.Source) { return $command.Source }
            if ($command.Path) { return $command.Path }
        }
    }

    throw "Command '$Name' not found."
}

function Resolve-Executable {
    param([string] $FilePath)

    if (Test-Path $FilePath) { return $FilePath }
    return Resolve-CommandPath $FilePath
}

function Invoke-Checked {
    param(
        [string] $FilePath,
        [string[]] $ArgumentList,
        [string] $WorkingDirectory
    )

    $executable = Resolve-Executable $FilePath
    $label = "$FilePath $($ArgumentList -join ' ')"
    Write-Host "> $label"
    $process = Start-Process `
        -FilePath $executable `
        -ArgumentList $ArgumentList `
        -WorkingDirectory $WorkingDirectory `
        -NoNewWindow `
        -Wait `
        -PassThru

    if ($process.ExitCode -ne 0) {
        throw "$label failed with exit code $($process.ExitCode)"
    }
}

function Ensure-Command {
    param(
        [string] $Name,
        [scriptblock] $Install
    )

    Update-CurrentPath
    if (Test-Command $Name) {
        return
    }

    if ($NoInstall) {
        throw "$Name not found. Re-run without -NoInstall to install it automatically."
    }

    & $Install
    Update-CurrentPath

    if (-not (Test-Command $Name)) {
        throw "$Name install finished, but command is still not available in PATH."
    }
}

function Install-Uv {
    Write-Host "uv not found. Installing uv..."
    Invoke-Checked "powershell" @("-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", "irm https://astral.sh/uv/install.ps1 | iex") $root
}

function Install-Bun {
    Write-Host "bun not found. Installing bun..."
    Invoke-Checked "powershell" @("-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", "irm bun.sh/install.ps1 | iex") $root
}

function Stop-ProcessTree {
    param([System.Diagnostics.Process] $Process)

    if ($null -eq $Process) {
        return
    }

    $live = Get-Process -Id $Process.Id -ErrorAction SilentlyContinue
    if ($null -eq $live) {
        return
    }

    Write-Host "Stopping PID $($Process.Id)..."
    # 用 CIM 递归终止子进程树：taskkill.exe 在部分 PowerShell 宿主（含受限环境）
    # 中无法在管道里调用（CantActivateDocumentInPipeline），会导致清理失败、端口残留。
    $children = Get-CimInstance Win32_Process -Filter "ParentProcessId=$($Process.Id)" -ErrorAction SilentlyContinue
    foreach ($child in $children) {
        Stop-Process -Id $child.ProcessId -Force -Confirm:$false -ErrorAction SilentlyContinue
    }
    Stop-Process -Id $Process.Id -Force -Confirm:$false -ErrorAction SilentlyContinue
}

function Start-DevProcess {
    param(
        [string] $Name,
        [string] $FilePath,
        [string[]] $ArgumentList,
        [string] $WorkingDirectory
    )

    $executable = Resolve-Executable $FilePath
    $label = "$FilePath $($ArgumentList -join ' ')"
    Write-Host "Starting ${Name}: $label"
    $process = Start-Process `
        -FilePath $executable `
        -ArgumentList $ArgumentList `
        -WorkingDirectory $WorkingDirectory `
        -NoNewWindow `
        -PassThru

    $script:children += $process
}

function Test-PortOpen {
    param([int] $Port)

    $client = New-Object System.Net.Sockets.TcpClient
    try {
        $task = $client.ConnectAsync("127.0.0.1", $Port)
        if ($task.Wait(1000) -and $client.Connected) { return $true }
        return $false
    } catch {
        return $false
    } finally {
        $client.Dispose()
    }
}

function Wait-ForPort {
    param(
        [int] $Port,
        [string] $Name,
        [int] $TimeoutSec = 90
    )

    Write-Host "Waiting for ${Name} on port ${Port}..."
    for ($i = 0; $i -lt $TimeoutSec; $i++) {
        if (Test-PortOpen $Port) {
            Write-Host "${Name} is ready."
            return $true
        }
        Start-Sleep -Seconds 1
    }

    Write-Host "${Name} did not open port ${Port} within ${TimeoutSec}s."
    return $false
}

$cancelHandler = [System.ConsoleCancelEventHandler] {
    param($sender, $eventArgs)
    $eventArgs.Cancel = $true
    $script:exitCode = 130
    $script:stopping = $true
}

try {
    [Console]::add_CancelKeyPress($cancelHandler)

    Ensure-Command "uv" { Install-Uv }

    Write-Host "Installing backend dependencies..."
    Invoke-Checked "uv" @("sync") $root # uv sync

    $webDir = Join-Path $root "web"

    # 前端包管理器：优先 bun，缺失时回退 npm（本机未安装 bun）
    $useBun = Test-Command "bun"
    if (-not $useBun) {
        if (Test-Command "node") {
            Write-Host "bun not found. Falling back to npm (node)."
        } else {
            Ensure-Command "bun" { Install-Bun }
            $useBun = Test-Command "bun"
        }
    }

    Write-Host "Installing frontend dependencies..."
    $installFailed = $false
    try {
        if ($useBun) {
            Invoke-Checked "bun" @("install") $webDir # bun install
        } else {
            # 绕开 npm.cmd（其内部依赖 PATH 中的 node，在部分环境下会静默失败）
            $nodePath = Resolve-CommandPath "node"
            $npmCliJs = Join-Path (Split-Path (Resolve-CommandPath "npm") -Parent) "node_modules\npm\bin\npm-cli.js"
            if (-not (Test-Path $npmCliJs)) {
                throw "npm-cli.js not found at $npmCliJs"
            }
            Invoke-Checked $nodePath @($npmCliJs, "install", "--no-audit", "--no-fund") $webDir
        }
    } catch {
        # 依赖安装失败不一定致命：node_modules 已存在时可以直接复用现有依赖启动
        $installFailed = $true
        Write-Host "Warning: frontend dependency install failed: $($_.Exception.Message)"
    }

    if ($installFailed) {
        if (Test-Path (Join-Path $webDir "node_modules\next")) {
            Write-Host "Reusing existing node_modules."
        } else {
            throw "Frontend dependencies are missing and install failed. Please run 'npm install' in web/ manually."
        }
    }

    Start-DevProcess "backend" "uv" @("run", "main.py") $root # uv run main.py
    Wait-ForPort -Port ([int]$BackendPort) -Name "backend" | Out-Null

    if ($useBun) {
        Start-DevProcess "frontend" "bun" @("run", "dev", "--port", $FrontendPort) $webDir
    } else {
        $nextBin = Join-Path $webDir "node_modules\next\dist\bin\next"
        if (-not (Test-Path $nextBin)) {
            throw "next entry not found at $nextBin"
        }
        $nodePath = Resolve-CommandPath "node"
        Start-DevProcess "frontend" $nodePath @($nextBin, "dev", "--webpack", "-H", "0.0.0.0", "--port", $FrontendPort) $webDir
    }

    Write-Host ""
    Write-Host "Dev servers started. Press Ctrl+C to stop all processes."
    Write-Host "backend : http://localhost:$BackendPort"
    Write-Host "frontend: http://localhost:$FrontendPort"

    while (-not $stopping) {
        foreach ($process in $children) {
            $process.Refresh()
            if ($process.HasExited) {
                $exitCode = $process.ExitCode
                Write-Host "PID $($process.Id) exited with code $exitCode. Stopping the rest..."
                $stopping = $true
                break
            }
        }

        if (-not $stopping) {
            Start-Sleep -Milliseconds 500
        }
    }
}
finally {
    [Console]::remove_CancelKeyPress($cancelHandler)

    for ($i = $children.Count - 1; $i -ge 0; $i--) {
        Stop-ProcessTree $children[$i]
    }
}

exit $exitCode