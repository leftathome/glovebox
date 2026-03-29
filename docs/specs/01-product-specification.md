# OpenClaw Home Agent — Product Specification

**Version 1.0 — March 2026**

---

## 1. What This Is

A self-hosted, always-on AI assistant that runs on dedicated hardware in a residential environment. The system manages daily tasks across messaging, media, calendar, and travel planning through persistent, domain-specialized agents that operate autonomously on the user's behalf.

This is not a chatbot. It is an operational system — it watches queues, processes incoming content, executes scheduled tasks, and acts on the user's behalf within defined boundaries. The user interacts with it conversationally when needed, but the system's primary value is the work it does without being asked.

## 2. Who This Is For

**Primary user:** A technically proficient individual (sole administrator) who manages their own infrastructure and is comfortable with CLI tooling, container orchestration, and self-hosted services. This user wants to delegate repetitive coordination work — triaging email, managing calendars, tracking packages, curating media, planning trips — to an automated system they fully control.

**Why self-hosted matters to this user:**

- Full ownership of data and conversation history — nothing leaves the home network unless explicitly configured to do so
- Ability to integrate with local network services (home automation, network monitoring, media servers) that cloud-hosted assistants cannot reach
- No subscription dependency for core functionality
- Freedom to customize agent behavior, tool access, and security policy without platform constraints

**Phase 2 audience expansion:** Household members who interact with the system through natural conversation channels (iMessage, voice) without needing technical knowledge. Multi-user support is a stated goal but not a Phase 1 requirement.

## 3. What Problem This Solves

The user's daily workflow involves a set of recurring coordination tasks that are individually small but collectively time-consuming:

- Checking and triaging email across multiple accounts
- Tracking package deliveries and shipping notifications
- Managing calendar events, invitations, and scheduling conflicts
- Monitoring home network equipment and security cameras
- Discovering and organizing media (music, film, TV)
- Planning events, outings, and trips based on personal preferences
- Navigating reservation systems, appointment schedulers, and IVR phone trees

Each of these is well-defined enough to automate but too context-dependent for simple rules or scripts. They require judgment — understanding what's important, what can wait, what needs human input, and what can be handled silently. That's the gap this system fills.

## 4. Core Capabilities

Each capability exists because it addresses a specific need from the problem statement above. They are grouped by the domain agent responsible for delivering them.

### 4.1 Messaging, Mail & Packages

**Why it exists:** Email and messaging are the primary inbound channels for information that requires action. The volume-to-signal ratio is poor — most messages need nothing, some need awareness, few need response. Manual triage is the single largest recurring time cost.

**What it does:**

- Monitors email accounts and surfaces messages that need attention, with priority assessment and context summary
- Tracks package shipments from delivery notification emails, consolidating status across carriers into a single view
- Drafts responses to routine messages for user review and approval
- Filters and categorizes incoming messages, separating actionable items from noise
- Sends notifications for time-sensitive items (delivery arriving today, response needed by deadline)

### 4.2 Media

**Why it exists:** The user maintains a local media server and has active interests in music and film. Discovering new content, managing libraries, and staying current with releases are ongoing tasks that benefit from an agent that knows the user's preferences.

**What it does:**

- Monitors media libraries (Plex/Jellyfin) and surfaces organizational issues or new additions
- Recommends new music, film, and TV based on established preferences and consumption history
- Tracks upcoming releases, premieres, and screenings relevant to the user's interests
- Manages download queues and library organization tasks

### 4.3 Calendar & Events

**Why it exists:** Calendar management involves more than recording events — it requires understanding scheduling conflicts, travel time, preparation needs, and the relationship between events. Invitation responses and scheduling coordination are particularly well-suited to agent handling.

**What it does:**

- Manages calendar entries across accounts, detecting and flagging conflicts
- Processes incoming event invitations, presenting them with relevant context (travel time, existing commitments, similar past events)
- Generates event suggestions based on user preferences, local happenings, and seasonal opportunities
- Monitors home network and security systems (UniFi Network/Protect) and surfaces notable events
- Coordinates scheduling tasks that require navigating external systems (appointment booking, reservation management)

