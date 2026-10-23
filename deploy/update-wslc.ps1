param(
  [Parameter(Mandatory)][string]$Session,
  [ValidateSet('uni-agent-search','super-agent-search')][string]$ContainerName='uni-agent-search',
  [string]$Image='127.0.0.1:5000/uni-agent-search:0.2.4',
  [string]$BackupVolume=('sas-backup-'+(Get-Date -Format yyyyMMddHHmmss)+'-'+[guid]::NewGuid().ToString('N').Substring(0,8)),
  [string]$HealthURL='',
  [string]$PlanPath=(Join-Path $PSScriptRoot ('recovery-'+[guid]::NewGuid().ToString('N')+'.clixml'))
)
$ErrorActionPreference='Stop'
$ProgressPreference='SilentlyContinue'
. (Join-Path $PSScriptRoot 'wslc-common.ps1')
if (Test-Path -LiteralPath $PlanPath) { throw 'Recovery plan already exists; choose a new path' }
$plan = Get-UasPlan $Session $ContainerName
Invoke-UasUpdate $plan $Image $BackupVolume $PlanPath $HealthURL
