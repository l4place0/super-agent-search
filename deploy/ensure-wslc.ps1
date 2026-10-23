param(
  [Parameter(Mandatory)][string]$Session,
  [ValidateSet('uni-agent-search','super-agent-search')][string]$ContainerName='uni-agent-search',
  [string]$HealthURL=''
)
$ErrorActionPreference='Stop'
. (Join-Path $PSScriptRoot 'wslc-common.ps1')
$plan = Get-UasPlan $Session $ContainerName
# A transient health/forwarding failure is not authorization to destroy state.
Ensure-UasPlan $plan $HealthURL
