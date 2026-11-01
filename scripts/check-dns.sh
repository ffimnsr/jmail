#!/usr/bin/env bash
#
# check-dns.sh - query the DNS records jmail depends on and report what is wrong.
#
# It reads the environment file the installer writes, so on the mail server it
# needs no arguments:
#
#   ./scripts/check-dns.sh
#   ./scripts/check-dns.sh --env ./jmail.env one.example two.example
#
# Each line is PASS, WARN, FAIL or INFO. FAIL exits 1; WARN does not, because
# some of those (p=none, a missing rua) are choices rather than mistakes.
#
# The sending IP is detected from the local routing table. Pass --server-ip to
# check a different address, and --no-port-check on a host that is not the mail
# server.

set -euo pipefail

CONTAINER="jmail"
SPF_MAX_DEPTH=10
# What receivers ask, rather than whatever the local resolver has cached. Pass
# --resolver system (or another address) to change it.
RESOLVER="1.1.1.1"

FAILS=0
WARNS=0

# --- output -----------------------------------------------------------------

die() {
  printf 'error: %s\n' "$*" >&2
  exit 2
}

need_cmd() {
  command -v "$1" >/dev/null 2>&1
}

report() {
  local level="$1"
  shift
  printf '  %-5s %s\n' "$level" "$*"
  case "$level" in
    FAIL) FAILS=$((FAILS + 1)) ;;
    WARN) WARNS=$((WARNS + 1)) ;;
  esac
}

# detail lines are indented under the check they explain.
detail() {
  printf '        %s\n' "$*"
}

usage() {
  cat <<'EOF'
Usage: ./scripts/check-dns.sh [options] [domain...]

Checks, for JMAIL_HOSTNAME and every sending domain:

  - A record of JMAIL_HOSTNAME points at the sending IP
  - PTR of the sending IP resolves back to JMAIL_HOSTNAME (forward-confirmed)
  - outbound port 25 is open
  - SPF exists, is a single record, and authorizes the sending IP
  - the DKIM key is published, and matches the one in the container
  - DMARC exists and would not reject mail that this server sends

Options:
  --env FILE        read JMAIL_HOSTNAME, JMAIL_ORIGIN and JMAIL_DOMAINS from FILE
                    (default: $XDG_CONFIG_HOME/jmail/jmail.env)
  --server-ip IP    check this sending IP instead of detecting it
  --hostname NAME   banner hostname to check; defaults to JMAIL_HOSTNAME from the
                    environment file, or mail.<domain> for a one-off check
  --no-port-check   skip the outbound port 25 check
  --container NAME  container holding the DKIM keys (default: jmail)
  --resolver IP     query this resolver (default: 1.1.1.1, which is what
                    receivers see); pass "system" to use the host's resolver,
                    whose cached negative answers can hide a fresh record
  -h, --help        show this help

Exit status: 0 if nothing failed, 1 if a check failed, 2 on a usage error.
EOF
}

# --- DNS helpers ------------------------------------------------------------

# dig without the noise: the caller decides what an empty answer means. dig
# writes "communications error" lines to stdout, so drop them here; otherwise
# they read as answers and an unreachable resolver looks like empty records.
dig_q() {
  if [[ -n "$RESOLVER" && "$RESOLVER" != "system" ]]; then
    dig +short +timeout=3 +tries=2 "@$RESOLVER" "$@" 2>/dev/null | grep -v '^;;' || true
  else
    dig +short +timeout=3 +tries=2 "$@" 2>/dev/null | grep -v '^;;' || true
  fi
}

# TXT answers may come back as several quoted chunks on one line; join them.
dig_txt() {
  local name="$1" out
  out="$(dig_q -t TXT "$name")"
  [[ -n "$out" ]] || return 0
  printf '%s\n' "$out" | while IFS= read -r line; do
    [[ -n "$line" ]] || continue
    printf '%s\n' "$line" | sed -e 's/" *"//g' -e 's/^"//' -e 's/"$//'
  done
}

dig_a() {
  local name="$1"
  dig_q -t A "$name" | grep -E '^[0-9]{1,3}(\.[0-9]{1,3}){3}$' || true
}

dig_ptr() {
  dig_q -x "$1" | sed 's/\.$//'
}

