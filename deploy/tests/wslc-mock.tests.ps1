$ErrorActionPreference='Stop'
. (Join-Path (Split-Path -Parent $PSScriptRoot) 'wslc-common.ps1')
$savePlanFunction = ${function:Save-UasPlan}
foreach ($file in Get-ChildItem (Split-Path -Parent $PSScriptRoot) -Recurse -Filter *.ps1) {
  $tokens=$null; $parseErrors=$null
  [Management.Automation.Language.Parser]::ParseFile($file.FullName,[ref]$tokens,[ref]$parseErrors) | Out-Null
  if ($parseErrors.Count) { throw "Parser failure: $($file.Name)" }
}
function Assert($Condition,[string]$Message) { if (-not $Condition) { throw $Message } }
$fixture = @'
{"Id":"old-id","Image":"sha256:old","State":{"Running":true},"Mounts":[{"Type":"volume","Name":"custom-data","Source":"/var/lib/docker/volumes/custom-data/_data","Destination":"/data","RW":true}],"Config":{"Image":"old-tag","User":"10001:10001","Entrypoint":["/usr/local/bin/sas-server"],"Cmd":null,"WorkingDir":"","Labels":{"app":"uni-agent-search","custom":"keep"},"Env":["SAS_DATA_DIR=/data","SAS_LISTEN=:8080","HTTP_PROXY=http://proxy.invalid:8888","SAS_DOH_URL=https://dns.invalid/query"]},"HostConfig":{"NetworkMode":"bridge","Privileged":false,"ReadonlyRootfs":false,"PortBindings":{"8080/tcp":[{"HostIp":"192.0.2.2","HostPort":"8123"}]},"Memory":536870912,"MemorySwap":1073741824,"NanoCpus":2000000000,"RestartPolicy":{"Name":"unless-stopped","MaximumRetryCount":0},"LogConfig":{"Type":"json-file","Config":{}}}}
'@
$script:calls = New-Object System.Collections.ArrayList
$script:current = $fixture | ConvertFrom-Json
$script:exists = $true
$script:failAt = ''
$script:healthy = $true
function Invoke-UasWSLC {
  param([string]$Session,[string[]]$Arguments)
  [void]$script:calls.Add(@($Arguments))
  $joined = $Arguments -join ' '
  if ($script:failAt -and $joined.Contains($script:failAt)) { $script:failAt=''; throw 'injected failure' }
  if ($Arguments[0] -eq 'system') {
    $docker = @($Arguments | Select-Object -Skip 4)
    switch ($docker[0]) {
      'inspect' { return ($script:current | ConvertTo-Json -Depth 12 -Compress) }
      'image' { return '{"Id":"sha256:new"}' }
      'volume' { if ($docker[1] -eq 'ls') { return }; return 'backup' }
      'ps' { if ($script:exists) { return $script:current.Id }; return }
      'exec' { return '{"status":"ready","version":"0.2.4","instance_id":"inside"}' }
    }
  }
  if ($Arguments[0] -eq 'remove') { $script:exists=$false }
  if ($Arguments[0] -eq 'run') {
    $script:exists=$true
    $script:current = $fixture | ConvertFrom-Json
    $script:current.Id = 'new-id'
    $script:current.Image = $Arguments[-1]
    $script:current.Config.Labels | Add-Member -NotePropertyName 'uas.transaction' -NotePropertyValue $script:transaction
  }
}
# No Docker/WSLC process, network, credentials or paid provider is accessed.
function Save-UasPlan { param($Plan,[string]$Path) }
function Start-Sleep { param([int]$Seconds) }
function Read-UasHealth { param([string]$URL)
  if ($script:healthy) { return [pscustomobject]@{status='ready';version='0.2.4';instance_id='inside'} }
  return [pscustomobject]@{status='ready';version='0.2.4';instance_id='wrong-instance'}
}
function Reset-Test {
  $script:current=$fixture | ConvertFrom-Json
  $script:calls.Clear(); $script:exists=$true; $script:healthy=$true; $script:failAt=''
}
Reset-Test
$plan=Get-UasPlan 'mock-session' 'uni-agent-search'
$script:transaction=$plan.TransactionID
Assert ($plan.Source -eq 'custom-data') 'Lost actual data volume'
Assert ($plan.Binds[0] -eq '192.0.2.2:8123:8080') 'Lost bind address'
Assert ($plan.Environment[0] -is [Security.SecureString]) 'Plaintext recovery environment'
Assert ((Get-UasHealthURL $plan) -eq 'http://192.0.2.2:8123/readyz') 'Wrong derived health URL'
Invoke-UasUpdate $plan 'new-tag' 'backup-test' 'unused' '' | Out-Null
$flattened = @($script:calls | ForEach-Object { $_ -join ' ' }) -join "`n"
Assert ($flattened.Contains('source=custom-data,target=/source,readonly')) 'Backed up default volume instead of actual volume'
Assert (-not $flattened.Contains('source=sas-data')) 'Default volume leaked into update'
Assert ($flattened.Contains('HTTP_PROXY=http://proxy.invalid:8888') -and $flattened.Contains('SAS_DOH_URL=https://dns.invalid/query')) 'Proxy/DoH lost'
Assert ($flattened.Contains('192.0.2.2:8123:8080') -and $flattened.Contains('--cpus 2')) 'Port/resource settings lost'
Assert ($script:current.Image -eq 'sha256:new') 'Did not pin target image'
Reset-Test
$plan=Get-UasPlan 'mock-session' 'uni-agent-search';$script:transaction=$plan.TransactionID
$script:healthy=$false
Assert (-not (Test-UasIdentity $plan 'http://unused')) 'Accepted a healthy response from another instance'
$failed=$false
try { Invoke-UasUpdate $plan 'new-tag' 'backup-test' 'unused' '' | Out-Null } catch { $failed=$true }
Assert $failed 'Identity failure did not abort upgrade'
Assert ($script:current.Image -eq 'sha256:old') 'Failed upgrade did not restore original image'
$flattened = @($script:calls | ForEach-Object { $_ -join ' ' }) -join "`n"
Assert ($flattened.Contains('cp -a /backup/. /target/')) 'Recovery did not restore full data snapshot'
foreach ($failure in @('volume create','/bin/cp','remove','run --pull','update --restart')) {
  Reset-Test
  $plan=Get-UasPlan 'mock-session' 'uni-agent-search';$script:transaction=$plan.TransactionID
  $script:failAt=$failure
  $failed=$false
  try { Invoke-UasUpdate $plan 'new-tag' 'backup-test' 'unused' '' | Out-Null } catch { $failed=$true }
  Assert $failed "Injected failure ignored: $failure"
  Assert ($script:current.Image -eq 'sha256:old') "Original image not recovered: $failure"
}
Reset-Test
$script:current.Mounts[0].Type='bind';$script:current.Mounts[0].Source='/srv/my-search'
$plan=Get-UasPlan 'mock-session' 'uni-agent-search'
Assert ((Get-UasMount $plan) -eq 'type=bind,source=/srv/my-search,target=/data') 'Bind mount lost'
Reset-Test
$script:current.HostConfig.NetworkMode='host'
$failed=$false;try { Get-UasPlan 'mock-session' 'uni-agent-search' | Out-Null } catch { $failed=$true }
Assert $failed 'Unsupported network silently accepted'
Assert ($script:calls.Count -eq 1) 'Validation mutated deployment'
Reset-Test
$script:current.Config.Labels.app='unrelated'
$failed=$false;try { Get-UasPlan 'mock-session' 'uni-agent-search' | Out-Null } catch { $failed=$true }
Assert $failed 'Unrelated container accepted'
Reset-Test
$plan=Get-UasPlan 'mock-session' 'uni-agent-search';$script:transaction=$plan.TransactionID
$plan.Replaced=$true;$plan.Backup='backup-test'
$script:current.Id='foreign-id'
$failed=$false;try { Restore-UasPlan $plan } catch { $failed=$true }
Assert $failed 'Recovery accepted foreign container identity'
Assert (-not (@($script:calls | ForEach-Object { $_ -join ' ' }) -match '^remove')) 'Recovery removed foreign container'
Reset-Test
$plan=Get-UasPlan 'mock-session' 'uni-agent-search'
$script:healthy=$false
$failed=$false;try { Ensure-UasPlan $plan 'http://unused' | Out-Null } catch { $failed=$true }
Assert $failed 'Ensure accepted wrong instance'
Assert (-not (@($script:calls | ForEach-Object { $_ -join ' ' }) -match '^(run|remove|start)')) 'Ensure recreated a running container after health failure'
$script:calls.Clear();$script:healthy=$true;$plan.Running=$false
Ensure-UasPlan $plan 'http://unused' | Out-Null
Assert (@($script:calls | ForEach-Object { $_ -join ' ' }) -contains 'start uni-agent-search') 'Ensure did not start stopped original container'
if ([Environment]::OSVersion.Platform -eq [PlatformID]::Win32NT) {
  $temp=Join-Path ([IO.Path]::GetTempPath()) ([guid]::NewGuid().ToString('N')+'.clixml')
  try {
    & $savePlanFunction $plan $temp
    $raw=[IO.File]::ReadAllText($temp)
    Assert (-not $raw.Contains('http://proxy.invalid')) 'Recovery file exposed environment'
    $restored=Import-Clixml -LiteralPath $temp
    $plain=(New-Object Net.NetworkCredential('', $restored.Environment[2])).Password
    Assert ($plain -eq 'HTTP_PROXY=http://proxy.invalid:8888') 'DPAPI roundtrip failed'
  } finally { Remove-Item -LiteralPath $temp -ErrorAction SilentlyContinue }
}
Write-Output 'WSLC mock tests passed (no live operations).'
