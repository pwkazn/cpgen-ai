Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$required = @(
    'one foreground executor per run',
    'per-run process lock',
    'fixed pipeline',
    'no workflow-hosting service'
)
$forbidden = @(
    'execution\s+lease',
    'lease[ _-]?epoch',
    'owner[ _-]?epoch',
    'fencing([ _-]?(token|epoch))?',
    '\bPROBING\b',
    '\bQUIESCING\b',
    'startup[ _-]?janitor',
    'recovery[ _-]?intent',
    'observation[ _-]?(ticket|floor)'
)

$repoRoot = Split-Path -Parent $PSScriptRoot
$decisionPaths = @(
    'docs/adr/0006-lightweight-local-workflow.md',
    'docs/superpowers/specs/2026-08-31-lightweight-local-workflow-design.md'
)
$contractPaths = @(
    'ARCHITECTURE.md',
    'docs/implementation-plan.md',
    'docs/superpowers/specs/2026-08-31-phase1-mvp-design.md'
) + $decisionPaths
$summaryPaths = @('README.md', 'docs/README.md', 'TODO.md')
$fixedPaths = @(
    'ARCHITECTURE.md',
    'README.md',
    'docs/README.md',
    'docs/implementation-plan.md',
    'docs/traceability.md',
    'TODO.md'
)

$discoveredPaths = @(
    Get-ChildItem -LiteralPath (Join-Path $repoRoot 'docs/adr') -Filter '*.md' -File
    Get-ChildItem -LiteralPath (Join-Path $repoRoot 'docs/design') -Filter '*.md' -File
    Get-ChildItem -LiteralPath (Join-Path $repoRoot 'docs/superpowers/specs') -Filter '*.md' -File
) | ForEach-Object { [System.IO.Path]::GetRelativePath($repoRoot, $_.FullName).Replace('\', '/') }
$normativePaths = @($fixedPaths + $discoveredPaths | Sort-Object -Unique)
$failures = [System.Collections.Generic.List[string]]::new()

function Add-Failure {
    param(
        [string]$Path,
        [int]$Line,
        [string]$Phrase,
        [string]$Reason
    )

    $failures.Add("${Path}:${Line}: ${Reason}: '${Phrase}'")
}

function Get-Lines {
    param([string]$Path)

    $absolutePath = Join-Path $repoRoot $Path
    if (-not (Test-Path -LiteralPath $absolutePath -PathType Leaf)) {
        Add-Failure -Path $Path -Line 1 -Phrase $Path -Reason 'missing required file'
        return $null
    }

    return @(Get-Content -LiteralPath $absolutePath)
}

foreach ($path in $contractPaths) {
    $lines = Get-Lines -Path $path
    if ($null -eq $lines) {
        continue
    }

    $text = $lines -join "`n"
    foreach ($phrase in $required) {
        if ($text -notmatch [regex]::Escape($phrase)) {
            Add-Failure -Path $path -Line 1 -Phrase $phrase -Reason 'missing required phrase'
        }
    }
}

foreach ($path in $decisionPaths) {
    $lines = Get-Lines -Path $path
    if ($null -eq $lines) {
        continue
    }

    $statusLine = 0
    for ($index = 0; $index -lt $lines.Count; $index++) {
        if ($lines[$index] -cmatch '^Status:\s*Accepted\s*$') {
            $statusLine = $index + 1
            break
        }
    }
    if ($statusLine -eq 0) {
        Add-Failure -Path $path -Line 1 -Phrase 'Status: Accepted' -Reason 'missing accepted status'
    }
}

foreach ($path in $summaryPaths) {
    $lines = Get-Lines -Path $path
    if ($null -eq $lines) {
        continue
    }

    $found = $false
    for ($index = 0; $index -lt $lines.Count; $index++) {
        if ($lines[$index] -match '(?i)(Slice\s*1.*lightweight local workflow|lightweight local workflow.*Slice\s*1)') {
            $found = $true
            break
        }
    }
    if (-not $found) {
        Add-Failure -Path $path -Line 1 -Phrase 'Slice 1 lightweight local workflow' -Reason 'missing Slice 1 name'
    }
}

foreach ($path in $normativePaths) {
    if ($decisionPaths -contains $path) {
        continue
    }

    $lines = Get-Lines -Path $path
    if ($null -eq $lines) {
        continue
    }

    $isOlderAdr = $path -like 'docs/adr/*'
    $insideSupersededDesign = $false
    for ($index = 0; $index -lt $lines.Count; $index++) {
        $line = $lines[$index]
        if ($isOlderAdr -and $line -ceq '<!-- Superseded design: begin -->') {
            if ($insideSupersededDesign) {
                Add-Failure -Path $path -Line ($index + 1) -Phrase $line -Reason 'nested superseded-design delimiter'
            }
            $insideSupersededDesign = $true
            continue
        }
        if ($isOlderAdr -and $line -ceq '<!-- Superseded design: end -->') {
            if (-not $insideSupersededDesign) {
                Add-Failure -Path $path -Line ($index + 1) -Phrase $line -Reason 'unmatched superseded-design delimiter'
            }
            $insideSupersededDesign = $false
            continue
        }
        if ($insideSupersededDesign) {
            continue
        }

        foreach ($pattern in $forbidden) {
            $match = [regex]::Match($line, $pattern, [System.Text.RegularExpressions.RegexOptions]::IgnoreCase)
            if ($match.Success) {
                Add-Failure -Path $path -Line ($index + 1) -Phrase $match.Value -Reason "forbidden active architecture phrase matched by /$pattern/"
            }
        }
    }

    if ($insideSupersededDesign) {
        Add-Failure -Path $path -Line $lines.Count -Phrase '<!-- Superseded design: begin -->' -Reason 'unterminated superseded-design section'
    }
}

if ($failures.Count -gt 0) {
    foreach ($failure in $failures) {
        Write-Output $failure
    }
    Write-Output "Slice 1 architecture check failed with $($failures.Count) issue(s)."
    exit 1
}

Write-Output "Slice 1 architecture check passed for $($normativePaths.Count) normative file(s)."