# --- IPv4 maths -------------------------------------------------------------

ip4_int() {
  local IFS=. a b c d
  read -r a b c d <<<"$1"
  # 10# forces base 10, so an octet written like 010 is not read as octal.
  printf '%s' "$(((10#$a << 24) | (10#$b << 16) | (10#$c << 8) | 10#$d))"
}

# ip4_in_cidr <ip> <address|address/bits>
ip4_in_cidr() {
  local ip="$1" spec="$2" base bits mask ip_int base_int
  [[ "$ip" =~ ^[0-9]{1,3}(\.[0-9]{1,3}){3}$ ]] || return 1
  base="${spec%%/*}"
  bits="${spec#*/}"
  [[ "$bits" == "$spec" ]] && bits=32
  [[ "$base" =~ ^[0-9]{1,3}(\.[0-9]{1,3}){3}$ ]] || return 1
  [[ "$bits" =~ ^[0-9]{1,2}$ ]] || return 1
  ((bits <= 32)) || return 1
  ((bits > 0)) || return 0
  mask=$(((0xFFFFFFFF << (32 - bits)) & 0xFFFFFFFF))
  ip_int="$(ip4_int "$ip")"
  base_int="$(ip4_int "$base")"
  (((ip_int & mask) == (base_int & mask)))
}

# --- SPF --------------------------------------------------------------------

# Result of the last spf_eval call.
SPF_VERDICT=""
SPF_REASON=""
SPF_TERM=""
SPF_SEEN=""

# Evaluates one domain's SPF record for one IPv4 address, recursing through
# include: and redirect= with RFC 7208's lookup budget and loop detection. The
# verdict lands in SPF_VERDICT: pass, fail, softfail, neutral, none, permerror
# or temperror.
#
# IPv6 sources only match a literal ip6: term. That is deliberate: this script
# does not evaluate ip6 CIDR blocks, and pretending it did would be worse than
# saying so.
spf_eval() {
  local domain="$1" ip="$2" depth="$3"
  local records count record tokens=() token raw qual rest host cidr target
  local targets matched redirect="" addr

  if ((depth > SPF_MAX_DEPTH)); then
    SPF_VERDICT=permerror
    SPF_REASON="include/redirect nested deeper than $SPF_MAX_DEPTH"
    return 0
  fi
  if [[ " $SPF_SEEN " == *" $domain "* ]]; then
    SPF_VERDICT=permerror
    SPF_REASON="include/redirect loop at $domain"
    return 0
  fi
  SPF_SEEN="$SPF_SEEN $domain"

  records="$(dig_txt "$domain" | grep -i '^v=spf1' || true)"
  if [[ -z "$records" ]]; then
    SPF_VERDICT=none
    SPF_REASON="no SPF record"
    return 0
  fi
  count="$(printf '%s\n' "$records" | grep -c .)"
  if ((count > 1)); then
    SPF_VERDICT=permerror
    SPF_REASON="$count SPF records; RFC 7208 allows exactly one"
    return 0
  fi
  record="$(printf '%s\n' "$records" | head -1)"

  read -ra tokens <<<"$record"
  for token in "${tokens[@]}"; do
    case "$token" in
      v=spf1) continue ;;
      redirect=*) redirect="${token#redirect=}"; continue ;;
      exp=*) continue ;;
      # ptr: is deprecated and exists: needs macros this script does not
      # expand; skipping them leaves the rest of the record authoritative.
      ptr | ptr:* | exists:* | exists=*) continue ;;
    esac

    raw="$token"
    qual="${token:0:1}"
    case "$qual" in
      "+" | "-" | "~" | "?") token="${token:1}" ;;
      *) qual="+" ;;
    esac

    matched=0
    case "$token" in
      all)
        matched=1
        ;;
      ip4:*)
        if ip4_in_cidr "$ip" "${token#ip4:}"; then matched=1; fi
        ;;
      ip6:*)
        if [[ "$ip" == *:* ]] && [[ "$ip" == "${token#ip6:}" ]]; then matched=1; fi
        ;;
      a | a:* | a/*)
        rest="${token#a}"
        rest="${rest#:}"
        host="${rest%%/*}"
        cidr=""
        if [[ "$rest" == */* ]]; then cidr="${rest#*/}"; fi
        [[ -n "$host" ]] || host="$domain"
        [[ -n "$cidr" ]] || cidr=32
        while IFS= read -r addr; do
          [[ -n "$addr" ]] || continue
          if ip4_in_cidr "$ip" "$addr/$cidr"; then matched=1; fi
        done <<<"$(dig_a "$host")"
        ;;
      mx | mx:* | mx/*)
        rest="${token#mx}"
        rest="${rest#:}"
        host="${rest%%/*}"
        cidr=""
        if [[ "$rest" == */* ]]; then cidr="${rest#*/}"; fi
        [[ -n "$host" ]] || host="$domain"
        [[ -n "$cidr" ]] || cidr=32
        targets="$(dig_q -t MX "$host" | awk '{print $2}')"
        # RFC 7208: with no MX records the mechanism falls back to the A record.
        [[ -n "$targets" ]] || targets="$host"
        while IFS= read -r target; do
          target="${target%.}"
          [[ -n "$target" ]] || continue
          while IFS= read -r addr; do
            [[ -n "$addr" ]] || continue
            if ip4_in_cidr "$ip" "$addr/$cidr"; then matched=1; fi
          done <<<"$(dig_a "$target")"
          if ((matched)); then break; fi
        done <<<"$targets"
        ;;
      include:*)
        target="${token#include:}"
        spf_eval "$target" "$ip" $((depth + 1))
        case "$SPF_VERDICT" in
          pass)
            matched=1
            ;;
          none)
            SPF_VERDICT=permerror
            SPF_REASON="include:$target has no SPF record"
            return 0
            ;;
          permerror | temperror)
            SPF_REASON="include:$target: $SPF_REASON"
            return 0
            ;;
        esac
        ;;
      *)
        SPF_VERDICT=permerror
        SPF_REASON="cannot evaluate mechanism '$raw'"
        return 0
        ;;
    esac

    if ((matched)); then
      case "$qual" in
        "+") SPF_VERDICT=pass ;;
        "-") SPF_VERDICT=fail ;;
        "~") SPF_VERDICT=softfail ;;
        "?") SPF_VERDICT=neutral ;;
      esac
      SPF_TERM="$raw"
      return 0
    fi
  done

  if [[ -n "$redirect" ]]; then
    spf_eval "$redirect" "$ip" $((depth + 1))
    SPF_TERM="redirect=$redirect"
    return 0
  fi

  SPF_VERDICT=neutral
  SPF_REASON="no mechanism matched and there is no all term"
  return 0
}

