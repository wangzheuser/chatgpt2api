param(
    [switch] $NoInstall
)

$ErrorActionPreference = "Stop"

$root = $PSScriptRoot
$children = @()
$exitCode = 0
$stopping = $false

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

function Resolve-CommandPath {
    param([string] $Name)

    $command = Get-Command $Name -ErrorAction Stop
    if ($command.Source) { return $command.Source }
    if ($command.Path) { return $command.Path }
    return $Name
}

function Invoke-Checked {
    param(
        [string] $FilePath,
        [string[]] $ArgumentList,
        [string] $WorkingDirectory
    )

    $label = "$FilePath $($ArgumentList -join ' ')"
    Write-Host "> $label"
    $process = Start-Process `
        -FilePath (Resolve-CommandPath $FilePath) `
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
    if (Get-Command $Name -ErrorAction SilentlyContinue) {
        return
    }

    if ($NoInstall) {
        throw "$Name not found. Re-run without -NoInstall to install it automatically."
    }

    & $Install
    Update-CurrentPath

    if (-not (Get-Command $Name -ErrorAction SilentlyContinue)) {
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
    taskkill.exe /PID $Process.Id /T /F | Out-Null
}

function Start-DevProcess {
    param(
        [string] $Name,
        [string] $FilePath,
        [string[]] $ArgumentList,
        [string] $WorkingDirectory
    )

    $label = "$FilePath $($ArgumentList -join ' ')"
    Write-Host "Starting ${Name}: $label"
    $process = Start-Process `
        -FilePath (Resolve-CommandPath $FilePath) `
        -ArgumentList $ArgumentList `
        -WorkingDirectory $WorkingDirectory `
        -NoNewWindow `
        -PassThru

    $script:children += $process
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
    Ensure-Command "bun" { Install-Bun }

    Write-Host "Installing dependencies..."
    Invoke-Checked "uv" @("sync") $root # uv sync
    Invoke-Checked "bun" @("install") (Join-Path $root "web") # bun install

    Start-DevProcess "backend" "uv" @("run", "main.py") $root # uv run main.py
    Start-DevProcess "frontend" "bun" @("run", "dev") (Join-Path $root "web") # bun run dev

    Write-Host ""
    Write-Host "Dev servers started. Press Ctrl+C to stop all processes."

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
