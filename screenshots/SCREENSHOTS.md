# Screenshots

A visual tour of the Arbetern UI.

## Home

The landing page opens on the **integrations** section — every configured
integration with its live permission / auth state.

![Home — integrations](home_integrations.png)

Scrolling down reveals the **agents** roster alongside the changelog feed.

![Home — agents](home_agents.png)

Clicking an agent card expands its **prompt set** — one panel per prompt
(general, security, debug, etc.) with the exact text sent to the LLM.

![Home — agent card](home_agent_card.png)

## Dashboards

A dashboard view — scheduled snapshots of an agent's data sources rendered
as Markdown and refreshed on the cadence configured at creation time.

![Dashboard](dashboard.png)

## Workflows — Grid

The workflow grid for an agent: each card shows the schedule, last-run
status, and the live "running" badge while a tick is in flight.

![Workflow grid](workflow_grid.png)

Opening a workflow card reveals per-run history, prompt, and the detected
source/output integrations.

![Workflow grid — expanded](workflow_grid_2.png)

## Workflow Editor

Editing a workflow: prompt, schedule (`cron` UTC expression), trigger
type, and the auto-detected source/output integration flow diagram.

![Workflow editor](workflow_edit.png)


