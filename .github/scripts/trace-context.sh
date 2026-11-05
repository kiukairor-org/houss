#!/usr/bin/env bash
# D80: the pipeline item's W3C trace context for Claude Code's built-in OpenTelemetry. The catcher writes
# `<!-- homie-trace 00-<trace>-<span>-01 -->` into the issue body; for a PR, follow "Closes #N" to that issue.
# Env: GH_TOKEN, NUM (issue or PR number). Output: traceparent=<value or empty>. Never fails the job.
set -uo pipefail
re='<!-- homie-trace (00-[0-9a-f]{32}-[0-9a-f]{16}-0[01]) -->'
body_of() { gh api "repos/$GITHUB_REPOSITORY/issues/$1" --jq '.body // ""' 2>/dev/null || true; }
tp=""
body=$(body_of "${NUM:-0}")
if [[ $body =~ $re ]]; then
  tp=${BASH_REMATCH[1]}
else
  for n in $(grep -oiE '(close[sd]?|fix(e[sd])?|resolve[sd]?) #[0-9]+' <<<"$body" | grep -oE '[0-9]+' | head -3); do
    b=$(body_of "$n")
    if [[ $b =~ $re ]]; then tp=${BASH_REMATCH[1]}; break; fi
  done
fi
echo "traceparent=$tp" >> "$GITHUB_OUTPUT"
echo "pipeline trace context: ${tp:-none (not a pipeline item)}"
