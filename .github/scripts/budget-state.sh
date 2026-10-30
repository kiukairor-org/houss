#!/usr/bin/env bash
# Claude budget gate (DECISIONS D62): read the state the homie-catcher publishes in the open issue labelled
# `budget-status` in kiukairor-org/homie-catcher (public). Writes state=ok|paused|limit to $GITHUB_OUTPUT.
# Fails OPEN (ok) if the issue can't be read, or if a non-ok state is older than 2 h (catcher down).
set -uo pipefail
q='repos/kiukairor-org/homie-catcher/issues?labels=budget-status&state=open&per_page=1'
body=$(gh api "$q" --jq '.[0].body // ""' 2>/dev/null || curl -fsS "https://api.github.com/$q" 2>/dev/null | jq -r '.[0].body // ""' || true)
json=$(printf '%s' "$body" | sed -n 's/.*<!-- homie-budget \(.*\) -->.*/\1/p' | head -1)
state=$(printf '%s' "$json" | jq -r '.state // "ok"' 2>/dev/null || echo ok)
updated=$(printf '%s' "$json" | jq -r '.updated // 0 | floor' 2>/dev/null || echo 0)
case "$state" in ok|paused|limit) ;; *) state=ok ;; esac
if [ "$state" != ok ] && [ $(( $(date +%s) - updated )) -gt 7200 ]; then
  echo "::warning::budget status is stale (catcher down?); ignoring it"
  state=ok
fi
echo "state=$state" >> "$GITHUB_OUTPUT"
echo "Claude budget state: $state"
