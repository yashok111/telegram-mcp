#!/usr/bin/env bash
# Post-deploy health check for the telegram-mcp daemon.
#
# Runs a battery of read-only runtime checks against a *running* daemon and its
# on-disk state, prints one PASS/WARN/FAIL line per check, and exits non-zero if
# any CRITICAL check fails — so it can gate `make deploy` or run unattended.
#
# This checks the live process, NOT the source: run `make check` for build/lint/
# test. Pass --full to run `make check` first, then the runtime checks.
#
# Usage:
#   bash scripts/healthcheck.sh            # runtime checks only
#   bash scripts/healthcheck.sh --full     # `make check` + runtime checks
#   STATE_DIR=/path SERVICE=telegram-mcp bash scripts/healthcheck.sh
#
# Env overrides:
#   STATE_DIR    state dir (default ~/.claude/channels/telegram)
#   BINARY       daemon binary (default ./bin/telegram-mcp, repo-relative)
#   SERVICE      systemd --user unit name (default telegram-mcp)
#   LOG_WINDOW   minutes to scan back when the service start time is unknown (default 15)

set -uo pipefail

cd "$(dirname "$0")/.." # repo root

STATE_DIR="${STATE_DIR:-$HOME/.claude/channels/telegram}"
BINARY="${BINARY:-./bin/telegram-mcp}"
SERVICE="${SERVICE:-telegram-mcp}"
LOG_WINDOW="${LOG_WINDOW:-15}"

PID_FILE="$STATE_DIR/daemon.pid"
SOCK_FILE="$STATE_DIR/daemon.sock"
LOG_FILE="$STATE_DIR/daemon.log"
ACCESS_FILE="$STATE_DIR/access.json"
SESS_DIR="$STATE_DIR/sessions"

# ---- output helpers ----------------------------------------------------------
if [[ -t 1 && -z "${NO_COLOR:-}" ]]; then
  C_OK=$'\e[32m'; C_WARN=$'\e[33m'; C_BAD=$'\e[31m'; C_DIM=$'\e[2m'; C_RST=$'\e[0m'
else
  C_OK=''; C_WARN=''; C_BAD=''; C_DIM=''; C_RST=''
fi

FAILED=0
WARNED=0

pass() { printf '%s  PASS%s  %s\n' "$C_OK" "$C_RST" "$*"; }
warn() { printf '%s  WARN%s  %s\n' "$C_WARN" "$C_RST" "$*"; WARNED=$((WARNED + 1)); }
fail() { printf '%s  FAIL%s  %s\n' "$C_BAD" "$C_RST" "$*"; FAILED=$((FAILED + 1)); }
info() { printf '%s  ··  %s%s\n' "$C_DIM" "$*" "$C_RST"; }
hdr()  { printf '\n%s== %s ==%s\n' "$C_DIM" "$*" "$C_RST"; }

# ---- optional full build/lint/test gate -------------------------------------
if [[ "${1:-}" == "--full" ]]; then
  hdr "make check (lint + test + build)"
  if make check; then
    pass "make check"
  else
    fail "make check failed — see output above"
  fi
fi

# ---- binary ------------------------------------------------------------------
hdr "binary"
if [[ -x "$BINARY" ]]; then
  pass "binary present & executable: $BINARY"
  if "$BINARY" self --statusline >/dev/null 2>&1; then
    pass "binary runs (self --statusline exit 0)"
  else
    warn "binary present but 'self --statusline' returned non-zero"
  fi
else
  fail "binary missing or not executable: $BINARY (run: make build)"
fi

# ---- systemd unit ------------------------------------------------------------
hdr "systemd unit"
SVC_ACTIVE=no
SVC_START_ISO=""
if command -v systemctl >/dev/null 2>&1 && systemctl --user show -p Version >/dev/null 2>&1; then
  state="$(systemctl --user is-active "$SERVICE" 2>/dev/null || true)"
  if [[ "$state" == "active" ]]; then
    SVC_ACTIVE=yes
    pass "unit '$SERVICE' is active"
    enter="$(systemctl --user show "$SERVICE" -p ActiveEnterTimestamp --value 2>/dev/null || true)"
    if [[ -n "$enter" && "$enter" != "n/a" ]]; then
      SVC_START_ISO="$(date -d "$enter" +%Y-%m-%dT%H:%M:%S 2>/dev/null || true)"
      info "started: $enter"
    fi
  else
    warn "unit '$SERVICE' is '$state' (not active) — daemon may be running bare/spawned"
  fi
else
  info "no reachable systemd --user manager — skipping unit check (daemon may be shim-spawned)"
fi

# ---- daemon process ----------------------------------------------------------
hdr "daemon process"
RUN_PID=""
RUN_PID="$(pgrep -f 'telegram-mcp daemon' | head -n1 || true)"
if [[ -n "$RUN_PID" ]]; then
  pass "daemon process alive (pid $RUN_PID)"
else
  fail "no 'telegram-mcp daemon' process found"
fi

