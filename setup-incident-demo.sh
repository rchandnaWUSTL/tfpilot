#!/bin/bash
# setup-incident-demo.sh — stages prod-incident-demo for the tfpilot incident response demo
# Run once before the demo. Re-run to reset to a clean broken state.
# Usage: ./setup-incident-demo.sh

set -euo pipefail

ORG="sarah-test-org"
WORKSPACE="prod-incident-demo"
TFC_TOKEN=$(cat ~/.terraform.d/credentials.tfrc.json | python3 -c 'import json,sys; print(json.load(sys.stdin)["credentials"]["app.terraform.io"]["token"])')
API="https://app.terraform.io/api/v2"

echo "==> Setting up incident demo environment"
echo "    org: $ORG"
echo "    workspace: $WORKSPACE"
echo ""

auth_header() {
  echo "Authorization: Bearer $TFC_TOKEN"
}

# Step 1 — Get or create workspace
echo "==> Step 1: Getting or creating workspace $WORKSPACE..."
WS_RESPONSE=$(curl -s \
  -H "$(auth_header)" \
  -H "Content-Type: application/vnd.api+json" \
  "$API/organizations/$ORG/workspaces/$WORKSPACE")

WS_ID=$(echo "$WS_RESPONSE" | python3 -c "import json,sys; d=json.load(sys.stdin); print(d['data']['id'])" 2>/dev/null || echo "")

if [ -z "$WS_ID" ]; then
  echo "    Creating workspace..."
  WS_RESPONSE=$(curl -s \
    -X POST \
    -H "$(auth_header)" \
    -H "Content-Type: application/vnd.api+json" \
    "$API/organizations/$ORG/workspaces" \
    -d "{
      \"data\": {
        \"type\": \"workspaces\",
        \"attributes\": {
          \"name\": \"$WORKSPACE\",
          \"execution-mode\": \"remote\",
          \"auto-apply\": false,
          \"terraform-version\": \"1.2.9\"
        }
      }
    }")
  WS_ID=$(echo "$WS_RESPONSE" | python3 -c "import json,sys; print(json.load(sys.stdin)['data']['id'])")
fi
echo "    Workspace ID: $WS_ID"

upload_config_and_run() {
  local dir="$1"
  local message="$2"

  echo "    Creating config version..."
  CV_RESPONSE=$(curl -s \
    -X POST \
    -H "$(auth_header)" \
    -H "Content-Type: application/vnd.api+json" \
    "$API/workspaces/$WS_ID/configuration-versions" \
    -d '{
      "data": {
        "type": "configuration-versions",
        "attributes": {
          "auto-queue-runs": false
        }
      }
    }')
  CV_ID=$(echo "$CV_RESPONSE" | python3 -c "import json,sys; print(json.load(sys.stdin)['data']['id'])")
  UPLOAD_URL=$(echo "$CV_RESPONSE" | python3 -c "import json,sys; print(json.load(sys.stdin)['data']['attributes']['upload-url'])")
  echo "    Config version ID: $CV_ID"

  echo "    Uploading config from $dir..."
  TMPTAR=$(mktemp /tmp/tfconfig.XXXXXX.tar.gz)
  tar -czf "$TMPTAR" -C "$dir" .
  curl -s \
    -X PUT \
    -H "Content-Type: application/octet-stream" \
    --data-binary "@$TMPTAR" \
    "$UPLOAD_URL"
  rm -f "$TMPTAR"

  echo "    Waiting for config version to be ready..."
  for i in $(seq 1 10); do
    STATUS=$(curl -s \
      -H "$(auth_header)" \
      "$API/configuration-versions/$CV_ID" | python3 -c "import json,sys; print(json.load(sys.stdin)['data']['attributes']['status'])")
    if [ "$STATUS" = "uploaded" ]; then break; fi
    sleep 2
  done
  echo "    Config version status: $STATUS"

  echo "    Creating run: $message..."
  RUN_RESPONSE=$(curl -s \
    -X POST \
    -H "$(auth_header)" \
    -H "Content-Type: application/vnd.api+json" \
    "$API/runs" \
    -d "{
      \"data\": {
        \"type\": \"runs\",
        \"attributes\": {
          \"message\": \"$message\",
          \"auto-apply\": false,
          \"plan-only\": false
        },
        \"relationships\": {
          \"workspace\": {
            \"data\": { \"type\": \"workspaces\", \"id\": \"$WS_ID\" }
          },
          \"configuration-version\": {
            \"data\": { \"type\": \"configuration-versions\", \"id\": \"$CV_ID\" }
          }
        }
      }
    }")
  RUN_ID=$(echo "$RUN_RESPONSE" | python3 -c "import json,sys; print(json.load(sys.stdin)['data']['id'])")
  echo "    Run ID: $RUN_ID"

  echo "    Waiting for plan to complete..."
  for i in $(seq 1 60); do
    RUN_STATUS=$(curl -s \
      -H "$(auth_header)" \
      "$API/runs/$RUN_ID" | python3 -c "import json,sys; print(json.load(sys.stdin)['data']['attributes']['status'])")
    echo "    Status: $RUN_STATUS"
    if [ "$RUN_STATUS" = "planned" ] || [ "$RUN_STATUS" = "planned_and_finished" ] || [ "$RUN_STATUS" = "cost_estimated" ] || [ "$RUN_STATUS" = "policy_checked" ]; then break; fi
    if [ "$RUN_STATUS" = "errored" ] || [ "$RUN_STATUS" = "canceled" ]; then
      echo "    Run failed with status: $RUN_STATUS"
      return 1
    fi
    sleep 3
  done

  if [ "$RUN_STATUS" = "planned_and_finished" ]; then
    echo "    No-op plan — nothing to apply."
    return 0
  fi

  echo "    Applying run..."
  curl -s \
    -X POST \
    -H "$(auth_header)" \
    -H "Content-Type: application/vnd.api+json" \
    "$API/runs/$RUN_ID/actions/apply" \
    -d '{"data": {"attributes": {"comment": "setup-incident-demo"}}}' > /dev/null

  echo "    Waiting for apply to complete..."
  for i in $(seq 1 60); do
    RUN_STATUS=$(curl -s \
      -H "$(auth_header)" \
      "$API/runs/$RUN_ID" | python3 -c "import json,sys; print(json.load(sys.stdin)['data']['attributes']['status'])")
    echo "    Status: $RUN_STATUS"
    if [ "$RUN_STATUS" = "applied" ]; then break; fi
    if [ "$RUN_STATUS" = "errored" ] || [ "$RUN_STATUS" = "canceled" ]; then
      echo "    Apply failed with status: $RUN_STATUS"
      return 1
    fi
    sleep 3
  done
  echo "    Run complete: $RUN_STATUS"
}

