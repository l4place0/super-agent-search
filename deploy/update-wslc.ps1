param(
  [Parameter(Mandatory)][string]$Session,
  [string]$Image='127.0.0.1:5000/super-agent-search:0.2.4',
  [string]$BackupVolume=('sas-backup-'+(Get-Date -Format yyyyMMddHHmmss)),
  [string]$Bind='127.0.0.1:8098:8080',
  [string]$HealthURL='http://127.0.0.1:8098/healthz',
  [string]$ProxyURL='',
  [string]$DoHURL=''
)
$ErrorActionPreference='Stop'
$ProgressPreference='SilentlyContinue'
$old = wslc --session $Session inspect --format json super-agent-search | ConvertFrom-Json
if($LASTEXITCODE -ne 0 -or $old[0].Labels.app -ne 'super-agent-search'){throw 'Expected application container not found'}
$oldImage=$old[0].Config.Image
Write-Output "Backing up sas-data to $BackupVolume; previous image: $oldImage"
wslc --session $Session stop super-agent-search
if($LASTEXITCODE -ne 0){throw 'Container stop failed'}
wslc --session $Session system session run docker volume create $BackupVolume
if($LASTEXITCODE -ne 0){throw 'Backup volume creation failed'}
wslc --session $Session system session run docker run --rm --user 0:0 --entrypoint /bin/cp --mount type=volume,source=sas-data,target=/source,readonly --mount "type=volume,source=$BackupVolume,target=/backup" $oldImage -a /source/. /backup/
if($LASTEXITCODE -ne 0){throw 'Data backup failed; original volume preserved'}
wslc --session $Session remove super-agent-search
if($LASTEXITCODE -ne 0){throw 'Container removal failed'}
& (Join-Path $PSScriptRoot 'start-wslc.ps1') -Session $Session -Image $Image -Bind $Bind -ProxyURL $ProxyURL -DoHURL $DoHURL
$ready=$false
for($attempt=0;$attempt -lt 15;$attempt++){
  try {
    $request=[Net.WebRequest]::Create($HealthURL)
    $request.Proxy=$null; $request.Timeout=2000
    $response=$request.GetResponse(); $response.Close(); $ready=$true; break
  } catch { Start-Sleep -Seconds 2 }
}
if(-not $ready){throw "New service health check failed; backup retained: $BackupVolume; previous image: $oldImage"}
Write-Output "Upgrade healthy. Image: $Image. Backup: $BackupVolume."