### 4.4 Itineraries & Travel

**Why it exists:** Trip planning is research-intensive, involves many small decisions, and benefits enormously from personalization. An agent with knowledge of the user's preferences, budget patterns, and travel history can produce meaningfully better starting points than generic search.

**What it does:**

- Generates trip proposals and itineraries based on stated preferences, budget, and timing
- Researches destinations, accommodations, and activities with the user's specific interests in mind
- Navigates booking and reservation systems on the user's behalf (with approval gates for financial commitments)
- Tracks active trip logistics — flight status, hotel confirmations, reservation times
- Suggests day trips, local outings, and weekend plans based on weather, events, and mood

### 4.5 Quarantine Review

**Why it exists:** Because the system processes external content (email, webhooks, notifications) that could contain prompt injection attacks — content designed to hijack agent behavior. A human-in-the-loop review process is essential for any content the automated scanning layer flags as suspicious.

**What it does:**

- Presents flagged items to the user with metadata only (source, sender, signals triggered, content length) — never the raw content itself
- Accepts binary release/reject decisions from the user
- Routes approved content to the appropriate agent workspace
- Logs rejected content metadata for pattern analysis, then deletes the content itself

## 5. Content Safety

All external content entering the system passes through a scanning layer before reaching any agent. This is not spam filtering or malware detection — it is specifically designed to catch content that attempts to manipulate LLM behavior (prompt injection).

**Why this matters:** The agents in this system have real capabilities — they can send emails, modify calendars, interact with services. An attacker who successfully injects instructions into an agent's context could cause the agent to take unauthorized actions. The scanning layer exists to prevent untrusted external content from reaching agents without inspection.

**What the user should expect:**

- Routine content (normal emails, standard notifications, calendar invites) passes through transparently with no delay
- Content matching known injection patterns is held for review — the user is notified and makes the call
- The system errs on the side of caution: false positives go to review, false negatives are the real risk
- In Phase 1, scanning is heuristic-only (pattern matching). Phase 2 adds an LLM-based classifier for more sophisticated detection

## 6. Phased Delivery

The system is delivered in two phases, reflecting hardware availability and capability maturity.

### Phase 1 — Prototype

**Hardware:** Intel NUC, Talos Linux, Kubernetes

**What it delivers:** The four domain agents and the review agent running as containerized workloads. Heuristic-only content scanning. Filesystem-based inter-agent coordination. Cloud LLM inference (Anthropic API). Local embedding server for memory retrieval. Full secrets management, monitoring, backup, and GitOps pipeline.

**What it proves:** That the multi-agent topology, content scanning model, and operational patterns work before committing to production hardware.

### Phase 2 — Production

**Hardware:** Mac Studio M5, native macOS

**What it adds:** Apple ecosystem integrations (iMessage, Shortcuts, AppleScript, desktop automation, voice). Local LLM inference for both agent tasks and content classification. Knowledge graph memory layer. Multi-user support. Browser automation via Playwright for navigating external systems (IVR, appointment schedulers, reservation platforms).

**Why the hardware change:** Phase 2 capabilities require native macOS access (TCC permissions, Accessibility APIs, Automation frameworks) that cannot run inside containers. The Mac Studio's unified memory architecture supports local model inference alongside agent workloads.

## 7. What This Is Not

- **Not a general-purpose assistant.** It handles specific, well-defined domains. Ad-hoc questions go to Claude or other conversational AI — this system is for operational tasks.
- **Not a home automation controller.** It can monitor and surface events from home systems (UniFi, smart home devices) but is not a replacement for Home Assistant or similar platforms. Integration is read-heavy, write-light, and always gated by user approval for actions.
- **Not autonomous without boundaries.** Every agent operates within an explicit permission model. Actions with real-world consequences (sending email, booking reservations, modifying calendar) require user approval. The system is designed to propose and draft, not to act unilaterally.
- **Not multi-tenant.** Even in Phase 2, this is a single-household system. Multi-user means multiple people in the same home interacting with the same agent topology, not isolated environments for unrelated users.