# Step 2 — Clean baseline
echo ""
echo "==> Step 2: Applying clean baseline (v1.0.0)..."
CLEAN_DIR=$(mktemp -d)
cat > "$CLEAN_DIR/main.tf" << 'EOF'
terraform {
  required_version = "~> 1.2.0"
  required_providers {
    null = {
      source  = "hashicorp/null"
      version = "~> 3.1"
    }
  }
}

resource "null_resource" "app_server" {
  triggers = { version = "1.0.0" }
}

resource "null_resource" "database" {
  triggers = { version = "1.0.0" }
}

resource "null_resource" "load_balancer" {
  triggers = { version = "1.0.0" }
}
EOF
upload_config_and_run "$CLEAN_DIR" "tfpilot demo: clean baseline v1.0.0"
rm -rf "$CLEAN_DIR"

# Step 3 — Bad deploy
echo ""
echo "==> Step 3: Applying bad deploy (v2.0.0)..."
BROKEN_DIR=$(mktemp -d)
cat > "$BROKEN_DIR/main.tf" << 'EOF'
terraform {
  required_version = "~> 1.2.0"
  required_providers {
    null = {
      source  = "hashicorp/null"
      version = "~> 3.1"
    }
  }
}

resource "null_resource" "app_server" {
  triggers = { version = "2.0.0" }
}

resource "null_resource" "database" {
  triggers = { version = "2.0.0" }
}

resource "null_resource" "load_balancer" {
  triggers = { version = "2.0.0" }
}

resource "null_resource" "cache_server" {
  triggers = { version = "2.0.0" }
}
EOF
upload_config_and_run "$BROKEN_DIR" "tfpilot demo: bad deploy v2.0.0 (adds cache_server)"
rm -rf "$BROKEN_DIR"

echo ""
echo "==> Incident demo environment ready."
echo ""
echo "    Workspace prod-incident-demo now has:"
echo "    - Clean baseline run (v1.0.0, 3 resources)"
echo "    - Bad deploy on top (v2.0.0, 4 resources — adds cache_server)"
echo ""
echo "    Demo flow:"
echo "    ~/Documents/GitHub/tfpilot/tfpilot --org=sarah-test-org --workspace=prod-incident-demo --auth=copilot --apply"
echo "    > something is wrong in prod"
echo "    > is it safe to revert?"
echo "    > yes"
echo "    > revert it and generate an incident summary"
echo "    > yes"
echo ""
echo "    To reset: run this script again."
