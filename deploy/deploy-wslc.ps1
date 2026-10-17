#requires -Version 7.0
param(
  [Parameter(Mandatory)][string]$RemoteHost,
  [Parameter(Mandatory)][string]$Session,
  [string]$RemoteDirectory = 'C:\containers\super-agent-search',
  [int]$Port = 8098,
  [string]$BindAddress = '127.0.0.1',
  [string]$Tag = '0.2.4',
  [string]$ImageArchive = ''
)
$ErrorActionPreference = 'Stop'
if ($Session -notmatch '^[a-zA-Z0-9_-]+$' -or $Tag -notmatch '^[a-zA-Z0-9_.-]+$' -or $BindAddress -notmatch '^[0-9.]+$') { throw 'Invalid deployment argument' }
$root = Split-Path -Parent $PSScriptRoot
if ($ImageArchive -eq '') { $ImageArchive = Join-Path $root "dist/sas-image-$Tag.tar" }
if (-not (Test-Path -LiteralPath $ImageArchive)) { throw 'Build the versioned image archive first; see deploy/README.md' }
$archive = Join-Path ([IO.Path]::GetTempPath()) ('sas-source-'+[guid]::NewGuid().ToString('N')+'.tar.gz')
try {
  & tar --exclude=verification*.md --exclude=*.log -czf $archive -C $root go.mod go.sum Dockerfile Dockerfile.prebuilt cmd internal skills deploy README.md LICENSE compose.yaml .dockerignore .gitignore .gitattributes
  if ($LASTEXITCODE -ne 0) { throw 'Source packaging failed' }
  $remoteScript = @'
$ErrorActionPreference='Stop'
$ProgressPreference='SilentlyContinue'
$directory='__DIRECTORY__'
$session='__SESSION__'
$tag='__TAG__'
New-Item -ItemType Directory -Force -Path $directory | Out-Null
if (Test-Path "$directory\source") { throw 'Source staging directory already exists; choose a new release directory' }
New-Item -ItemType Directory -Path "$directory\source" | Out-Null
tar -xzf "$env:USERPROFILE\sas-source-upload.tar.gz" -C "$directory\source"
if ($LASTEXITCODE -ne 0) { throw 'Source extraction failed' }
cmd /d /c "type `"$env:USERPROFILE\sas-image.tar`" | wslc --session $session system session run docker load"
if ($LASTEXITCODE -ne 0) { throw 'Image import failed' }
wslc --session $session system session run docker tag "super-agent-search:$tag" "127.0.0.1:5000/super-agent-search:$tag"
if ($LASTEXITCODE -ne 0) { throw 'Image tagging failed' }
wslc --session $session system session run docker push "127.0.0.1:5000/super-agent-search:$tag"
if ($LASTEXITCODE -ne 0) { throw 'Registry push failed' }
wslc --session $session system session run docker volume create sas-data
if ($LASTEXITCODE -ne 0) { throw 'Volume creation failed' }
$taskName='SuperAgentSearch-Deploy-'+[guid]::NewGuid().ToString('N')
$user=[System.Security.Principal.WindowsIdentity]::GetCurrent().Name
$principal=New-ScheduledTaskPrincipal -UserId $user -LogonType Interactive -RunLevel Limited
$arguments="-NoProfile -WindowStyle Hidden -File `"$directory\source\deploy\start-wslc.ps1`" -Session $session -Image 127.0.0.1:5000/super-agent-search:$tag -Bind __BIND__:__PORT__:8080"
$action=New-ScheduledTaskAction -Execute 'powershell.exe' -Argument $arguments
Register-ScheduledTask -TaskName $taskName -Action $action -Principal $principal | Out-Null
Start-ScheduledTask -TaskName $taskName
Write-Output "Deployment dispatched: $taskName. Check $directory\source\deploy\start-wslc.log and the /healthz endpoint."
'@
  $remoteScript = $remoteScript.Replace('__DIRECTORY__',$RemoteDirectory.Replace("'","''")).Replace('__SESSION__',$Session).Replace('__TAG__',$Tag).Replace('__BIND__',$BindAddress).Replace('__PORT__',[string]$Port)
  & scp -o BatchMode=yes $archive "${RemoteHost}:sas-source-upload.tar.gz"
  if ($LASTEXITCODE -ne 0) { throw 'Source upload failed' }
  & scp -o BatchMode=yes $ImageArchive "${RemoteHost}:sas-image.tar"
  if ($LASTEXITCODE -ne 0) { throw 'Image upload failed' }
  $encoded = [Convert]::ToBase64String([Text.Encoding]::Unicode.GetBytes($remoteScript))
  & ssh -o BatchMode=yes $RemoteHost powershell -NoProfile -EncodedCommand $encoded
  if ($LASTEXITCODE -ne 0) { throw 'Remote deployment failed' }
} finally { Remove-Item -LiteralPath $archive -ErrorAction SilentlyContinue }
