#!/usr/bin/env bash
# M1 acceptance: standard host -> deploy -> HTTPS -> health -> new release ->
# failed release -> automatic revert -> manual rollback -> reboot -> controller
# outage. Runs from control-01 against a bootstrapped node.
#
#   test/m1/acceptance.sh            # all steps
#   STEPS="2 3" test/m1/acceptance.sh
#
# Needs: lwd CLI configured for the controller, host $HOST registered, podman,
# the lwd-hello repo at $HELLO, and a registry the node can pull from ($REG).
set -euo pipefail

HOST=${HOST:-m1}
NODE_IP=${NODE_IP:-192.168.122.25}
REG=${REG:-$NODE_IP:5000}            # DEV registry on the node (see infra)
HELLO=${HELLO:-$HOME/dev/OBH/lwd-hello}
APP=hello ENV=staging
WEB=hello.m1.lwd.internal API=api.hello.m1.lwd.internal
STEPS=${STEPS:-"1 2 3 4 5 6 7 8"}
WORK=$(mktemp -d); trap 'rm -rf "$WORK"' EXIT

pass() { printf '  \033[32mPASS\033[0m %s\n' "$*"; }
fail() { printf '  \033[31mFAIL\033[0m %s\n' "$*"; exit 1; }
step() { printf '\n== %s\n' "$*"; }
want() { [[ " $STEPS " == *" $1 "* ]]; }

# get URL-host path -> body (HTTPS through the node's Caddy, internal CA)
get() { curl -fsSk --max-time 5 --resolve "$1:443:$NODE_IP" "https://$1$2"; }
code() { curl -sk -o /dev/null -w '%{http_code}' --max-time 5 --resolve "$1:443:$NODE_IP" "https://$1$2" || true; }
live_commit() { get "$API" /version | python3 -c 'import json,sys; print(json.load(sys.stdin)["commit"])'; }
live_release() { lwd app status "$APP" "$ENV" --json | python3 -c 'import json,sys; d=json.load(sys.stdin); print((d.get("live") or {}).get("release",""))'; }

# build_release MARK [manifest-env-overrides] -> prints release id.
# Drill images are the lwd-hello base images plus one ENV COMMIT=MARK layer:
# a distinct digest per release without a full rebuild (and without trusting
# build-arg cache invalidation, which podman's builder does not do reliably).
BASE_TAG=${BASE_TAG:-dev}
build_release() {
  local mark=$1 extra=${2:-}
  for s in web api; do
    printf 'FROM localhost/lwd-hello-%s:%s\nENV COMMIT=%s\n' "$s" "$BASE_TAG" "$mark" |
      podman build -q -f - -t "$REG/lwd-hello-$s:sha-$mark" "$WORK" >/dev/null
    podman push -q --tls-verify=false "$REG/lwd-hello-$s:sha-$mark"
  done
  # Release config comes from the manifest snapshot: apply the drill variant.
  sed "/^\[env.staging\]/,\$ s|^env *=.*|env    = { LOG_LEVEL = \"debug\"${extra} }|" "$HELLO/lwd.toml" > "$WORK/lwd.toml"
  grep -q "LOG_LEVEL = \"debug\"${extra}" "$WORK/lwd.toml" || fail "could not template manifest"
  lwd app apply "$WORK" >/dev/null
  lwd release create "$APP" --commit "$mark" --tag "sha-$mark" \
    --image web="$REG/lwd-hello-web:sha-$mark" \
    --image api="$REG/lwd-hello-api:sha-$mark" \
    --image worker="$REG/lwd-hello-api:sha-$mark" --json |
    python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])'
}

deploy_status() { # deploy and print the final status word
  lwd deploy "$APP" "$ENV" "$@" --json 2>/dev/null |
    python3 -c 'import json,sys; print(json.load(sys.stdin)["status"])' || true
}

wait_serving() { # wait up to $1 seconds for the web+api domains to answer 200
  for _ in $(seq "$1"); do
    [[ $(code "$WEB" /) == 200 && $(code "$API" /ready) == 200 ]] && return 0
    sleep 1
  done
  return 1
}

if want 1; then
  step "1. standard host is healthy"
  lwd host status "$HOST" --json | python3 -c 'import json,sys; d=json.load(sys.stdin); assert d["reachable"], d; assert d["node"]["caddy"]["state"]=="running", d' \
    && pass "$HOST reachable, Caddy running" || fail "host not healthy"
fi