# --- configuration ----------------------------------------------------------

# Reads one key from an environment file, stripping the quotes that versions of
# install.sh before 1.0.18 wrote around every value.
env_read() {
  local file="$1" key="$2"
  [[ -f "$file" ]] || return 0
  awk -F= -v key="$key" '
    $1 == key {
      value = $0
      sub(/^[^=]*=/, "", value)
      if (value ~ /^".*"$/) { value = substr(value, 2, length(value) - 2) }
      found = 1
    }
    END { if (found) print value }
  ' "$file"
}

normalize_domain() {
  local domain
  domain="$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]')"
  domain="${domain#"${domain%%[![:space:]]*}"}"
  domain="${domain%"${domain##*[![:space:]]}"}"
  printf '%s' "${domain%.}"
}

detect_ip() {
  local ip=""
  if need_cmd ip; then
    ip="$(ip -4 route get 1.1.1.1 2>/dev/null | sed -n 's/.* src \([0-9.]*\).*/\1/p' | head -1 || true)"
  fi
  if [[ -z "$ip" ]] && need_cmd curl; then
    ip="$(curl -fsS -4 --max-time 10 https://ifconfig.me 2>/dev/null || true)"
  fi
  printf '%s' "$ip"
}

# Prints the p= value of the key the container generated for one domain, or
# nothing when the container is not reachable from here.
local_dkim_p() {
  local domain="$1" raw=""
  need_cmd podman || return 1
  podman exec "$CONTAINER" true >/dev/null 2>&1 || return 1
  raw="$(podman exec "$CONTAINER" cat "/etc/opendkim/keys/$domain/sendonly.txt" 2>/dev/null || true)"
  [[ -n "$raw" ]] || return 1
  printf '%s' "$raw" |
    sed -e 's/;-----.*$//' |
    # All whitespace, not just newlines: sendonly.txt splits the record into
    # quoted chunks separated by tabs and spaces, and stopping p= at the first
    # tab truncates the key to one chunk.
    tr -d '[:space:]"' |
    sed -n 's/.*p=\([A-Za-z0-9+/=]*\).*/\1/p'
}

# tag_value <record> <tag>
tag_value() {
  printf '%s' "$1" | tr -d ' ' | tr ';' '\n' | sed -n "s/^$2=//p" | head -1
}

# --- checks -----------------------------------------------------------------

check_host() {
  local hostname="$1" ip="$2" addresses ptr_names ptr_name match=0

  printf 'host %s\n' "$hostname"

  addresses="$(dig_a "$hostname")"
  if [[ -z "$addresses" ]]; then
    report FAIL "$hostname has no A record"
    detail "receivers look up the name your SMTP banner announces"
  elif grep -qxF "$ip" <<<"$addresses"; then
    report PASS "A $hostname -> $ip"
  else
    report FAIL "A $hostname -> $(printf '%s ' $addresses)but the sending IP is $ip"
    detail "SPF a:$hostname and client TLS both depend on this"
  fi

  ptr_names="$(dig_ptr "$ip")"
  if [[ -z "$ptr_names" ]]; then
    report FAIL "no PTR record for $ip"
    detail "set reverse DNS at your hosting provider"
    return 0
  fi

  while IFS= read -r ptr_name; do
    [[ -n "$ptr_name" ]] || continue
    if [[ "${ptr_name,,}" == "${hostname,,}" ]]; then match=1; fi
  done <<<"$ptr_names"

  if ((match)); then
    report PASS "PTR $ip -> $hostname"
    # Forward-confirmed: the PTR name has to resolve back to the same IP.
    if dig_a "$hostname" | grep -qxF "$ip"; then
      report PASS "forward-confirmed: $hostname resolves back to $ip"
    else
      report FAIL "$hostname does not resolve back to $ip (forward-confirmed reverse DNS)"
    fi
  else
    report FAIL "PTR $ip -> $(printf '%s ' $ptr_names)but the banner announces $hostname"
    detail "receivers compare the two; a mismatch costs deliverability"
  fi
}

check_port25() {
  local target
  if ! need_cmd timeout; then
    report WARN "cannot check outbound port 25: timeout(1) not found"
    return 0
  fi
  for target in gmail-smtp-in.l.google.com mx.cloudflare.net; do
    if timeout 8 bash -c "exec 3<>/dev/tcp/$target/25" >/dev/null 2>&1; then
      report PASS "outbound port 25 is open (reached $target)"
      return 0
    fi
  done
  report FAIL "outbound port 25 is blocked"
  detail "many providers block it; mail would queue instead of being delivered"
}

check_domain() {
  local domain="$1" ip="$2" hostname="$3"
  local record records record_count p_spf published_p p_dkim local_p mx dkim_raw
  local spf_ok=0 dkim_ok=0 dmarc dmarc_count p rua pct parent parent_p
  local ancestor inherited inherited_p inherited_rec

  printf '\n%s\n' "$domain"

  # SPF -----------------------------------------------------------------
  record="$(dig_txt "$domain" | grep -i '^v=spf1' || true)"
  record_count="$(printf '%s\n' "${record:-}" | grep -c . || true)"
  if [[ -z "$record" ]]; then
    report FAIL "no SPF record"
    detail "publish: v=spf1 ip4:$ip -all"
  elif ((record_count > 1)); then
    report FAIL "$record_count SPF records; RFC 7208 allows exactly one"
    detail "merge them into a single v=spf1 record"
  else
    p_spf="$(printf '%s\n' "$record" | head -1)"
    if [[ "$ip" == *:* ]]; then
      report WARN "SPF not evaluated for an IPv6 sending address"
      detail "$p_spf"
    else
      SPF_SEEN=""
      SPF_VERDICT=""
      SPF_REASON=""
      SPF_TERM=""
      spf_eval "$domain" "$ip" 0
      case "$SPF_VERDICT" in
        pass)
          spf_ok=1
          report PASS "SPF authorizes $ip (matched $SPF_TERM)"
          ;;
        fail | softfail)
          report FAIL "SPF does not authorize $ip (result: $SPF_VERDICT)"
          detail "$p_spf"
          detail "add ip4:$ip (or a:$hostname) before the all term"
          ;;
        neutral)
          report WARN "SPF is neutral for $ip${SPF_REASON:+ ($SPF_REASON)}"
          detail "$p_spf"
          ;;
        none)
          report FAIL "no SPF record"
          detail "publish: v=spf1 ip4:$ip -all"
          ;;
        *)
          report WARN "SPF could not be evaluated: ${SPF_REASON:-$SPF_VERDICT}"
          detail "$p_spf"
          ;;
      esac
      # An unqualified all, or +all, authorizes every sender on the internet.
      if [[ "$p_spf" =~ (^|[[:space:]])[+]?all([[:space:]]|$) ]]; then
        report WARN "SPF ends in '+all', which authorizes anyone"
        detail "$p_spf"
      fi
    fi
  fi

  # DKIM ----------------------------------------------------------------
  # A value that literally contains quote characters comes back from dig as
  # \" - the usual cause is pasting the quoted string from sendonly.txt into
  # the DNS provider, which stores the quotes as data. Receivers then read a
  # malformed key record, so this is a failure worth naming outright.
  dkim_raw="$(dig_q -t TXT "sendonly._domainkey.$domain" | head -1 || true)"
  published_p="$(dig_txt "sendonly._domainkey.$domain" | grep -i 'v=DKIM1' | head -1 || true)"
  if [[ "$dkim_raw" == *'\"'* ]]; then
    report FAIL "DKIM value contains literal quote characters"
    detail "the quotes from sendonly.txt were stored as part of the value"
    detail "paste it unquoted; the provider splits it into 255-byte strings itself"
  elif [[ -z "$published_p" ]]; then
    report FAIL "DKIM not published at sendonly._domainkey.$domain"
    local_p="$(local_dkim_p "$domain" || true)"
    if [[ -n "$local_p" ]]; then
      detail "publish the key the container generated:"
      detail "  podman exec $CONTAINER cat /etc/opendkim/keys/$domain/sendonly.txt"
    else
      detail "the installer prints the record once the container has started"
    fi
    # A subdomain's record belongs in the parent zone. Publishing it at the
    # parent apex is a common way to get this wrong, so name it when the key
    # sitting there is the one this container generated.
    parent="${domain#*.}"
    if [[ "$parent" != "$domain" && -n "$local_p" ]]; then
      parent_p="$(tag_value "$(dig_txt "sendonly._domainkey.$parent" | grep -i 'v=DKIM1' | head -1 || true)" p)"
      if [[ -n "$parent_p" && "$parent_p" == "$local_p" ]]; then
        detail "this key is published at sendonly._domainkey.$parent instead"
        detail "create it as sendonly._domainkey.$domain"
      fi
    fi
  else
    dkim_ok=1
    p_dkim="$(tag_value "$published_p" p)"
    local_p="$(local_dkim_p "$domain" || true)"
    if [[ -z "$p_dkim" ]]; then
      dkim_ok=0
      report FAIL "DKIM record has no p= value"
      detail "$published_p"
    elif [[ -z "$local_p" ]]; then
      report PASS "DKIM published (not compared: container $CONTAINER not reachable here)"
    elif [[ "$p_dkim" == "$local_p" ]]; then
      report PASS "DKIM published and matches the key in the container"
    else
      dkim_ok=0
      report FAIL "DKIM published key differs from the key in the container"
      detail "republish from: podman exec $CONTAINER cat /etc/opendkim/keys/$domain/sendonly.txt"
    fi
  fi

  # DMARC ---------------------------------------------------------------
  records="$(dig_txt "_dmarc.$domain" | grep -i '^v=DMARC1' || true)"
  dmarc_count="$(printf '%s\n' "${records:-}" | grep -c . || true)"
  if [[ -z "$records" ]]; then
    # DMARC still applies to a subdomain through the ancestor's sp= (or p=),
    # so a missing record here is not necessarily an open policy.
    ancestor="${domain#*.}"
    inherited=""
    inherited_p=""
    while [[ "$ancestor" == *.* ]]; do
      inherited_rec="$(dig_txt "_dmarc.$ancestor" | grep -i '^v=DMARC1' | head -1 || true)"
      if [[ -n "$inherited_rec" ]]; then
        inherited="$ancestor"
        inherited_p="$(tag_value "$inherited_rec" sp)"
        [[ -n "$inherited_p" ]] || inherited_p="$(tag_value "$inherited_rec" p)"
        break
      fi
      ancestor="${ancestor#*.}"
    done
    if [[ -z "$inherited" ]]; then
      report WARN "no DMARC record at _dmarc.$domain"
      detail "publish: v=DMARC1; p=none; rua=mailto:dmarc@$domain"
    elif [[ "$inherited_p" == "reject" || "$inherited_p" == "quarantine" ]]; then
      if [[ "$spf_ok" == "0" && "$dkim_ok" == "0" ]]; then
        report FAIL "DMARC inherits p=$inherited_p from _dmarc.$inherited, and neither SPF nor DKIM passes"
        detail "mail this server sends is blocked by the inherited policy"
      else
        report PASS "no record here; inherits p=$inherited_p from _dmarc.$inherited, and a mechanism passes"
      fi
    else
      report WARN "no record here; inherits p=${inherited_p:-none} from _dmarc.$inherited"
      detail "publish one to control this domain directly"
    fi
  elif ((dmarc_count > 1)); then
    report FAIL "more than one DMARC record; receivers ignore the policy"
  else
    dmarc="$(printf '%s\n' "$records" | head -1)"
    p="$(tag_value "$dmarc" p)"
    rua="$(tag_value "$dmarc" rua)"
    pct="$(tag_value "$dmarc" pct)"

    case "$p" in
      reject | quarantine | none) ;;
      *)
        report FAIL "DMARC record has no valid p= (none, quarantine or reject)"
        detail "$dmarc"
        ;;
    esac

    if [[ "$p" == "reject" && "$spf_ok" == "0" && "$dkim_ok" == "0" ]]; then
      report FAIL "DMARC p=reject, but neither SPF nor DKIM passes for $domain"
      detail "mail this server sends is rejected until one of them is fixed"
    elif [[ "$p" == "quarantine" && "$spf_ok" == "0" && "$dkim_ok" == "0" ]]; then
      report WARN "DMARC p=quarantine, but neither SPF nor DKIM passes for $domain"
      detail "mail this server sends is quarantined until one of them is fixed"
    elif [[ "$p" == "none" ]]; then
      report WARN "DMARC p=none: no enforcement"
      detail "tighten to p=reject once SPF and DKIM pass"
    elif [[ -n "$p" ]]; then
      report PASS "DMARC p=$p, and a mechanism already passes"
    fi

    if [[ -z "$rua" ]]; then
      report WARN "DMARC has no rua=, so you get no aggregate reports"
      detail "add: rua=mailto:dmarc@$domain"
    fi
    if [[ -n "$pct" ]] && ((pct < 100)); then
      report WARN "DMARC pct=$pct applies the policy to only part of your mail"
    fi
  fi

  # MX is informational: jmail is outbound-only, and inbound usually lives
  # elsewhere. A null MX is a deliberate "no mail here", not a fault.
  mx="$(dig_q -t MX "$domain" | awk '{print $2}' | tr '\n' ' ' | sed 's/ *$//')"
  if [[ -z "$mx" ]]; then
    report INFO "no MX record (fine: jmail does not accept inbound mail)"
  elif [[ "$mx" == "." ]]; then
    report INFO "null MX (fine: this domain accepts no inbound mail)"
  else
    report INFO "MX: $mx (inbound is handled outside jmail)"
  fi
}

