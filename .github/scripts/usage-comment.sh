#!/usr/bin/env bash
# Report this run's Claude usage to the homie-catcher (DECISIONS D61): the last element of the action's
# execution_file is {"type":"result", total_cost_usd, usage, duration_ms, num_turns, ...}. Posted as a
# hidden marker in a small comment on the issue/PR; the catcher polls comments (no inbound path).
# Env: GH_TOKEN, FILE, NUM, WF (agent|review), RUN. Never fails the job.
set -uo pipefail
[ -n "${FILE:-}" ] && [ -f "$FILE" ] || exit 0
summary=$(jq -c --arg wf "$WF" --arg run "$RUN" '
  [.[] | select(type == "object" and .type == "result")] | last // empty |
  {wf: $wf, run: $run, cost: (.total_cost_usd // 0), in: (.usage.input_tokens // 0), out: (.usage.output_tokens // 0),
   cr: (.usage.cache_read_input_tokens // 0), cc: (.usage.cache_creation_input_tokens // 0), ms: (.duration_ms // 0),
   turns: (.num_turns // 0), err: (.is_error // false), api: (.api_error_status // null),
   text: ((.result // "") | tostring | .[0:300] | gsub("--"; "- -"))}' "$FILE" 2>/dev/null) || exit 0
[ -n "$summary" ] || exit 0
cost=$(jq -r '.cost' <<<"$summary"); turns=$(jq -r '.turns' <<<"$summary")
printf '<sub>Claude usage (%s run %s): ≈$%s equivalent, %s turns</sub>\n<!-- homie-usage %s -->\n' \
  "$WF" "$RUN" "$cost" "$turns" "$summary" > usage.md
gh issue comment "$NUM" -R "$GITHUB_REPOSITORY" --body-file usage.md || echo "::warning::could not post usage"