if want 2; then
  step "2. deploy release A over HTTPS"
  A=$(build_release m1-a); echo "  release A = $A"
  [[ $(deploy_status --release "$A") == succeeded ]] || fail "deploy A did not succeed"
  [[ $(code "$WEB" /) == 200 ]] && pass "https://$WEB -> 200" || fail "web not serving"
  [[ $(live_commit) == m1-a ]] && pass "api /version commit = m1-a" || fail "wrong commit live"
  [[ $(code "$WEB" /) == 200 && $(curl -s -o /dev/null -w '%{http_code}' --resolve "$WEB:80:$NODE_IP" "http://$WEB/") == 308 ]] \
    && pass "HTTP redirects to HTTPS (308)" || fail "no HTTP->HTTPS redirect"
  echo "$A" > /tmp/claude-1000/m1-release-a
fi

if want 3; then
  step "3. deploy release B with zero failed requests"
  B=$(build_release m1-b); echo "  release B = $B"
  ( fails=0 total=0
    while [[ ! -f "$WORK/stop" ]]; do
      total=$((total+1)); [[ $(code "$API" /version) == 200 ]] || fails=$((fails+1)); sleep 0.1
    done; echo "$fails $total" > "$WORK/loop" ) &
  st=$(deploy_status --release "$B"); touch "$WORK/stop"; wait
  read -r fails total < "$WORK/loop"
  [[ $st == succeeded ]] || fail "deploy B: $st"
  [[ $(live_commit) == m1-b ]] || fail "B not live"
  [[ $fails == 0 ]] && pass "B live; $total requests during deploy, 0 failed" || fail "$fails/$total requests failed during deploy"
fi

if want 4; then
  step "4. release with broken readiness is rejected; B keeps serving"
  C=$(build_release m1-c ', BREAK = "ready"'); echo "  release C = $C"
  st=$(deploy_status --release "$C")
  [[ $st == failed ]] && pass "deploy C failed before going live" || fail "deploy C status: $st"
  [[ $(live_commit) == m1-b ]] && pass "B still live" || fail "live changed after failed deploy"
  lwd history "$APP" "$ENV" --json | python3 -c 'import json,sys; d=json.load(sys.stdin)[0]; assert d["status"]=="failed" and d["phase"]=="ready", d; assert d.get("result",{}).get("logs"), "no logs retained"' \
    && pass "deployment recorded failed at phase ready with logs retained" || fail "history/logs wrong"
fi

if want 5; then
  step "5. release failing after cutover is reverted automatically"
  D=$(build_release m1-d ', BREAK = "smoke", BREAK_AFTER = "3"'); echo "  release D = $D"
  st=$(deploy_status --release "$D")
  [[ $st == reverted ]] && pass "deploy D reverted after smoke failure" || fail "deploy D status: $st"
  wait_serving 10 && [[ $(live_commit) == m1-b ]] && pass "B serving again" || fail "B not restored"
  lwd events "$APP" --json | python3 -c 'import json,sys; ev=json.load(sys.stdin); assert any(e["kind"]=="deploy.reverted" for e in ev), ev[:3]' \
    && pass "deploy.reverted event recorded" || fail "no revert event"
fi

if want 6; then
  step "6. manual rollback to release A"
  A=${A:-$(cat /tmp/claude-1000/m1-release-a)}
  lwd rollback "$APP" "$ENV" --to "$A" >/dev/null
  [[ $(live_commit) == m1-a ]] && pass "A live after rollback --to $A" || fail "rollback did not restore A"
  [[ $(live_release) == "$A" ]] && pass "controller reports release $A live" || fail "controller live release mismatch"
fi

if want 7; then
  step "7. node reboot: serving returns with no controller action"
  ssh -o BatchMode=yes "ops@$NODE_IP" 'sudo systemctl reboot' || true
  sleep 15
  wait_serving 120 && [[ $(live_commit) == m1-a ]] && pass "A serving after reboot" || fail "not serving after reboot"
fi

if want 8; then
  step "8. controller outage: apps keep serving"
  systemctl --user stop lwd-controller
  sleep 2
  ok=1; for _ in 1 2 3 4 5; do [[ $(code "$API" /ready) == 200 ]] || ok=0; sleep 1; done
  systemctl --user start lwd-controller; sleep 2
  [[ $ok == 1 ]] && pass "served 5/5 checks with the controller stopped" || fail "serving depended on the controller"
fi

# Leave the manifest as committed in lwd-hello.
lwd app apply "$HELLO" >/dev/null
printf '\nM1 acceptance: all requested steps passed (%s)\n' "$STEPS"
