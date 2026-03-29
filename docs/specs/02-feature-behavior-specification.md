# OpenClaw Home Agent — Feature & Behavior Specification

**Version 1.0 — March 2026**

*This document details every behavior the system must exhibit. It is derived from the Product Specification and serves as the input to the Architecture Specification.*

---

## 1. Agent Lifecycle Behaviors

### 1.1 Startup & Initialization

- Each agent starts as a persistent, long-running process managed by the host's process supervisor (Kubernetes in Phase 1, launchd in Phase 2).
- On startup, each agent loads its system prompt, tool configuration, and workspace path from a per-agent configuration file.
- Agents do not share process space. Each runs in its own isolated context with its own memory, tools, and filesystem scope.
- If an agent crashes or is killed, the process supervisor restarts it automatically. The agent resumes from its last persisted state — it does not replay actions taken before the crash.
- Startup order is not dependent between agents. Any agent can start independently of the others.

### 1.2 Heartbeat Loop

- Each agent operates on a configurable heartbeat interval. On each tick, the agent checks its workspace for new content, evaluates pending tasks, and decides whether action is needed.
- Heartbeat intervals are tuned per agent domain: messaging checks frequently (every 10–15 minutes), media checks less often (hourly), calendar runs at morning/evening anchor points plus periodic checks, itinerary runs on demand or daily.
- If a heartbeat tick finds nothing to act on, the agent returns to idle with no output. Silent ticks are normal and expected.
- Heartbeat configuration is adjustable at runtime without restarting the agent.

### 1.3 Shutdown & Graceful Termination

- On receiving a shutdown signal, an agent completes any in-progress action before exiting. It does not start new actions after receiving the signal.
- In-progress write operations (filesystem, API calls) are completed or rolled back — never left in a partial state.
- Agents write a shutdown timestamp to their workspace on clean exit.

## 2. Messaging, Mail & Packages Agent

### 2.1 Email Monitoring

- Connects to configured email accounts via IMAP or Gmail API (MCP server).
- On each heartbeat, checks for new messages since the last processed timestamp.
- For each new message, produces a structured assessment: sender, subject, body summary, detected urgency level, suggested action (read, respond, ignore, flag).
- Urgency detection considers: known sender vs. unknown, keywords indicating deadlines or time sensitivity, reply-chain position, presence of attachments.
- Messages assessed as low priority are logged but do not generate user notifications.
- Messages assessed as high priority generate a notification to the user via the configured notification channel.

### 2.2 Email Response Drafting

- When the user requests a response to a specific message, the agent drafts a reply in the user's established voice and tone.
- Drafts are presented to the user for review. The agent does not send email without explicit user approval.
- The agent retains context from the message thread when drafting — it reads the full chain, not just the latest message.

### 2.3 Package Tracking

- Identifies shipping notification emails by sender, subject pattern, and body content.
- Extracts tracking numbers and carrier information from notification emails.
- Consolidates active shipments into a summary view: item description, carrier, tracking number, estimated delivery date, current status.
- On each heartbeat, checks for status updates on active shipments (via carrier tracking APIs or follow-up emails).
- Notifies the user when a package status changes to "out for delivery" or "delivered."
- Automatically archives tracking entries after confirmed delivery, with a configurable retention period.

### 2.4 SMS & Messaging Integration (Phase 2)

- Reads and sends iMessage and SMS via macOS Shortcuts and AppleScript integration.
- Applies the same triage logic as email: assess, prioritize, notify if urgent, draft responses when requested.
- Respects conversation-level mute/priority settings configured by the user.

## 3. Media Agent

### 3.1 Library Monitoring

- Connects to Plex and/or Jellyfin APIs to read library state.
- Detects new additions to the library and logs them.
- Identifies organizational issues: mismatched metadata, missing artwork, duplicate entries, items in wrong categories.
- Reports library health summary on request or on a scheduled basis.

### 3.2 Content Discovery & Recommendations

- Maintains a preference profile based on the user's stated interests and consumption history.
- Searches for new releases, upcoming premieres, and festival screenings matching the user's profile.
- Generates recommendations with reasoning — why this item matches the user's interests, not just that it does.
- Tracks recommendation history to avoid repeating suggestions the user has already seen or rejected.

### 3.3 Download & Queue Management

