#requires -Version 7.0
$ErrorActionPreference = 'Stop'
function Usage {
@'
Usage: sas.ps1 health|capabilities
       sas.ps1 search QUERY [--max-results 8] [--providers tavily,brave]
               [--include-domains go.dev] [--exclude-domains example.com] [--no-cache]
       sas.ps1 fetch URL [--focus WORDS] [--offset 0] [--limit 16000]
Environment: SAS_URL, SAS_TOKEN_FILE or SAS_TOKEN.
'@
}
try {
  $arguments = @($args)
  if ($arguments.Count -eq 0 -or $arguments[0] -in @('help','--help','-h')) { Usage; exit 0 }
  $operation = $arguments[0]
  if ($operation -notin @('health','capabilities','search','fetch')) { throw "Unknown command: $operation" }
  $base = if ($env:SAS_URL) { $env:SAS_URL.TrimEnd('/') } else { 'http://localhost:8098' }
  $parsedUri = [uri]$base
  if ($parsedUri.Scheme -notin @('http','https')) { throw 'SAS_URL must use HTTP(S)' }
  $accessToken = $env:SAS_TOKEN
  if ($env:SAS_TOKEN_FILE) { $accessToken = (Get-Content -LiteralPath $env:SAS_TOKEN_FILE -Raw).Trim() }
  if ($operation -ne 'health' -and -not $accessToken) { throw 'Set SAS_TOKEN_FILE or SAS_TOKEN' }
  $body = $null
  if ($operation -in @('search','fetch')) {
    if ($arguments.Count -lt 2) { throw 'A query or URL is required' }
    $body = if ($operation -eq 'search') { @{query=$arguments[1]; max_results=8} } else { @{url=$arguments[1]; offset=0; limit=16000} }
    for ($index=2; $index -lt $arguments.Count; $index++) {
      $option = $arguments[$index]
      if ($option -eq '--no-cache' -and $operation -eq 'search') { $body.no_cache=$true; continue }
      $searchOptions=@('--max-results','--providers','--include-domains','--exclude-domains')
      $fetchOptions=@('--focus','--offset','--limit')
      if (($operation -eq 'search' -and $option -notin $searchOptions) -or ($operation -eq 'fetch' -and $option -notin $fetchOptions)) { throw "Unknown option: $option" }
      $index++; if ($index -ge $arguments.Count) { throw "Missing value for $option" }; $value=$arguments[$index]
      switch ($option) {
        '--max-results' { $body.max_results=[int]$value }
        '--providers' { $body.providers=@($value.Split(',') | ForEach-Object { $_.Trim() } | Where-Object { $_ }) }
        '--include-domains' { $body.include_domains=@($value.Split(',') | ForEach-Object { $_.Trim() } | Where-Object { $_ }) }
        '--exclude-domains' { $body.exclude_domains=@($value.Split(',') | ForEach-Object { $_.Trim() } | Where-Object { $_ }) }
        '--focus' { $body.focus=$value }
        '--offset' { $body.offset=[int]$value }
        '--limit' { $body.limit=[int]$value }
      }
    }
  } elseif ($arguments.Count -gt 1) { throw "$operation takes no arguments" }
  $path = if ($operation -eq 'health') { '/healthz' } else { '/v1/'+$operation }
  $options=@{Uri=$base+$path; Headers=@{Authorization='Bearer '+$accessToken}; TimeoutSec=100; SkipHttpErrorCheck=$true}
  if ($null -ne $body) { $options.Method='POST'; $options.ContentType='application/json; charset=utf-8'; $options.Body=[Text.Encoding]::UTF8.GetBytes(($body | ConvertTo-Json -Depth 5 -Compress)) }
  $response=Invoke-WebRequest @options
  try { $response.Content | ConvertFrom-Json | ConvertTo-Json -Depth 30 } catch { $response.Content }
  if ([int]$response.StatusCode -ge 300) { [Console]::Error.WriteLine('HTTP '+[int]$response.StatusCode); exit 1 }
} catch { [Console]::Error.WriteLine($_.Exception.Message); exit 2 }