# --- main -------------------------------------------------------------------

main() {
  local env_file="${XDG_CONFIG_HOME:-$HOME/.config}/jmail/jmail.env"
  local server_ip="" port_check=1 hostname="" origin="" domains_raw=""
  local domains=() arg domain opt_hostname=""

  while [[ $# -gt 0 ]]; do
    case "$1" in
      --env)
        [[ $# -ge 2 ]] || die "--env needs a value"
        env_file="$2"
        shift 2
        ;;
      --env=*)
        env_file="${1#*=}"
        shift
        ;;
      --server-ip)
        [[ $# -ge 2 ]] || die "--server-ip needs a value"
        server_ip="$2"
        shift 2
        ;;
      --server-ip=*)
        server_ip="${1#*=}"
        shift
        ;;
      --no-port-check)
        port_check=0
        shift
        ;;
      --container)
        [[ $# -ge 2 ]] || die "--container needs a value"
        CONTAINER="$2"
        shift 2
        ;;
      --hostname)
        [[ $# -ge 2 ]] || die "--hostname needs a value"
        opt_hostname="$2"
        shift 2
        ;;
      --hostname=*)
        opt_hostname="${1#*=}"
        shift
        ;;
      --resolver)
        [[ $# -ge 2 ]] || die "--resolver needs a value"
        RESOLVER="$2"
        shift 2
        ;;
      --resolver=*)
        RESOLVER="${1#*=}"
        shift
        ;;
      -h | --help)
        usage
        exit 0
        ;;
      --)
        shift
        break
        ;;
      -*)
        die "unknown option: $1 (see --help)"
        ;;
      *)
        break
        ;;
    esac
  done

  need_cmd dig || die "dig not found (install bind-utils, or dnsutils on Debian)"

  # Fail early: an unreachable resolver answers nothing, which would otherwise
  # look like every record is missing.
  if [[ -z "$(dig_q NS .)" ]]; then
    if [[ -n "$RESOLVER" && "$RESOLVER" != "system" ]]; then
      die "resolver $RESOLVER is not answering; pass --resolver system or another address"
    fi
    die "the system resolver is not answering; pass --resolver 1.1.1.1"
  fi

  # Domains on the command line win, so a one-off check of another zone does not
  # need an environment file at all.
  if (($# > 0)); then
    for arg in "$@"; do
      domains+=("$(normalize_domain "$arg")")
    done
  elif [[ -f "$env_file" ]]; then
    hostname="$(env_read "$env_file" JMAIL_HOSTNAME)"
    origin="$(env_read "$env_file" JMAIL_ORIGIN)"
    domains_raw="$(env_read "$env_file" JMAIL_DOMAINS)"
    if [[ -n "$origin" ]]; then
      domains+=("$(normalize_domain "$origin")")
    fi
    # Word splitting is intended: JMAIL_DOMAINS is comma or space separated.
    for arg in ${domains_raw//,/ }; do
      domain="$(normalize_domain "$arg")"
      [[ -n "$domain" ]] || continue
      if [[ ! " ${domains[*]} " == *" $domain "* ]]; then
        domains+=("$domain")
      fi
    done
  fi

  if ((${#domains[@]} == 0)); then
    die "no domains to check; pass one or more, or create $env_file"
  fi

  [[ -n "$server_ip" ]] || server_ip="$(detect_ip)"
  [[ -n "$server_ip" ]] || die "could not detect the sending IP; pass --server-ip"
  # --hostname wins over the environment file, which wins over the fallback.
  if [[ -n "$opt_hostname" ]]; then
    hostname="$opt_hostname"
  fi
  [[ -n "$hostname" ]] || hostname="mail.${domains[0]}"
  hostname="$(normalize_domain "$hostname")"

  printf 'jmail dns check\n'
  printf '================\n'
  printf 'server ip : %s\n' "$server_ip"
  printf 'hostname  : %s\n' "$hostname"
  printf 'resolver  : %s\n' "${RESOLVER:-system}"
  printf 'domains   : %s\n' "${domains[*]}"

  check_host "$hostname" "$server_ip"
  if ((port_check)); then
    check_port25
  fi
  for domain in "${domains[@]}"; do
    check_domain "$domain" "$server_ip" "$hostname"
  done

  printf '\n'
  if ((FAILS == 0 && WARNS == 0)); then
    printf 'All checks passed.\n'
  else
    printf '%d failed, %d warning(s).\n' "$FAILS" "$WARNS"
  fi
  ((FAILS == 0))
}

# Run main unless this file is being sourced (which is how the test suite
# borrows spf_eval). Read from stdin, as in curl | bash, BASH_SOURCE is unset.
if [[ -z "${BASH_SOURCE[0]:-}" ]] || [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  main "$@"
fi