# ---- pid file consistency ----------------------------------------------------
hdr "pid file"
if [[ -f "$PID_FILE" ]]; then
  FPID="$(tr -dc '0-9' <"$PID_FILE" || true)"
  if [[ -z "$FPID" ]]; then
    fail "pid file empty/garbled: $PID_FILE"
  elif ! kill -0 "$FPID" 2>/dev/null; then
    fail "pid file points at dead pid $FPID (stale $PID_FILE)"
  else
    comm="$(cat "/proc/$FPID/comm" 2>/dev/null || true)"
    if [[ "$comm" == telegram-mcp* ]]; then
      pass "pid file → live telegram-mcp (pid $FPID)"
    else
      fail "pid $FPID is alive but comm='$comm' (not telegram-mcp)"
    fi
    if [[ -n "$RUN_PID" && "$FPID" != "$RUN_PID" ]]; then
      warn "pid file ($FPID) != pgrep ($RUN_PID) — possible split-brain"
    fi
  fi
else
  fail "pid file missing: $PID_FILE"
fi

# ---- socket ------------------------------------------------------------------
hdr "ipc socket"
if [[ -S "$SOCK_FILE" ]]; then
  pass "socket present: $SOCK_FILE"
  perms="$(stat -c '%a' "$SOCK_FILE" 2>/dev/null || true)"
  if [[ "$perms" == "600" ]]; then
    pass "socket perms 0600"
  else
    warn "socket perms are $perms (expected 600)"
  fi
elif [[ -e "$SOCK_FILE" ]]; then
  fail "$SOCK_FILE exists but is not a socket"
else
  fail "socket missing: $SOCK_FILE"
fi

# ---- daemon log: errors since start -----------------------------------------
hdr "daemon log"
if [[ -f "$LOG_FILE" ]]; then
  cutoff="$SVC_START_ISO"
  [[ -z "$cutoff" ]] && cutoff="$(date -d "-${LOG_WINDOW} min" +%Y-%m-%dT%H:%M:%S 2>/dev/null || true)"
  if [[ -z "$cutoff" ]]; then
    warn "could not compute log cutoff (no GNU date?) — skipping error scan"
  else
    # JSON lines look like {"time":"2026-06-10T06:20:09.483+03:00","level":"ERROR",...}
    # Compare the fixed-width 19-char ISO prefix lexicographically (same host TZ).
    scan() {
      local level="$1"
      awk -v cut="$cutoff" -v lvl="\"level\":\"$1\"" '
        index($0, lvl) {
          if (match($0, /"time":"[^"]+"/)) {
            t = substr($0, RSTART + 8, 19)
            if (t >= cut) { c++; if (c <= 5) print "       " $0 }
          }
        }
        END { exit (c > 0 ? 0 : 1) }
      ' "$LOG_FILE"
    }
    if errs="$(scan ERROR)"; then
      fail "ERROR lines in daemon.log since $cutoff:"
      printf '%s\n' "$errs"
    else
      pass "no ERROR in daemon.log since $cutoff"
    fi
    if warns="$(scan WARN)"; then
      warn "WARN lines since $cutoff (showing ≤5):"
      printf '%s\n' "$warns"
    fi
  fi
else
  info "no daemon.log at $LOG_FILE (journald-only deploy?) — skipping log scan"
fi

# ---- connected shims ---------------------------------------------------------
hdr "connected shims"
if [[ -d "$SESS_DIR" ]]; then
  shopt -s nullglob
  files=("$SESS_DIR"/*.json)
  shopt -u nullglob
  n=${#files[@]}
  if (( n == 0 )); then
    info "no shim session files (no Claude Code sessions attached right now)"
  else
    pass "$n shim session file(s)"
    for f in "${files[@]}"; do
      if command -v jq >/dev/null 2>&1; then
        line="$(jq -r '"@\(.alias // "?")  \(.workdir // "?")  \(.label // "")"' "$f" 2>/dev/null || echo "(unreadable)")"
      else
        line="$(tr -d '\n' <"$f")"
      fi
      info "$line"
    done
  fi
else
  info "no sessions dir at $SESS_DIR"
fi

# ---- access.json -------------------------------------------------------------
hdr "access state"
if [[ -f "$ACCESS_FILE" ]]; then
  if command -v jq >/dev/null 2>&1; then
    if jq -e . "$ACCESS_FILE" >/dev/null 2>&1; then
      allow="$(jq -r '.allowFrom | length' "$ACCESS_FILE" 2>/dev/null || echo '?')"
      pass "access.json valid JSON (allowFrom: $allow)"
    else
      fail "access.json is not valid JSON: $ACCESS_FILE"
    fi
  else
    pass "access.json present (jq absent — skipping JSON validation)"
  fi
else
  warn "access.json missing: $ACCESS_FILE (no allowlist yet?)"
fi

# ---- summary -----------------------------------------------------------------
hdr "summary"
if (( FAILED > 0 )); then
  printf '%s%d FAIL%s, %d warn — daemon UNHEALTHY\n' "$C_BAD" "$FAILED" "$C_RST" "$WARNED"
  exit 1
fi
if (( WARNED > 0 )); then
  printf '%s0 fail, %d warn%s — daemon healthy (with warnings)\n' "$C_WARN" "$WARNED" "$C_RST"
  exit 0
fi
printf '%sall checks passed%s — daemon healthy\n' "$C_OK" "$C_RST"
exit 0
