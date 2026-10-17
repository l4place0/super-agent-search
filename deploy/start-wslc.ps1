param([Parameter(Mandatory)][string]$Session,[string]$Image='127.0.0.1:5000/super-agent-search:0.2.4',[string]$Bind='127.0.0.1:8098:8080',[string]$DataVolume='sas-data',[string]$ProxyURL='',[string]$DoHURL='')
$ErrorActionPreference='Stop'
$ProgressPreference='SilentlyContinue'
Start-Transcript -Path (Join-Path $PSScriptRoot 'start-wslc.log') -Force
try {
  # This launcher is for an initial deployment. Never replace an existing service.
  # Windows PowerShell can turn native stderr warnings into terminating errors
  # when a caller redirects streams. Check the actual process exit code instead.
  $ErrorActionPreference='Continue'
  $networkArgs=@()
  if($ProxyURL){$networkArgs+=@('-e',"HTTP_PROXY=$ProxyURL",'-e',"HTTPS_PROXY=$ProxyURL",'-e','NO_PROXY=localhost,127.0.0.1')}
  if($DoHURL){$networkArgs+=@('-e',"SAS_DOH_URL=$DoHURL")}
  wslc --session $Session run --pull never -d --name super-agent-search -p $Bind -v "${DataVolume}:/data" --memory 512m --cpus 2 @networkArgs --label app=super-agent-search $Image
  $startExit=$LASTEXITCODE
  $ErrorActionPreference='Stop'
  if($startExit -ne 0){throw 'WSLC start failed'}
  wslc --session $Session system session run docker update --restart unless-stopped super-agent-search
  if($LASTEXITCODE -ne 0){throw 'Restart policy update failed'}
} finally { Stop-Transcript }
