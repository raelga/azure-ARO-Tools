#!/usr/bin/env bash
set -euo pipefail
N=324
REPO=Azure/ARO-Tools
BASELINE_FAIL=""

echo "watching PR #$N in $REPO"
while :; do
  state=$(gh pr view "$N" --repo "$REPO" --json state -q .state)
  if [ "$state" = "MERGED" ] || [ "$state" = "CLOSED" ]; then
    echo "PR reached terminal state: $state"
    exit 0
  fi

  checks_json=$(gh pr checks "$N" --repo "$REPO" --json name,state,bucket 2>/dev/null || echo '[]')
  fails=$(echo "$checks_json" | jq -r '.[] | select(.bucket=="fail") | .name' | sort | tr '\n' ',' )
  pending=$(echo "$checks_json" | jq -r '.[] | select(.bucket=="pending") | .name' | wc -l)

  if [ -n "$fails" ] && [ "$fails" != "$BASELINE_FAIL" ]; then
    echo "NEW FAILING CHECKS: $fails"
    exit 2
  fi

  # unresolved review threads
  unresolved=$(gh api graphql -f query='
  { repository(owner:"Azure",name:"ARO-Tools"){ pullRequest(number:'"$N"'){
      reviewThreads(first:50){ nodes{ isResolved } } } } }' \
    --jq '[.data.repository.pullRequest.reviewThreads.nodes[] | select(.isResolved==false)] | length')
  if [ "$unresolved" != "0" ]; then
    echo "NEW UNRESOLVED REVIEW THREAD(S): $unresolved"
    exit 3
  fi

  if [ -z "$fails" ] && [ "$pending" = "0" ]; then
    lgtm=$(gh pr view "$N" --repo "$REPO" --json labels --jq '[.labels[].name] | index("lgtm") != null')
    echo "all checks green, no pending, lgtm=$lgtm"
    if [ "$lgtm" = "true" ]; then
      echo "lgtm present - should merge via tide shortly"
      exit 4
    fi
  fi

  sleep 150
done
