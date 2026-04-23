# v0.9 Stacks Integration

## Context
tfpilot v0.7 is shipped. This plan adds Terraform Stacks awareness to the agent — stack discovery, component and deployment topology, stack health, and guidance on when to use Stacks vs workspaces.

Live org for testing: sarah-test-org
Auth: --auth=copilot or source .env for ANTHROPIC_API_KEY
All changes must be synced from aiki workspace to main repo path before building

Key Stacks concepts the agent must understand:
- Stack: a complete unit of infrastructure composed of components and deployments
- Component: an abstraction around a Terraform module (.tfcomponent.hcl files)
- Deployment: an instance of all components with specific inputs (.tfdeploy.hcl files)
- Stacks live inside projects, like workspaces
- Max 20 deployments per stack
- Stacks do NOT support: policy as code, drift detection, Explorer, run tasks (GA limitations)
- Stacks DO support: deferred changes, deployment orchestration, linked stacks, workload identity

## Goal
The agent can answer questions about an org's Stacks topology, compare Stacks vs workspaces, explain component and deployment health, and advise on when to use Stacks vs workspaces.

## Implementation

### 1. New tools in internal/tools/tools.go

#### _hcp_tf_stacks_list
Parameters: org
Command: hcptf stack list -org=<org> -output=json
Returns: list of stacks with id, name, project, status, deployment count
Apply HTML error guard. Add to Definitions() and route in Call().

#### _hcp_tf_stack_describe
Parameters: org, stack_id
Steps (parallel):
- hcptf stack list -org=<org> -output=json (to get stack metadata)
- hcptf stack configuration list -stack-id=<stack_id> -output=json
- hcptf stack deployment list -stack-id=<stack_id> -output=json (use hcptf stack list and filter, or equivalent)
Returns:
{
  "stack_id": "stk-xxx",
  "name": "...",
  "project": "...",
  "configuration_count": N,
  "deployments": [
    {
      "name": "production",
      "status": "applied|errored|planning|...",
      "deployment_group": "...",
      "last_updated": "relative timestamp"
    }
  ],
  "deployment_count": N,
  "health": "Healthy|Degraded|Unknown",
  "limitations": ["no policy as code", "no drift detection", "no run tasks"]
}
Health is Healthy if all deployments applied successfully, Degraded if any errored, Unknown if no deployments exist.
Apply HTML error guard. Add to Definitions() and route in Call().

#### _hcp_tf_stack_vs_workspace
Parameters: org, use_case (string describing what the user wants to do)
This is a reasoning tool — it does NOT call hcptf. Instead it returns a structured recommendation:
{
  "recommendation": "stack|workspace|either",
  "reasoning": "Plain English explanation",
  "use_stack_when": ["list of conditions that favor stacks"],
  "use_workspace_when": ["list of conditions that favor workspaces"],
  "key_limitations": ["stacks limitations relevant to this use case"]
}

Decision logic (encode as Go constants, not LLM reasoning):
Use Stack when use_case contains any of: "multiple environments", "repeat", "multi-region", "scale", "orchestrat", "deploy same", "kubernetes", "k8s", "deferred"
Use Workspace when use_case contains any of: "policy", "sentinel", "opa", "drift", "run task", "explorer", "no-code", "single environment", "simple"
Use Either when no strong signal either way.

Always include in limitations when recommending stacks:
- "Stacks do not support policy as code (Sentinel/OPA)"
- "Stacks do not support drift detection"
- "Stacks do not support run tasks"
- "Maximum 20 deployments per stack"

Add to Definitions() and route in Call().

### 2. New slash command /stacks in internal/repl/repl.go
/stacks — calls _hcp_tf_stacks_list for the pinned org and prints:

  Stacks in <org>:
  
  • <stack-name> (<project>)    <deployment-count> deployments    <health>

If no stacks found, print:
  No stacks found in <org>. Stacks are used for repeated infrastructure across environments or regions.
  Learn more: https://developer.hashicorp.com/terraform/cloud-docs/stacks

### 3. System prompt update in internal/agent/agent.go
Add to rules:
- "To list all stacks in the org, call _hcp_tf_stacks_list."
- "To describe a specific stack's components and deployments, call _hcp_tf_stack_describe with the stack_id."
- "When a user asks whether to use Stacks or workspaces, call _hcp_tf_stack_vs_workspace with their use case as the use_case parameter."
- "Always surface Stacks limitations when recommending Stacks: no policy as code, no drift detection, no run tasks, max 20 deployments."
- "Stacks are best for repeated infrastructure across environments, regions, or accounts. Workspaces are best when policy as code or drift detection are required."
- "Never confuse Stack deployments with HCP Terraform workspace runs — they are different concepts."

### 4. ROADMAP.md update
Mark v0.9 as in progress. Add to the v0.9 section what's being built.

### Testing
After implementation, sync to main repo and run:

1. go build -o tfpilot ./cmd/tfpilot
2. go test ./... — all green

3. /stacks slash command test:
   ./tfpilot --org=sarah-test-org --auth=copilot
   /stacks
   Expected: list of stacks in sarah-test-org, or "No stacks found" with guidance

4. Agent-driven stack discovery:
   Ask: "What stacks do we have?"
   Expected: agent calls _hcp_tf_stacks_list, describes what it finds

5. Stack vs workspace guidance:
   Ask: "Should I use a Stack or workspace to deploy my app to 3 regions?"
   Expected: agent calls _hcp_tf_stack_vs_workspace, recommends Stack, surfaces limitations

   Ask: "Should I use a Stack or workspace if I need Sentinel policies?"
   Expected: agent recommends Workspace, explains Stacks don't support policy as code yet

6. Post full REPL output as aiki task comment before closing

## Acceptance criteria
- go build exits 0
- go test ./... all green
- _hcp_tf_stacks_list returns stack list or empty gracefully
- _hcp_tf_stack_describe returns health, deployment list, and limitations
- _hcp_tf_stack_vs_workspace returns correct recommendation for multi-region (stack) and policy (workspace) use cases
- /stacks slash command works and shows guidance when no stacks found
- Agent correctly advises on Stacks vs workspaces
- Agent always surfaces Stacks GA limitations when recommending Stacks
- Commit: "feat: v0.9 Stacks integration with topology, health, and stack vs workspace guidance"
- Push to main
- Sync aiki workspace to main repo before building
- Show git diff --stat before committing
- Do not close epic until all criteria pass
