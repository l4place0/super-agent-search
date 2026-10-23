param([Parameter(Mandatory)][string]$PlanPath,[switch]$ConfirmRestore)
$ErrorActionPreference='Stop'
. (Join-Path $PSScriptRoot 'wslc-common.ps1')
if (-not $ConfirmRestore) { throw 'Restoration replaces current data with the backup. Stop clients and pass -ConfirmRestore after reviewing the saved plan.' }
if ([Environment]::OSVersion.Platform -ne [PlatformID]::Win32NT) { throw 'Use the Windows account/machine that created this DPAPI-encrypted plan' }
$plan = Import-Clixml -LiteralPath $PlanPath
if ($plan.Schema -ne 1 -or -not $plan.Backup -or $plan.Name -notin @('uni-agent-search','super-agent-search')) { throw 'Invalid recovery plan' }
Restore-UasPlan $plan
Write-Output 'Original snapshot/configuration restored; verify readiness and identity before resuming clients.'