- Manages download queues for media acquisition (within the user's configured toolset).
- Monitors download progress and reports completion or failures.
- Moves completed downloads to the appropriate library location and triggers metadata refresh.

## 4. Calendar & Events Agent

### 4.1 Calendar Management

- Connects to Google Calendar (and/or other calendar providers) via MCP server.
- Reads all events across configured calendars. Writes only to calendars the user has explicitly authorized for write access.
- Detects scheduling conflicts: overlapping events, events without sufficient travel time between them, double-bookings across calendars.
- Presents daily and weekly schedule summaries at configured times (morning briefing, evening preview).

### 4.2 Invitation Processing

- Detects calendar invitations from email or direct calendar shares.
- Presents each invitation with context: who sent it, when, conflicts with existing events, travel time from the prior event, whether similar events have been accepted or declined in the past.
- Accepts or declines invitations only with explicit user approval.
- For tentative responses, tracks follow-up timing and reminds the user to finalize.

### 4.3 Event Suggestions

- Generates event ideas based on: user preferences, local event listings, seasonal opportunities, weather forecasts, past event attendance patterns.
- Suggestions include enough detail to be actionable: what, when, where, estimated cost, booking/ticket links if applicable.
- Frequency of suggestions is configurable. Suggestions are never pushed as notifications unless the user has opted into proactive suggestions.

### 4.4 Home Network & Security Monitoring

- Connects to UniFi Network and UniFi Protect APIs (read-only).
- Surfaces notable events: new devices on network, connectivity issues, motion alerts from cameras, doorbell events.
- Does not take automated action on network or security events — surfaces information for the user to act on.
- Aggregates events into a summary rather than forwarding every individual alert.

### 4.5 External System Navigation (Phase 2)

- Uses browser automation (Playwright) to navigate appointment booking systems, reservation platforms, and similar web interfaces on the user's behalf.
- Navigates IVR phone systems via voice telephony integration to handle hold queues and menu navigation.
- All actions in external systems that create commitments (bookings, reservations, appointments) require explicit user approval before confirmation.
- Captures confirmation numbers, receipts, and booking details, and routes them to the appropriate workspace.

## 5. Itinerary & Travel Agent

### 5.1 Trip Planning

- Generates trip proposals based on: destination preferences, budget range, travel dates, group size, activity interests, accommodation preferences.
- Proposals include: daily itineraries with timing, accommodation recommendations, transportation options, activity suggestions, estimated costs.
- Itineraries are presented as drafts for user review and iterative refinement.
- The agent researches options via web search and travel-related APIs/skills.

### 5.2 Active Trip Tracking

- Once a trip is confirmed, the agent monitors logistics: flight status changes, hotel confirmation validity, weather at destination, reservation times approaching.
- Sends proactive notifications for: flight delays, gate changes, check-in reminders, reservation reminders.
- Maintains a consolidated trip document with all confirmation numbers, addresses, and timing in one place.

### 5.3 Local & Day-Trip Suggestions

- Generates local outing ideas (restaurants, events, day trips) based on: current weather, day of week, user preferences, what's happening locally.
- Distinguishes between "planning ahead" suggestions (next weekend) and "right now" suggestions (today, spontaneous).
- Learns from acceptance/rejection patterns to improve future suggestions.

## 6. Glovebox Review Agent

### 6.1 Quarantine Notification

- Monitors the quarantine directory on each heartbeat.
- When new quarantined items are detected, messages the user with a review prompt.
- The review prompt includes metadata only: source (email, webhook, etc.), sender/origin, signals that triggered quarantine, content length, timestamp. Never the raw content.
- If multiple items are pending, presents them as a batch with individual release/reject options.

### 6.2 Release Flow

- When the user approves an item, the agent moves the sanitized content from quarantine to the destination agent's workspace.
- The destination agent then picks up the content on its next heartbeat, treating it as any other workspace item.
- The agent logs the release action with timestamp, user decision, and destination.

### 6.3 Reject Flow

- When the user rejects an item, the agent appends the item's metadata to the audit log (rejected.jsonl).
- The raw content is deleted from the quarantine directory.
- No copy of rejected content is retained — only the structured metadata record.

### 6.4 Safety Constraints

- The review agent never reads, summarizes, paraphrases, or displays the raw content of quarantined items. It operates exclusively on pre-computed metadata fields.
- The review agent has no email, browser, exec, or external API access. Its tool surface is limited to: filesystem read/write on quarantine and workspace directories, and user messaging.
- The review agent's system prompt explicitly prohibits acting on instructions found within quarantined content metadata.

## 7. Glovebox (Content Scanning Service)

### 7.1 Ingestion

- Connectors write incoming content to the glovebox staging directory. No other component reads from staging.
- Each item is written as a directory containing: the raw content file, and a metadata file with source, sender, timestamp, and intended destination agent.
- The intended destination is determined by the connector at delivery time (source-based routing). The glovebox does not make routing decisions based on content.

### 7.2 Heuristic Scanning (Phase 1)

- On detecting a new item in staging, the glovebox runs the heuristic ruleset against the content.
- Heuristic rules detect:
  - Explicit instruction override patterns: "ignore previous instructions," "disregard your instructions," "new directive," "system prompt," and variants.
  - Role/persona reassignment attempts: "you are now," "act as," "pretend you are," and variants.
  - Tool invocation syntax appearing in content: `exec:`, `<tool>`, function-call-shaped strings, shell command patterns.
  - Unusual encoding: base64-encoded blocks in plain text contexts, excessive unicode escapes, zero-width characters.
  - Prompt template structure: content that resembles a system prompt or instruction block rather than natural communication.
  - Non-English linguistic content: detected as a signal (weight booster), not a standalone trigger. Non-English content alone passes with a metadata flag. Non-English content combined with any other signal triggers quarantine.
- Each signal has a weight. Signals are additive. A single strong signal or multiple weak signals exceeding the threshold trigger quarantine.

### 7.3 Verdict & Routing

- **PASS**: No signals detected, or signals below threshold. Content is moved from staging to the destination agent's workspace directory. Metadata is appended to the pass log.
- **QUARANTINE**: One or more signals detected above threshold. Content is moved to the quarantine directory as a timestamped, hashed subdirectory containing a sanitized content representation and a metadata file with full signal details. A notification entry is written to the shared notification file for the review agent.
- **REJECT** (Phase 1): Reserved for structural failures only — malformed content that cannot be parsed, or source authentication failures. Metadata logged, content deleted. Phase 2 expands REJECT to include high-confidence classifier verdicts.
- The glovebox never modifies content. It reads, assesses, and routes.

### 7.4 Audit Logging

- Every item processed by the glovebox is logged with: timestamp, source, sender, content hash, content length, signals detected (if any), verdict, destination.
- Rejected/quarantined items include additional detail: which specific heuristic rules fired, signal weights, and (in Phase 2) classifier confidence and reasoning.
- Logs are append-only. The glovebox process has no delete access to the audit log.
- Hostile content itself is not retained in logs — only enough metadata to reconstruct the pattern for analysis and reporting.

### 7.5 LLM Classifier (Phase 2)

- Items that pass the heuristic pre-filter are additionally evaluated by a locally-hosted LLM running in classification-only mode.
- The classifier LLM has no tools, no memory, no action surface. It receives the content and returns a structured verdict: pass/quarantine/reject with confidence score and reasoning.
- The classifier's system prompt is narrowly scoped to prompt injection detection. It does not assess content quality, relevance, or spam characteristics.
- Classifier verdicts augment (not replace) heuristic signals. A heuristic QUARANTINE is never downgraded by the classifier.

## 8. Inter-Agent Coordination

### 8.1 Shared Filesystem

- A shared directory is readable by all agents. Each agent writes only to its own subdirectory within the shared space.
- Cross-domain events (e.g., concert ticket detected in email that is relevant to calendar) are communicated by the originating agent writing a structured entry to its shared subdirectory.
- Consuming agents poll the shared directory on their heartbeat and process entries relevant to their domain.
- Entries in the shared space are structured (JSON or Markdown with front matter) with a consistent schema: event type, timestamp, source agent, payload, and whether the entry has been acknowledged.

### 8.2 No Direct Agent-to-Agent Messaging (Phase 1)

- Agents do not send messages to each other in Phase 1. All coordination flows through the shared filesystem.
- Direct inter-agent messaging (via OpenClaw sessions_send or equivalent) is deferred to Phase 2 and only adopted if shared-file coordination proves insufficient.

### 8.3 Human as Router

- For ambiguous cross-domain items, the default behavior is to surface the item to the user and let them decide which agent should handle it.
- Agents do not autonomously delegate work to other agents.

## 9. User Interaction Behaviors

### 9.1 Notification Model

- Agents notify the user through a single configured notification channel (Phase 1: likely a messaging platform or webhook; Phase 2: iMessage, push notifications).
- Notifications are batched where possible. The system does not send a separate notification for every email or event — it aggregates and surfaces summaries at configured intervals.
- Urgent items (time-sensitive, high-priority) break through batching and notify immediately.
- The user can configure quiet hours during which only urgent notifications are delivered.

### 9.2 Approval Gates

- Any action with real-world consequences requires explicit user approval before execution. This includes: sending email, accepting/declining calendar invitations, making reservations or bookings, releasing quarantined content, modifying external systems.
- The agent presents the proposed action with full context and waits for a yes/no response.
- Approval requests that go unacknowledged for a configurable timeout are escalated (re-notified) once, then logged as timed out. The agent does not take the action.

### 9.3 Conversational Interaction

- The user can initiate a conversation with any agent at any time to ask questions, give instructions, or request status updates.
- Agents respond within their domain scope. If asked about something outside their domain, they indicate which agent would handle it rather than attempting to answer.
- Agents maintain conversational context within a session but do not carry conversational context across sessions. Persistent knowledge lives in memory files, not conversation history.

### 9.4 Briefings & Summaries

- The system produces scheduled briefings: a morning summary (overnight messages, today's calendar, active shipments, weather) and an evening preview (tomorrow's calendar, pending items).
- Briefings are consolidated across agents into a single output. The orchestration for briefing assembly is defined in the architecture spec.
- Briefing format and content are configurable. The user can add or remove sections.

## 10. Memory & State

### 10.1 Per-Agent Memory

- Each agent maintains its own memory files in its private workspace directory.
- Memory is stored as Markdown files, human-readable and auditable.
- Memory retrieval uses hybrid search with MMR (maximal marginal relevance) and temporal decay to surface relevant context without redundancy.
- A local embedding server provides vector embeddings for memory retrieval with no external API dependency.

### 10.2 Shared State

- The shared filesystem serves as the coordination layer for cross-agent state.
- Shared state is append-oriented. Agents do not modify each other's shared entries — they write acknowledgment entries in their own subdirectory.

### 10.3 Knowledge Graph (Phase 2)

- A knowledge graph layer (Cognee) indexes memory files and provides relationship-based retrieval.
- The knowledge graph augments, not replaces, the Markdown memory files. Files remain the source of truth.
- Graph extraction runs against a local LLM to maintain privacy.

## 11. Security Behaviors

### 11.1 Tool Surface Isolation

- Each agent is configured with an explicit tool allowlist. Tools not on the list are unavailable to that agent, regardless of what the agent's system prompt says.
- Tool profiles are defined per agent: messaging has email tools but no exec; media has library APIs but no email; calendar has calendar APIs but no exec; itinerary has web search but no email send; review has filesystem only.
- No agent has unrestricted tool access. Exec (shell command execution) is disabled for all agents in Phase 1.

### 11.2 Filesystem Isolation

- Each agent has write access only to its own workspace directory and its own subdirectory within the shared space.
- Each agent has read access to the shared space (all subdirectories) and its own workspace. No agent can read another agent's private workspace.
- The glovebox review agent additionally has read access to the quarantine directory and write access to destination agent workspaces (for content release).
- Filesystem permissions are enforced at the OS/container level, not by the agent's system prompt.

### 11.3 Egress Control

- Outbound network access is restricted per agent. Each agent can only reach the specific external services it needs (e.g., messaging agent can reach Gmail API but not arbitrary URLs).
- Default-deny egress policy: anything not explicitly allowed is blocked.
- Egress rules are enforced at the network level (Cilium CNI in Phase 1, macOS firewall rules or application-level controls in Phase 2).

### 11.4 Secrets Management

- Secrets (API keys, tokens, credentials) are injected into agent processes at startup via a secrets management system. They are never stored in agent configuration files, memory, or the shared filesystem.
- Phase 1: 1Password Connect + External Secrets Operator in Kubernetes.
- Phase 2: 1Password CLI (`op run`) for secret injection into launchd processes.

### 11.5 Monitoring & Audit

- All agent actions are logged: tool invocations, API calls, filesystem operations, user interactions.
- Glovebox scanning results are logged separately with full signal detail.
- Logs are shipped to a monitoring stack (Prometheus/Grafana in Phase 1) for dashboarding and alerting.
- A scheduled security audit reviews: tool invocation patterns, egress traffic, glovebox false positive/negative rates, any anomalous agent behavior.

## 12. Backup & Recovery

### 12.1 What Is Backed Up

- Agent configuration files (system prompts, tool profiles, heartbeat settings).
- Agent memory files (all workspace Markdown files).
- Glovebox heuristic rules and audit logs.
- Shared filesystem contents.
- Secrets references (not the secrets themselves — those live in 1Password).

### 12.2 Backup Target

- Primary backup to Synology NAS on the local network.
- Secondary backup to Backblaze B2 for off-site redundancy.
- Backup frequency: daily for memory and configuration, hourly for audit logs.

### 12.3 Recovery

- Full system rebuild from backup is documented in a runbook.
- Recovery restores agent state to the last backup point. Any actions taken between the last backup and the failure are not replayed — the agents resume from restored state and pick up new work on their next heartbeat.
- Recovery time objective: under 2 hours for a full rebuild from backup on clean hardware.
