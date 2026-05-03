---
tracker:
  kind: memory
  active_states:
    - "In Progress"
    - "Todo"
  terminal_states:
    - "Done"
    - "Cancelled"

polling:
  interval_ms: 1000

workspace:
  root: /tmp/symphony_workspaces

hooks:
  after_create: ""
  before_run: ""
  after_run: ""
  before_remove: ""
  # Optional: run golangci-lint between agent turns. Stdout from a non-zero exit
  # is fed into the next turn's continuation prompt under a "Validation feedback"
  # heading. The run never aborts on hook failure alone.
  # between_turns: |
  #   golangci-lint run --out-format=line-number
  timeout_ms: 60000

agent:
  runtime: mock
  max_concurrent_agents: 5
  max_turns: 10
  max_retry_backoff_ms: 300000
---
You are working on issue {{ issue.identifier }}.

Title: {{ issue.title }}

Description:
{{ issue.description }}

Please complete the work described above.
