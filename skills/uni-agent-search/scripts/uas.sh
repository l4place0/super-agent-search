#!/bin/sh
set -eu
usage() {
  cat <<'EOF'
Usage: uas.sh health|capabilities
       uas.sh search QUERY [--max-results 8] [--providers tavily,brave]
             [--include-domains go.dev] [--exclude-domains example.com] [--no-cache]
       uas.sh fetch URL [--focus WORDS] [--offset 0] [--limit 16000]
Environment: SAS_URL, SAS_TOKEN_FILE or SAS_TOKEN. Requires curl and jq.
EOF
}
die() { printf '%s\n' "$1" >&2; exit 2; }
command -v curl >/dev/null 2>&1 || die 'curl is required'
command -v jq >/dev/null 2>&1 || die 'jq is required'
op=${1:-help}
case "$op" in help|--help|-h) usage; exit 0;; esac
shift
base=${SAS_URL:-http://localhost:8098}
base=${base%/}
token=${SAS_TOKEN:-}
if [ "$op" != health ] && [ -n "${SAS_TOKEN_FILE:-}" ]; then token=$(cat "$SAS_TOKEN_FILE") || die 'Cannot read SAS_TOKEN_FILE'; fi
case "$token" in *'
'*|*'"'*|*'\'*) die 'Invalid token format';; esac
case "$base" in http://*|https://*) ;; *) die 'SAS_URL must use http:// or https://';; esac
if [ "$op" != health ] && [ -z "$token" ]; then die 'Set SAS_TOKEN_FILE or SAS_TOKEN'; fi
payload=''; method=GET; path=''
case "$op" in
  health) [ "$#" -eq 0 ] || die 'health takes no arguments'; path=/healthz;;
  capabilities) [ "$#" -eq 0 ] || die 'capabilities takes no arguments'; path=/v1/capabilities;;
  search|fetch)
    [ "$#" -gt 0 ] || die 'A query or URL is required'
    query=$1; shift
    max=8; providers=''; include=''; exclude=''; no_cache=false; focus=''; offset=0; limit=16000
    while [ "$#" -gt 0 ]; do
      option=$1; shift
      case "$option" in
        --no-cache) [ "$op" = search ] || die '--no-cache is search-only'; no_cache=true;;
        --max-results|--providers|--include-domains|--exclude-domains|--focus|--offset|--limit)
          [ "$#" -gt 0 ] || die "Missing value for $option"
          value=$1; shift
          case "$option" in
            --max-results) [ "$op" = search ] || die 'search-only option'; max=$value;;
            --providers) [ "$op" = search ] || die 'search-only option'; providers=$value;;
            --include-domains) [ "$op" = search ] || die 'search-only option'; include=$value;;
            --exclude-domains) [ "$op" = search ] || die 'search-only option'; exclude=$value;;
            --focus) [ "$op" = fetch ] || die 'fetch-only option'; focus=$value;;
            --offset) [ "$op" = fetch ] || die 'fetch-only option'; offset=$value;;
            --limit) [ "$op" = fetch ] || die 'fetch-only option'; limit=$value;;
          esac;;
        *) die "Unknown option: $option";;
      esac
    done
    for value in "$max" "$offset" "$limit"; do case "$value" in ''|*[!0-9]*) die 'Numeric options require nonnegative integers';; esac; done
    if [ "$op" = search ]; then
      payload=$(jq -cn --arg q "$query" --argjson n "$max" --arg p "$providers" --arg i "$include" --arg x "$exclude" --argjson nc "$no_cache" '{query:$q,max_results:$n,providers:($p|split(",")|map(select(length>0))),include_domains:($i|split(",")|map(select(length>0))),exclude_domains:($x|split(",")|map(select(length>0))),no_cache:$nc}')
    else
      payload=$(jq -cn --arg u "$query" --arg f "$focus" --argjson o "$offset" --argjson l "$limit" '{url:$u,focus:$f,offset:$o,limit:$l}')
    fi
    method=POST; path=/v1/$op;;
  *) die "Unknown command: $op";;
esac
umask 077
tmp=$(mktemp -d)
trap 'rm -f "$tmp/headers" "$tmp/body"; rmdir "$tmp"' EXIT HUP INT TERM
printf 'Authorization: Bearer %s\nContent-Type: application/json\n' "$token" > "$tmp/headers"
if [ "$method" = POST ]; then
  status=$(printf '%s' "$payload" | curl --silent --show-error --connect-timeout 8 --max-time 100 --request POST --header "@$tmp/headers" --data-binary @- --output "$tmp/body" --write-out '%{http_code}' "$base$path") || exit 1
else
  status=$(curl --silent --show-error --connect-timeout 8 --max-time 100 --header "@$tmp/headers" --output "$tmp/body" --write-out '%{http_code}' "$base$path") || exit 1
fi
if jq . "$tmp/body" 2>/dev/null; then :; else cat "$tmp/body"; fi
case "$status" in 2??) exit 0;; *) printf '\nHTTP %s\n' "$status" >&2; exit 1;; esac
