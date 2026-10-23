param([Parameter(Mandatory)][string]$Session,[string]$Image='127.0.0.1:5000/uni-agent-search:0.2.4',[string]$Bind='127.0.0.1:8098:8080',[string]$ProxyURL='',[string]$DoHURL='')
$ErrorActionPreference='Stop'
$ProgressPreference='SilentlyContinue'
Start-Transcript -Path (Join-Path $PSScriptRoot 'recreate-wslc.log') -Force
try {
  wslc --session $Session system session run docker stop uni-agent-search
  if($LASTEXITCODE -ne 0){throw 'Container stop failed'}
  wslc --session $Session remove uni-agent-search
  if($LASTEXITCODE -ne 0){throw 'Container removal failed; data volume preserved'}
} finally { Stop-Transcript }
& (Join-Path $PSScriptRoot 'start-wslc.ps1') -Session $Session -Image $Image -Bind $Bind -ProxyURL $ProxyURL -DoHURL $DoHURL
