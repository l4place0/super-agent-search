param([Parameter(Mandatory)][string]$Session,[string]$Image='127.0.0.1:5000/super-agent-search:0.2.4',[string]$Bind='127.0.0.1:8098:8080',[string]$HealthURL='http://127.0.0.1:8098/healthz',[string]$ProxyURL='',[string]$DoHURL='')
$ErrorActionPreference='Stop'
$ProgressPreference='SilentlyContinue'
try {
  $request=[Net.WebRequest]::Create($HealthURL)
  $request.Proxy=$null
  $request.Timeout=3000
  $response=$request.GetResponse()
  $response.Close()
  exit 0
} catch { }
$container = wslc --session $Session inspect --format json super-agent-search | ConvertFrom-Json
if ($LASTEXITCODE -ne 0) { throw 'Container inspection failed' }
if (-not $container[0].State.Running) {
  wslc --session $Session start super-agent-search
  if ($LASTEXITCODE -ne 0) { throw 'Container start failed' }
} else {
  # WSLC preview can lose Windows forwarding after restart while Docker reports
  # the container running. Re-register it by recreating only this container.
  & (Join-Path $PSScriptRoot 'recreate-wslc.ps1') -Session $Session -Image $Image -Bind $Bind -ProxyURL $ProxyURL -DoHURL $DoHURL
}
