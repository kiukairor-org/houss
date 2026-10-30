#!/usr/bin/env bash
# Escalation gate for the reviewer, enforced in code BEFORE Claude runs.
# Runs from the default branch (workflow_run), never from PR code. Reads the PR via the API only.
# Env: GH_TOKEN, GH_REPO, PR, HEAD_SHA, AUTO_MERGE_ENABLED, MAX_CHANGED_LINES (default 400).
# Writes `escalate=true|false` and `reason=...` to $GITHUB_OUTPUT.
set -euo pipefail
MAX="${MAX_CHANGED_LINES:-400}"
reasons=()

pr_json=$(gh api "repos/$GH_REPO/pulls/$PR")
state=$(jq -r .state <<<"$pr_json")
head=$(jq -r .head.sha <<<"$pr_json")
labels=$(jq -r '[.labels[].name] | join(",")' <<<"$pr_json")
changed=$(jq -r '.additions + .deletions' <<<"$pr_json")

[ "$state" = open ] || { echo "escalate=skip" >> "$GITHUB_OUTPUT"; echo "PR not open"; exit 0; }
[ "$head" = "$HEAD_SHA" ] || { echo "escalate=skip" >> "$GITHUB_OUTPUT"; echo "stale CI run (head moved)"; exit 0; }

[ "${AUTO_MERGE_ENABLED:-}" = "true" ] || reasons+=("kill switch: org variable AUTO_MERGE_ENABLED is not 'true'")
case ",$labels," in *,hold,*) reasons+=("label 'hold' is set") ;; esac
case ",$labels," in *,fix-round-2,*) reasons+=("the PR has already used its 2 fix rounds") ;; esac
[ "$changed" -le "$MAX" ] || reasons+=("diff is $changed changed lines (> $MAX)")

# Sensitive paths: CI/workflows, ownership, agent instructions, secrets/RBAC/permissions in deploy/.
files=$(gh api --paginate "repos/$GH_REPO/pulls/$PR/files" --jq '.[].filename')
while IFS= read -r f; do
  [ -n "$f" ] || continue
  case "$f" in
    .github/*)                  reasons+=("changes CI/automation: $f") ;;
    CODEOWNERS|*/CODEOWNERS)    reasons+=("changes CODEOWNERS: $f") ;;
    CLAUDE.md|.claude/*)        reasons+=("changes agent instructions: $f") ;;
  esac
  if [[ "$f" == deploy/* ]] && grep -qiE '(secret|sealed|rbac|role|serviceaccount|networkpolic|psp|permission)' <<<"$f"; then
    reasons+=("changes secrets/RBAC/permissions in deploy/: $f")
  fi
done <<<"$files"
# Content check for deploy/: new or edited permission-bearing kinds.
patches=$(gh api --paginate "repos/$GH_REPO/pulls/$PR/files" --jq '.[] | select(.filename | startswith("deploy/")) | .patch // ""')
if grep -qE '^\+.*kind:[[:space:]]*(Secret|SealedSecret|Role|ClusterRole|RoleBinding|ClusterRoleBinding|ServiceAccount|NetworkPolicy)\b' <<<"$patches"; then
  reasons+=("adds/edits Secret, RBAC, ServiceAccount or NetworkPolicy objects in deploy/")
fi
if grep -qE '^\+.*(hostNetwork|privileged|hostPath|automountServiceAccountToken:[[:space:]]*true)' <<<"$patches"; then
  reasons+=("adds privileged pod settings in deploy/")
fi

if [ "${#reasons[@]}" -gt 0 ]; then
  {
    echo "escalate=true"
    echo "reason<<EOF"; printf -- '- %s\n' "${reasons[@]}"; echo "EOF"
  } >> "$GITHUB_OUTPUT"
  printf 'ESCALATE:\n'; printf -- '- %s\n' "${reasons[@]}"
else
  echo "escalate=false" >> "$GITHUB_OUTPUT"
  echo "no escalation rule applies"
fi
