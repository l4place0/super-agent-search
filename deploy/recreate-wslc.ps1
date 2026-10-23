param(
  [Parameter(Mandatory)][string]$Session,
  [ValidateSet('uni-agent-search','super-agent-search')][string]$ContainerName='uni-agent-search',
  [string]$HealthURL='',
  [string]$PlanPath=(Join-Path $PSScriptRoot ('recovery-'+[guid]::NewGuid().ToString('N')+'.clixml'))
)
$ErrorActionPreference='Stop'
. (Join-Path $PSScriptRoot 'wslc-common.ps1')
if (Test-Path -LiteralPath $PlanPath) { throw 'Recovery plan already exists' }
$plan = Get-UasPlan $Session $ContainerName
$backup = 'sas-backup-'+[guid]::NewGuid().ToString('N')
# Re-register forwarding using the exact current image and inspected settings.
Invoke-UasUpdate $plan $plan.Image $backup $PlanPath $HealthURL
