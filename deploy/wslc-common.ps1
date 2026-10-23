# Shared, fail-closed deployment operations. Never print inspect/env contents.
function Invoke-UasWSLC {
  param([string]$Session,[string[]]$Arguments)
  if (-not (Get-Command wslc -ErrorAction SilentlyContinue)) { throw 'WSLC executable not found' }
  $preference = $ErrorActionPreference
  try {
    # Native stderr may echo argv, including authenticated proxy URLs. Keep
    # diagnostics generic and inspect state locally instead of leaking them.
    $ErrorActionPreference = 'Continue'
    $output = & wslc --session $Session @Arguments 2>$null
    $code = $LASTEXITCODE
  } finally { $ErrorActionPreference = $preference }
  if ($code -ne 0) { throw 'WSLC operation failed (inspect local container state before retrying)' }
  return $output
}
function Invoke-UasDocker {
  param([string]$Session,[string[]]$Arguments)
  Invoke-UasWSLC $Session (@('system','session','run','docker') + $Arguments)
}
function Get-UasContainer {
  param([string]$Session,[string]$Name)
  $items = @(Invoke-UasDocker $Session @('inspect',$Name) | ConvertFrom-Json)
  if ($items.Count -ne 1) { throw 'Expected exactly one inspected container' }
  return $items[0]
}
function Get-UasPlan {
  param([string]$Session,[string]$Name)
  $c = Get-UasContainer $Session $Name
  if ($c.Config.Labels.app -notin @('uni-agent-search','super-agent-search')) { throw 'Refusing unrelated container' }
  $mounts = @($c.Mounts)
  if ($mounts.Count -ne 1 -or $mounts[0].Destination -ne '/data' -or -not $mounts[0].RW -or $mounts[0].Type -notin @('volume','bind')) { throw 'Only one writable /data volume or bind mount is supported' }
  $mount = $mounts[0]
  $source = $mount.Source
  if ($mount.Type -eq 'volume') { $source = $mount.Name }
  if (-not $source -or $source -match '[,\r\n]') { throw 'Unsupported data mount source' }
  if ($c.HostConfig.NetworkMode -notin @('default','bridge') -or $c.HostConfig.Privileged -or $c.HostConfig.ReadonlyRootfs -or $c.HostConfig.CapAdd -or $c.HostConfig.Devices -or @($c.HostConfig.Binds).Count -gt 1 -or $c.HostConfig.ExtraHosts -or $c.HostConfig.Dns) { throw 'Unsupported host/network configuration; use a manual migration' }
  if ($c.HostConfig.CpuPeriod -or $c.HostConfig.CpuQuota -or $c.HostConfig.CpusetCpus -or $c.HostConfig.CapDrop -or $c.HostConfig.SecurityOpt -or $c.HostConfig.GroupAdd -or $c.HostConfig.Ulimits -or $c.HostConfig.Tmpfs -or $c.HostConfig.PublishAllPorts -or $c.HostConfig.VolumesFrom -or $c.HostConfig.AutoRemove) { throw 'Unsupported resource/security settings' }
  if ($c.HostConfig.MemorySwap -notin @(0,($c.HostConfig.Memory * 2))) { throw 'Custom swap limits are not supported' }
  if ($c.HostConfig.LogConfig.Type -notin @('','json-file','local') -or @($c.HostConfig.LogConfig.Config.PSObject.Properties | Where-Object { $_ }).Count -gt 0) { throw 'Custom logging configuration is not supported' }
  if ($c.Config.Labels.'com.docker.compose.project' -or $c.HostConfig.IpcMode -notin @($null,'','private') -or $c.HostConfig.PidMode -or $c.HostConfig.CgroupParent -or $c.HostConfig.CpusetMems -or $c.HostConfig.MemoryReservation -or $c.HostConfig.OomKillDisable -or $c.HostConfig.Sysctls -or $c.HostConfig.Links -or $c.HostConfig.DnsSearch -or $c.HostConfig.DnsOptions) { throw 'Managed/custom host settings require a manual migration' }
  if ($c.HostConfig.CpuShares -or $c.HostConfig.CpuRealtimePeriod -or $c.HostConfig.CpuRealtimeRuntime -or $c.HostConfig.DeviceRequests -or $c.HostConfig.Init -or ($c.HostConfig.ShmSize -and $c.HostConfig.ShmSize -ne 67108864) -or $c.Config.StopSignal -notin @($null,'','SIGTERM')) { throw 'Unsupported process/resource settings' }
  $networks = @($c.NetworkSettings.Networks.PSObject.Properties | Where-Object { $_ })
  if ($networks.Count -gt 1 -or ($networks.Count -eq 1 -and ($networks[0].Name -ne 'bridge' -or $networks[0].Value.IPAMConfig -or $networks[0].Value.Aliases))) { throw 'Custom network attachment is not supported' }
  if ($c.Config.Hostname -and $c.Config.Hostname -ne $c.Id.Substring(0,12)) { throw 'Custom hostname is not supported' }
  if ($c.Config.User -notin @('10001:10001','app')) { throw 'Unsupported user configuration' }
  if (($c.Config.Entrypoint -join ' ') -ne '/usr/local/bin/sas-server' -or $c.Config.Cmd -or $c.Config.WorkingDir -notin @('','/')) { throw 'Custom command/workdir is not supported' }
  $bindings = @($c.HostConfig.PortBindings.PSObject.Properties)
  if ($bindings.Count -ne 1 -or $bindings[0].Name -ne '8080/tcp') { throw 'Only port 8080/tcp forwarding is supported' }
  $binds = @()
  foreach ($b in @($bindings[0].Value)) {
    if ($b.HostPort -notmatch '^\d+$' -or [int]$b.HostPort -lt 1 -or [int]$b.HostPort -gt 65535) { throw 'Invalid published port' }
    $hostIP = $b.HostIp
    if (-not $hostIP) { $hostIP = '0.0.0.0' }
    $ip = $null
    if (-not [Net.IPAddress]::TryParse($hostIP,[ref]$ip)) { throw 'Invalid bind IP' }
    if ($hostIP.Contains(':')) { $hostIP = "[$hostIP]" }
    $binds += "${hostIP}:$($b.HostPort):8080"
  }
  if ($binds.Count -lt 1) { throw 'Missing port mapping' }
  $allowed = @('PATH','SSL_CERT_FILE','TZ','HTTP_PROXY','HTTPS_PROXY','ALL_PROXY','NO_PROXY','http_proxy','https_proxy','all_proxy','no_proxy','SAS_DOH_URL','SAS_LISTEN','SAS_DATA_DIR','SAS_ADMIN_TOKEN','SAS_CLIENT_TOKEN')
  $environment = @()
  foreach ($entry in @($c.Config.Env)) {
    $key = ($entry -split '=',2)[0]
    if ($key -notin $allowed) { throw 'Unrecognized environment configuration; use a manual migration' }
    if ($key -eq 'SAS_LISTEN' -and $entry -ne 'SAS_LISTEN=:8080') { throw 'Custom container listen address is not supported' }
    if ($key -eq 'SAS_DATA_DIR' -and $entry -ne 'SAS_DATA_DIR=/data') { throw 'Custom container data path is not supported' }
    # Export-Clixml encrypts SecureString with Windows DPAPI (current user).
    $environment += ConvertTo-SecureString $entry -AsPlainText -Force
  }
  $labels = @()
  foreach ($p in $c.Config.Labels.PSObject.Properties) { if ($p.Name -notin @('app','uas.transaction')) { $labels += "$($p.Name)=$($p.Value)" } }
  $restart = $c.HostConfig.RestartPolicy.Name
  if ($restart -notin @('no','always','unless-stopped','on-failure')) { throw 'Unsupported restart policy' }
  [pscustomobject]@{
    Schema=1; Session=$Session; Name=$Name; OriginalID=$c.Id; Image=$c.Image; Running=[bool]$c.State.Running; AppLabel=$c.Config.Labels.app; LogDriver=$c.HostConfig.LogConfig.Type
    MountType=$mount.Type; Source=$source; Binds=$binds; Environment=$environment; Labels=$labels
    Memory=[long]$c.HostConfig.Memory; NanoCPUs=[long]$c.HostConfig.NanoCpus
    Restart=$restart; RetryCount=[int]$c.HostConfig.RestartPolicy.MaximumRetryCount
    Backup=''; ReplacementID=''; Replaced=$false; TransactionID=[guid]::NewGuid().ToString('N')
  }
}
function Get-UasMount {
  param($Plan,[string]$Target='/data',[switch]$ReadOnly)
  $value = "type=$($Plan.MountType),source=$($Plan.Source),target=$Target"
  if ($ReadOnly) { $value += ',readonly' }
  return $value
}
function Start-UasPlan {
  param($Plan,[string]$Image=$Plan.Image)
  $args = @('run','--pull','never','-d','--name',$Plan.Name,'--stop-timeout','20','--mount',(Get-UasMount $Plan),'--memory',([string]$Plan.Memory))
  if ($Plan.NanoCPUs -gt 0) { $args += @('--cpus',([string]([decimal]$Plan.NanoCPUs / 1000000000)).Replace(',','.')) }
  foreach ($bind in $Plan.Binds) { $args += @('-p',$bind) }
  foreach ($secret in $Plan.Environment) {
    $plain = (New-Object System.Net.NetworkCredential('', $secret)).Password
    $args += @('-e',$plain)
  }
  foreach ($label in $Plan.Labels) { $args += @('--label',$label) }
  if ($Plan.LogDriver) { $args += @('--log-driver',$Plan.LogDriver) }
  $args += @('--label',"app=$($Plan.AppLabel)",'--label',"uas.transaction=$($Plan.TransactionID)",$Image)
  Invoke-UasWSLC $Plan.Session $args | Out-Null
  $restart = $Plan.Restart
  if ($restart -eq 'on-failure') { $restart += ':' + $Plan.RetryCount }
  Invoke-UasDocker $Plan.Session @('update','--restart',$restart,$Plan.Name) | Out-Null
}
function Get-UasHealthURL {
  param($Plan)
  $bind = $Plan.Binds[0]
  if ($bind -notmatch '^(\[[0-9a-fA-F:]+\]|[0-9.]+):(\d+):8080$') { throw 'Cannot derive health URL; pass -HealthURL' }
  $hostIP = $Matches[1]; $port = $Matches[2]
  if ($hostIP -eq '0.0.0.0' -or $hostIP -eq '[::]') { $hostIP = '127.0.0.1' }
  return "http://${hostIP}:$port/readyz"
}
function Read-UasHealth {
  param([string]$URL)
  $request = [Net.WebRequest]::Create($URL)
  $request.Proxy = $null; $request.Timeout = 2000
  $response = $request.GetResponse()
  try {
    $reader = New-Object IO.StreamReader($response.GetResponseStream())
    try { return ($reader.ReadToEnd() | ConvertFrom-Json) } finally { $reader.Dispose() }
  } finally { $response.Close() }
}
function Test-UasIdentity {
  param($Plan,[string]$HealthURL)
  try {
    $inside = Invoke-UasDocker $Plan.Session @('exec',$Plan.Name,'wget','-q','-O','-','http://127.0.0.1:8080/readyz') | ConvertFrom-Json
    $outside = Read-UasHealth $HealthURL
    return ($inside.status -eq 'ready' -and $outside.status -eq 'ready' -and $inside.instance_id -and $inside.instance_id -ceq $outside.instance_id -and $inside.version -ceq $outside.version)
  } catch { return $false }
}
function Ensure-UasPlan {
  param($Plan,[string]$HealthURL)
  if (-not $HealthURL) { $HealthURL = Get-UasHealthURL $Plan }
  if (-not $Plan.Running) { Invoke-UasWSLC $Plan.Session @('start',$Plan.Name) | Out-Null }
  for ($attempt=0; $attempt -lt 5; $attempt++) {
    if (Test-UasIdentity $Plan $HealthURL) { Write-Output 'Expected instance is ready'; return }
    Start-Sleep -Seconds 2
  }
  throw 'Readiness/identity failed; no container was recreated. Inspect service and forwarding, then explicitly run recreate-wslc.ps1 if needed.'
}
function Save-UasPlan {
  param($Plan,[string]$Path)
  if ([Environment]::OSVersion.Platform -ne [PlatformID]::Win32NT) { throw 'Recovery plans require Windows DPAPI; do not export plaintext plans on Linux' }
  $Plan | Export-Clixml -LiteralPath $Path -Depth 8
}
function Restore-UasPlan {
  param($Plan)
  if (-not $Plan.Replaced) {
    $unchanged = Get-UasContainer $Plan.Session $Plan.Name
    if ($unchanged.Id -ne $Plan.OriginalID) { throw 'Recovery refused: original container identity changed' }
    if ($Plan.Running) { Invoke-UasWSLC $Plan.Session @('start',$Plan.Name) | Out-Null }
    return
  }
  # A failed run/update can leave a partial container. Never remove a foreign ID.
  $existing = @(Invoke-UasDocker $Plan.Session @('ps','-a','--filter',"name=^/$($Plan.Name)$",'--format','{{.ID}}'))
  if ($existing.Count -gt 0) {
    $current = Get-UasContainer $Plan.Session $Plan.Name
    if ($current.Id -ne $Plan.ReplacementID -and $current.Id -ne $Plan.OriginalID -and $current.Config.Labels.'uas.transaction' -ne $Plan.TransactionID) { throw 'Recovery refused: container identity changed' }
    Invoke-UasDocker $Plan.Session @('stop','--time','20',$Plan.Name) | Out-Null
    Invoke-UasWSLC $Plan.Session @('remove',$Plan.Name) | Out-Null
  }
  # Restore the complete snapshot, not a merge that retains WAL/SHM/new files.
  $restore = 'find /target -mindepth 1 -maxdepth 1 -exec rm -rf -- {} + && cp -a /backup/. /target/'
  Invoke-UasDocker $Plan.Session @('run','--rm','--user','0:0','--entrypoint','/bin/sh','--mount',"type=volume,source=$($Plan.Backup),target=/backup,readonly",'--mount',(Get-UasMount $Plan '/target'),$Plan.Image,'-c',$restore) | Out-Null
  Start-UasPlan $Plan
  if (-not $Plan.Running) { Invoke-UasDocker $Plan.Session @('stop','--time','20',$Plan.Name) | Out-Null }
}
function Invoke-UasUpdate {
  param($Plan,[string]$Image,[string]$Backup,[string]$PlanPath,[string]$HealthURL)
  if ($Backup -notmatch '^[a-zA-Z0-9][a-zA-Z0-9_.-]+$' -or $Backup -eq $Plan.Source) { throw 'Invalid backup volume' }
  # Resolve and validate the target image before any stop/mutation.
  $target = @(Invoke-UasDocker $Plan.Session @('image','inspect',$Image) | ConvertFrom-Json)
  if ($target.Count -ne 1 -or -not $target[0].Id) { throw 'Target image unavailable' }
  if (@(Invoke-UasDocker $Plan.Session @('volume','ls','--format','{{.Name}}')) -contains $Backup) { throw 'Backup volume already exists' }
  $Plan.Backup = $Backup
  Save-UasPlan $Plan $PlanPath
  Write-Output "Validated migration: $($Plan.Name); mount type $($Plan.MountType); $($Plan.Binds.Count) port binding(s). Recovery plan: $PlanPath"
  try {
    $unchanged = Get-UasContainer $Plan.Session $Plan.Name
    if ($unchanged.Id -ne $Plan.OriginalID) { throw 'Container identity changed since planning' }
    Invoke-UasDocker $Plan.Session @('stop','--time','20',$Plan.Name) | Out-Null
    Invoke-UasDocker $Plan.Session @('volume','create',$Backup) | Out-Null
    Invoke-UasDocker $Plan.Session @('run','--rm','--user','0:0','--entrypoint','/bin/cp','--mount',(Get-UasMount $Plan '/source' -ReadOnly),'--mount',"type=volume,source=$Backup,target=/backup",$Plan.Image,'-a','/source/.','/backup/') | Out-Null
    $Plan.Replaced = $true
    # The complete backup exists. Persist before remove/run so recovery can
    # handle interruption on either side of the replacement boundary.
    Save-UasPlan $Plan $PlanPath
    Invoke-UasWSLC $Plan.Session @('remove',$Plan.Name) | Out-Null
    Start-UasPlan $Plan $target[0].Id
    $created = Get-UasContainer $Plan.Session $Plan.Name
    $Plan.ReplacementID = $created.Id
    Save-UasPlan $Plan $PlanPath
    if ($created.Image -ne $target[0].Id) { throw 'Replacement image identity mismatch' }
    if (-not $HealthURL) { $HealthURL = Get-UasHealthURL $Plan }
    $ready = $false
    for ($attempt=0; $attempt -lt 15; $attempt++) {
      if (Test-UasIdentity $Plan $HealthURL) { $ready=$true; break }
      Start-Sleep -Seconds 2
    }
    if (-not $ready) { throw 'Replacement readiness/identity check failed' }
    Write-Output "Upgrade verified; backup retained: $Backup. Recovery: restore-wslc.ps1 -PlanPath <saved-plan>"
  } catch {
    try { Restore-UasPlan $Plan; Write-Warning 'Upgrade failed; original snapshot/configuration restored' }
    catch { Write-Warning 'Automatic recovery failed; retain backup and use restore-wslc.ps1 with the saved plan after checking container identity' }
    throw 'Upgrade failed; see recovery plan (never delete the backup volume)'
  }
}
