# Glovebox Competitive & Ecosystem Landscape

**Date: 2026-08-20.** Research question: has glovebox (deterministic, no-LLM
content-scanning firewall + connector framework for OpenClaw Home Agent
deployments) been superseded elsewhere, and what should it learn from the field?

---

## 0. Status of glovebox itself

[github.com/leftathome/glovebox](https://github.com/leftathome/glovebox) is
**public**: Apache-2.0, Go, ~500 commits, **1 star, 0 forks**. Web searches for
"glovebox" + OpenClaw/content-scanning surface no third-party discussion, blog
coverage, forks, or ClawHub listing — the project is currently invisible to the
ecosystem despite the OpenClaw security topic being extremely hot (CrowdStrike,
Bitsight, arXiv papers, and multiple 2026 hardening guides all cover OpenClaw
prompt-injection risk; none mention glovebox).

Notably, OpenClaw core itself has **no native inbound-content injection
scanning**: a feature request for prompt-injection scanning config
([openclaw/openclaw#7705](https://github.com/openclaw/openclaw/issues/7705))
was **closed as not planned**, and OpenClaw's own docs recommend prompt-level
mitigations (SOUL.md instructions) that hardening guides admit are weak
([Contabo guide](https://contabo.com/blog/openclaw-security-guide-2026/),
[Valletta guide](https://vallettasoftware.com/blog/post/openclaw-security-2026-best-practices-risks-hardening-guide)).
The niche glovebox targets is real and acknowledged as unfilled in OpenClaw core.

---

## 1. The competitive field

### 1.1 Meta LlamaFirewall / PromptGuard 2 — the reference OSS guardrail framework
- **URL:** [github.com/meta-llama/PurpleLlama](https://github.com/meta-llama/PurpleLlama) (LlamaFirewall subproject), [paper](https://arxiv.org/abs/2505.03574)
- **What:** Modular guardrail framework (May 2025): **PromptGuard 2** (86M and
  22M-param ML classifiers for jailbreak/injection, 97.5% detection on Meta's
  internal set), **AlignmentCheck** (LLM chain-of-thought auditor),
  **CodeShield** (static analysis), plus customizable regex filters
  ([Help Net Security](https://www.helpnetsecurity.com/2025/05/26/llamafirewall-open-source-framework-detect-mitigate-ai-centric-security-risks/),
  [docs](https://meta-llama.github.io/PurpleLlama/LlamaFirewall/docs/documentation/about-llamafirewall)).
- **Maturity:** PurpleLlama ~4.1k stars, active into 2026.
- **Approach:** ML classifier + LLM-judge + rules. Python library embedded in
  the agent loop — **not** a connector-side pipeline service.
- **vs glovebox:** Could replace glovebox's *detection engine* (better recall
  than regex), not its connector/staging/quarantine pipeline.
  PromptGuard-2-22M runs locally with no API dependency — the strongest
  candidate for an optional glovebox detector plugin.

### 1.2 ProtectAI llm-guard — **archived** (cautionary tale)
- **URL:** [github.com/protectai/llm-guard](https://github.com/protectai/llm-guard), ~3.2k stars
- **Status:** Palo Alto acquired Protect AI July 2025; last release v0.3.16
  May 2025; **repo archived July 9, 2026**, models unmaintained
  ([issue #324](https://github.com/protectai/llm-guard/issues/324),
  [alternatives roundup](https://senthex.com/en/llm-guard-alternatives/)).
- **Approach:** Python scanner toolkit (ML + rules: injection classifier, PII, secrets, toxicity).
- **vs glovebox:** Was the closest "self-hosted scanner toolkit" analog; its
  death leaves a **gap for a maintained, self-hostable, no-cloud scanner** — a
  gap glovebox partially fills.

### 1.3 Rebuff — **archived**
- **URL:** [github.com/protectai/rebuff](https://github.com/protectai/rebuff), ~1.5k stars, archived May 2025.
- **Approach:** 4-layer: heuristics + LLM check + vector DB of past attacks +
  **canary tokens** ([LangChain blog](https://www.langchain.com/blog/rebuff)).
  Required Pinecone + Supabase + OpenAI — heavy external dependencies
  criticized in the ecosystem ([zeroclaw#2590](https://github.com/zeroclaw-labs/zeroclaw/issues/2590)).
- **vs glovebox:** Dead, but its canary-token idea (detect leakage of a planted
  secret in agent *output*) is worth adopting.

### 1.4 Lakera Guard (Check Point) — commercial API firewall
- **URL:** [lakera.ai](https://www.lakera.ai); acquired by **Check Point Sept 2025**
  ([Maxim roundup](https://www.getmaxim.ai/articles/top-5-llm-security-tools-for-enterprise-ai-applications-in-2026/)).
- **Approach:** SaaS API inspecting prompts/responses for injection,
  jailbreaks, PII (ML classifiers). Cloud-dependent, closed, prompt-level.
- **vs glovebox:** Different market (enterprise SaaS). Does not serve
  self-hosted/home, no connector integration, not deterministic. Not a
  replacement for glovebox's niche.

### 1.5 NVIDIA NeMo Guardrails — active OSS rails framework
- **URL:** [github.com/NVIDIA-NeMo/Guardrails](https://github.com/NVIDIA-NeMo/Guardrails)
- **What:** Programmable input/output/dialog rails (Colang DSL), jailbreak &
  injection detection, PII, OpenTelemetry tracing; v0.23 (2026) added
  lightweight HF classifier rails and parallel rail execution
  ([releases](https://github.com/NVIDIA-NeMo/Guardrails/releases),
  [appsecsanta review](https://appsecsanta.com/nemo-guardrails),
  [NVIDIA injection-detection tutorial](https://docs.nvidia.com/nemo/microservices/latest/guardrails/tutorials/injection-detection.html)).
- **Approach:** Rules + ML + LLM self-checking, wrapped around a conversational app.
- **vs glovebox:** Serves developers building LLM apps, not a pipeline scanning
  connector feeds pre-context. Heavy Python stack; much of it needs an LLM.
  Could replace glovebox only by rebuilding the pipeline around it.

### 1.6 Invariant Labs (mcp-scan / Gateway / Guardrails) → Snyk agent-scan
- **URLs:** [invariantlabs.ai](https://invariantlabs.ai/blog/introducing-mcp-scan),
  [pypi mcp-scan](https://pypi.org/project/mcp-scan/),
  [github.com/snyk/agent-scan](https://github.com/snyk/agent-scan),
  [Snyk acquisition](https://labs.snyk.io/resources/snyk-labs-invariant-labs/)
- **What:** mcp-scan statically scans MCP server configs for tool
  poisoning/rug pulls, plus a **proxy mode** applying rule-based guardrail
  policies to live MCP traffic; Invariant Guardrails is a deterministic-ish
  policy-rule layer between app and MCP/LLM. Now folded into Snyk's agent-scan
  (scans MCP servers, tools, prompts, skills; 15+ risk types).
- **Approach:** Rules/policy engine + static scanning; gateway architecture.
- **vs glovebox:** Closest *architectural* cousin (interception + policy,
  largely deterministic), but scoped to **MCP traffic and agent-tool supply
  chain**, not email/RSS/webhook connector ingestion; no human-quarantine
  workflow, no byte-identical staging handoff.

### 1.7 Vault (vaultmcp) — MCP prompt-injection scanning proxy
- **URL:** [github.com/vaultmcp/vault](https://github.com/vaultmcp/vault) — 101 stars, 18 forks, TypeScript, MIT, active.
- **What:** Runtime proxy scanning **every MCP tool response** before it
  reaches the agent. Layered: L0 deterministic decoder (encodings), L1 regex
  heuristics (<1ms; instruction overrides, **unicode tag smuggling**, control
  chars), L2 on-device embedding similarity vs 31 attack categories (~8ms,
  bge-small), L3 optional LLM judge (Claude Haiku). Works offline at L1+L2.
  Claims 100% detection / 0% FP on its 80-attack public holdout.
- **vs glovebox:** The most similar new (2026) project in spirit — "scan
  untrusted content before the context window," self-hostable,
  deterministic-first with optional escalation. But MCP-transport-only: no
  connectors, no staging/quarantine/human review, no byte-identical delivery
  guarantee. **Partially overlapping; strong design to learn from (layered
  escalation, encoding normalization).**

### 1.8 Pipelock — open-source Go agent firewall (May 2026)
- **URL:** [github.com/luckyPipewrench/pipelock](https://github.com/luckyPipewrench/pipelock)
  — ~800 stars, Apache-2.0, single Go binary, active Aug 2026
  ([Help Net Security](https://www.helpnetsecurity.com/2026/05/04/pipelock-open-source-ai-agent-firewall/),
  [pipelab.org](https://pipelab.org/pipelock/)).
- **What:** Sidecar between agent and network; 11-layer pipeline scanning
  HTTP/WebSocket/MCP/A2A traffic for **egress exfiltration** (48 DLP
  patterns), **inbound prompt injection** (25 patterns, 6-pass normalization),
  SSRF, tool-chain risks; **Ed25519-signed action receipts** as verifiable
  audit evidence from outside the agent trust boundary. Supports Claude Code,
  Cursor, LangGraph, etc.
- **Approach:** Deterministic patterns + normalization; network-interposition
  rather than file-staging.
- **vs glovebox:** The closest **philosophical** competitor: deterministic, Go,
  self-hosted, pattern-based, audit-focused. Complementary axis, though —
  pipelock guards the agent's *live network traffic* (esp. egress); glovebox
  guards *batch connector ingestion* with human quarantine. Could partially
  replace glovebox for users who route ingestion through the agent's own
  network calls. Its signed receipts and multi-pass normalization outclass
  glovebox's audit log and raw regex.

### 1.9 NVIDIA SkillSpector — agent-skill supply-chain scanner (Aug 2026)
- **URL:** [github.com/nvidia/skillspector](https://github.com/nvidia/skillspector)
  — ~14.8k stars, Apache-2.0, very active
  ([Help Net Security](https://www.helpnetsecurity.com/2026/08/03/skillspector-open-source-agent-skill-security-scanner/)).
- **What:** Static scanner for agent *skills* before install (68 vulnerability
  patterns / 17 categories: hidden instructions, unicode lookalikes,
  credential theft, poisoned deps), optional LLM semantic pass; anchors
  NVIDIA's Verified Skills program.
- **vs glovebox:** Different layer (supply chain of skills, not runtime content
  feeds). Not a replacement; validates the "deterministic static checks first,
  optional LLM second" pattern.

### 1.10 OpenClaw-native ecosystem: ClawGuard family, email-defense skill, VirusTotal
- **email-prompt-injection-defense skill**
  ([ClawHub via playbooks](https://playbooks.com/skills/openclaw/skills/email-prompt-injection-defense)):
  official-ecosystem skill that scans incoming email bodies for injection
  patterns (fake system outputs, embedded thinking blocks, encoded payloads,
  hidden text), marks severity, quarantines, and requires user confirmation.
  **Closest OpenClaw-native overlap with glovebox's email path** — but it's a
  *skill*, i.e., prompt-level guidance executed by the very LLM being
  defended: non-deterministic, bypassable, advisory only. Glovebox's
  out-of-band enforcement is strictly stronger.
- **ClawGuard variants** (at least 7 distinct projects —
  [Gk0Wk/ClawGuard](https://github.com/Gk0Wk/ClawGuard) "antivirus for
  OpenClaw" (action approval, skill scan, secret-leak blocking),
  [lombax85/clawguard](https://github.com/lombax85/clawguard) (CIBA auth
  gateway w/ Telegram approval),
  [newtro/ClawGuard](https://github.com/newtro/ClawGuard) (skill permission
  manifests/sandboxing),
  [Claw-Guard/ClawGuard](https://github.com/Claw-Guard/ClawGuard)
  (deterministic tool-call rule enforcement), plus commercial
  [clawguard.io](https://clawguard.io/)): all focus on **tool-call/action
  gating and skill scanning** — the *output/action* side — not deterministic
  scanning of inbound connector data.
- **OpenClaw + VirusTotal**
  ([The Hacker News, Feb 2026](https://thehackernews.com/2026/02/openclaw-integrates-virustotal-scanning.html)):
  ClawHub skill marketplace scanning — supply chain again, not content feeds.
- **Verdict for the OpenClaw niche:** crowded on skills/actions; **near-empty
  on deterministic inbound-connector content scanning**.

### Notable adjacents (briefer)
- **Cloudflare AI Security for Apps** (ex-"Firewall for AI"): WAF-level,
  model-agnostic injection scoring + PII on prompts in HTTP traffic;
  commercial/edge, wrong deployment model for self-hosted home agents
  ([docs](https://developers.cloudflare.com/waf/detections/ai-security-for-apps/prompt-injection/),
  [blog](https://blog.cloudflare.com/firewall-for-ai/)).
- **Microsoft Defender for Office 365** now detects/quarantines
  prompt-injection emails before AI agents process them
  ([Microsoft Community Hub](https://techcommunity.microsoft.com/blog/MicrosoftDefenderforOffice365Blog/defending-the-inbox-against-prompt-injection-attacks/4534636))
  — enterprise-cloud version of glovebox's email path; confirms the threat
  model is mainstream but leaves self-hosted unserved.
- **Promptfoo** ([promptfoo.dev](https://www.promptfoo.dev/docs/red-team/)):
  red-teaming/eval, 50+ attack plugins; acquired by OpenAI Mar 2026, stays OSS
  ([qaskills guide](https://qaskills.sh/blog/promptfoo-llm-red-teaming-guide)).
  **Complement, not competitor** — glovebox should use it to test its rules.
- **Tencent AI-Infra-Guard**
  ([github topic listing](https://github.com/topics/prompt-injection?o=desc&s=stars)):
  4.9k-star red-team platform (agent/skill/MCP scan) — assessment, not runtime
  pipeline defense.
- **ZeroClaw** (Rust OpenClaw alternative,
  [elev8tion/zeroclaw](https://github.com/elev8tion/zeroclaw)): builds
  **PromptGuard (360-LOC injection detection), LeakDetector, CanaryGuard
  directly into the agent runtime**
  ([issue #2590](https://github.com/zeroclaw-labs/zeroclaw/issues/2590),
  [issue #1979](https://github.com/zeroclaw-labs/zeroclaw/issues/1979)) —
  evidence agent runtimes are internalizing deterministic security layers, a
  long-term substitution threat.

---

## 2. Connector-framework side

- **Mature generic ingestion exists everywhere:** Airbyte (550+ connectors),
  n8n, Huginn, Node-RED, RSS-Bridge, changedetection.io
  ([Airbyte](https://airbyte.com/connectors/n8n),
  [alternatives roundup](https://www.lindy.ai/blog/n8n-alternatives)).
  Glovebox's ~24 connectors cannot compete on breadth, and its framework
  (staging, checkpoints, health, metrics) re-implements what these platforms
  have at massive scale.
- **But none combine ingestion with adversarial-content scanning.** Searches
  for security/injection-scanning layers in Airbyte/n8n/Huginn surface only
  compliance features (SOC 2, encryption, RBAC) — no prompt-injection scanning
  of payloads destined for LLM agents. **No project found anywhere combines a
  connector framework + deterministic injection scanning + human quarantine +
  byte-identical delivery.** That combination is glovebox's genuinely novel
  surface.
- **Strategic implication:** the moat is the *combination and the handoff
  protocol*, not the connectors themselves. A "scan-only" mode that fronts
  Airbyte/n8n/OpenClaw-native connectors (accepting their output into staging)
  would widen the funnel without competing on connector count.

---

## 3. Verdict

**Glovebox's niche is partially served but not superseded.** As of Aug 2026:

- **Unserved (glovebox is still novel):** deterministic + fully offline +
  connector-integrated ingestion scanning + human quarantine queue +
  byte-identical delivery, packaged for self-hosted OpenClaw home deployments.
  OpenClaw core declined to build it
  ([#7705 closed](https://github.com/openclaw/openclaw/issues/7705)); the
  ClawGuard ecosystem guards actions/skills, not inbound feeds; enterprise
  equivalents (Defender for O365, Cloudflare, Lakera/Check Point) are
  cloud-only. The archiving of llm-guard and Rebuff actually *widened* this gap.
- **Partially served:** Vault (MCP responses) and pipelock (network traffic)
  now scan untrusted content deterministically before it hits the agent, with
  active communities and, in pipelock's case, the same language and license.
  Users who ingest via MCP tools or the agent's own HTTP calls rather than
  out-of-band connectors can get most of glovebox's value there.
- **Headwind to acknowledge honestly:** the 2026 consensus (CaMeL, Meta's
  Agents Rule of Two, Nylas, OWASP Agentic Top 10) is that **pattern detection
  is bypassable and should be telemetry + quarantine-triage, while the real
  security boundary must be capability/policy control outside the model**
  ([Simon Willison on CaMeL](https://simonwillison.net/2025/Apr/11/camel/),
  [Meta Rule of Two](https://ai.meta.com/blog/practical-ai-agent-security/),
  [Nylas](https://cli.nylas.com/guides/email-prompt-injection-defense)).
  Glovebox's quarantine-for-human-review framing is exactly the right use of
  deterministic detection — but it should not market itself as a complete
  defense.

## 4. Ideas to adopt

1. **Multi-pass normalization/decoding before rules** (pipelock's 6-pass
   normalization; Vault's L0 decoder + unicode-tag-smuggling detection) —
   plain regex over raw bytes misses base64/homoglyph/invisible-unicode
   payloads; normalization for *scanning* is compatible with byte-identical
   *delivery*.
2. **Optional local ML detector plugin**: PromptGuard-2-22M (ONNX, CPU-viable,
   no API) as a weighted detector alongside regex — preserves "no cloud LLM
   dependency" while fixing regex's recall ceiling.
3. **Canary tokens** (Rebuff/ZeroClaw CanaryGuard) for downstream exfiltration
   detection.
4. **Spotlighting/content-marking metadata**: deliver a sidecar
   provenance/trust-label file with each item (keeping the payload
   byte-identical) so agents can apply Rule-of-Two-style policies.
5. **Signed audit receipts** (pipelock's Ed25519 receipts) to upgrade the
   JSONL audit log into verifiable evidence.
6. **Publish benchmark results** (AgentDojo,
   [LivePI](https://arxiv.org/pdf/2605.17986), Vault's public holdout) — every
   credible 2026 entrant ships detection/FP numbers.
7. **Map rules to OWASP LLM Top 10 / Agentic Top 10** and position explicitly
   as the detection+quarantine layer of a defense-in-depth stack next to
   action-gating tools (ClawGuard), rather than a standalone shield.
8. **An MCP-proxy or scan-only ingestion mode** to meet users where ingestion
   actually happens in 2026.

## Sources

- Glovebox repo: https://github.com/leftathome/glovebox
- OpenClaw scanning feature request (closed): https://github.com/openclaw/openclaw/issues/7705
- OpenClaw security guides: https://contabo.com/blog/openclaw-security-guide-2026/ · https://vallettasoftware.com/blog/post/openclaw-security-2026-best-practices-risks-hardening-guide · https://www.crowdstrike.com/en-us/blog/what-security-teams-need-to-know-about-openclaw-ai-super-agent/
- LlamaFirewall: https://arxiv.org/abs/2505.03574 · https://meta-llama.github.io/PurpleLlama/LlamaFirewall/docs/documentation/about-llamafirewall · https://www.helpnetsecurity.com/2025/05/26/llamafirewall-open-source-framework-detect-mitigate-ai-centric-security-risks/ · https://github.com/meta-llama/PurpleLlama
- llm-guard archived: https://github.com/protectai/llm-guard · https://github.com/protectai/llm-guard/issues/324 · https://senthex.com/en/llm-guard-alternatives/
- Rebuff: https://github.com/protectai/rebuff · https://www.langchain.com/blog/rebuff
- Lakera/Check Point: https://www.getmaxim.ai/articles/top-5-llm-security-tools-for-enterprise-ai-applications-in-2026/
- NeMo Guardrails: https://github.com/NVIDIA-NeMo/Guardrails/releases · https://appsecsanta.com/nemo-guardrails · https://docs.nvidia.com/nemo/microservices/latest/guardrails/tutorials/injection-detection.html
- Invariant/Snyk: https://invariantlabs.ai/blog/introducing-mcp-scan · https://labs.snyk.io/resources/snyk-labs-invariant-labs/ · https://github.com/snyk/agent-scan · https://pypi.org/project/mcp-scan/
- Vault: https://github.com/vaultmcp/vault
- Pipelock: https://github.com/luckyPipewrench/pipelock · https://www.helpnetsecurity.com/2026/05/04/pipelock-open-source-ai-agent-firewall/ · https://pipelab.org/pipelock/
- SkillSpector: https://github.com/nvidia/skillspector · https://www.helpnetsecurity.com/2026/08/03/skillspector-open-source-agent-skill-security-scanner/
- OpenClaw ecosystem: https://playbooks.com/skills/openclaw/skills/email-prompt-injection-defense · https://github.com/Gk0Wk/ClawGuard · https://github.com/lombax85/clawguard · https://github.com/newtro/ClawGuard · https://github.com/Claw-Guard/ClawGuard · https://clawguard.io/ · https://thehackernews.com/2026/02/openclaw-integrates-virustotal-scanning.html
- ZeroClaw: https://github.com/elev8tion/zeroclaw · https://github.com/zeroclaw-labs/zeroclaw/issues/2590 · https://github.com/zeroclaw-labs/zeroclaw/issues/1979
- Cloudflare: https://developers.cloudflare.com/waf/detections/ai-security-for-apps/prompt-injection/ · https://blog.cloudflare.com/firewall-for-ai/
- Microsoft Defender for O365: https://techcommunity.microsoft.com/blog/MicrosoftDefenderforOffice365Blog/defending-the-inbox-against-prompt-injection-attacks/4534636
- Promptfoo: https://www.promptfoo.dev/docs/red-team/ · https://qaskills.sh/blog/promptfoo-llm-red-teaming-guide
- CaMeL / architectural defenses: https://simonwillison.net/2025/Apr/11/camel/ · https://www.infoq.com/news/2025/04/deepmind-camel-promt-injection · https://ai.meta.com/blog/practical-ai-agent-security/ · https://cli.nylas.com/guides/email-prompt-injection-defense
- Connectors: https://airbyte.com/connectors/n8n · https://www.lindy.ai/blog/n8n-alternatives
- GitHub topic sweep: https://github.com/topics/prompt-injection
- Benchmarks: https://arxiv.org/pdf/2605.17986 (LivePI)
