param(
  [Parameter(Mandatory)][string]$Session,
  [string]$Image='127.0.0.1:5000/uni-agent-search:0.2.4',
  [string[]]$Bind=@('127.0.0.1:8098:8080'),
  [string]$DataVolume='sas-data',
  [string]$ProxyURL='',
  [string]$DoHURL=''
)
$ErrorActionPreference='Stop'
$ProgressPreference='SilentlyContinue'
. (Join-Path $PSScriptRoot 'wslc-common.ps1')
if ($DataVolume -notmatch '^[a-zA-Z0-9][a-zA-Z0-9_.-]*$') { throw 'Invalid volume name' }
foreach ($binding in $Bind) { if ($binding -notmatch '^(\[[0-9a-fA-F:]+\]|[0-9.]+):\d+:8080$') { throw 'Invalid bind mapping' } }
$environment = @('SAS_DATA_DIR=/data','SAS_LISTEN=:8080')
if ($ProxyURL) { $environment += @("HTTP_PROXY=$ProxyURL","HTTPS_PROXY=$ProxyURL",'NO_PROXY=localhost,127.0.0.1') }
if ($DoHURL) { $environment += "SAS_DOH_URL=$DoHURL" }
$secure = @($environment | ForEach-Object { ConvertTo-SecureString $_ -AsPlainText -Force })
$plan = [pscustomobject]@{
  Session=$Session; Name='uni-agent-search'; Image=$Image; MountType='volume'; Source=$DataVolume
  Binds=$Bind; Environment=$secure; Labels=@(); Memory=536870912; NanoCPUs=2000000000
  Restart='unless-stopped'; RetryCount=0; AppLabel='uni-agent-search'; LogDriver='json-file'; TransactionID=[guid]::NewGuid().ToString('N')
}
# Initial deployment only: never stop/remove an existing instance. No transcript
# is created, as native arguments may contain authenticated proxy URLs.
Start-UasPlan $plan $Image
Write-Output 'Initial deployment started; verify /readyz and preserve the data volume.'
