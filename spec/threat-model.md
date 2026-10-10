# Threat Model (spec)

This document defines the normative threat model for the AI-SDLC system, ranking threat classes, detailing attack vectors, and mapping defenses to runtime controls. It is maintained as a living document with citations and open questions.

## Ranked Threat Classes

Threats are ranked by potential impact and likelihood in the AI-SDLC context:

1. **External Injection** (Highest priority)
   - Malicious inputs from outside the trust boundary (T1–T3 content)
   - Goal: Exfiltrate secrets, steer agent behavior, corrupt outputs

2. **Insider Threat**
   - Authorized actors misusing privileges or being compromised
   - Goal: Data theft, sabotage, policy violation

3. **Denial of Service (DoS)**
   - Exhausting system resources to disrupt availability
   - Goal: Prevent legitimate operations, increase costs

4. **Drift**
   - Gradual degradation of model or system alignment from intended behavior
   - Goal: Silent failure, loss of trust, unsafe outputs

5. **Supply Chain** (Lowest priority in direct agent context but critical for integrity)
   - Compromise of dependencies, tools, or data sources
   - Goal: Persistent backdoor, integrity violation, widespread impact

## Attack Classes and Defenses

### External Injection

**Attack Classes:**
- **Steganographic Unicode**: Hiding malicious instructions in Unicode characters (e.g., zero-width joysticks, homoglyphs) that bypass visual inspection but are processed by the agent.
- **Indirect Disclosure**: Injecting queries that cause the agent to leak sensitive information through side channels (e.g., timing, error messages, output formatting).
- **Memory Poisoning**: Corrupting the agent's context or state via injected content to influence future reasoning (e.g., planting false facts, altering goal state).
- **Test Poisoning**: Injecting malicious content into test suites or evaluation data to compromise assessment results.
- **Coordinated Inauthentic Contributions**: Multiple seemingly independent inputs working in concert to overwhelm defenses or create consensus for malicious output.

**Defenses (Runtime Controls):**
- **Fencing and Scrubbing** (spec/injection-defense.md): All untrusted content (T1–T3) is fenced with explicit DATA markers and scrubbed of imperative overrides.
- **Secret Scanning**: Separate pass to redact credential-shaped content from logs and re-prompts.
- **Capability Splitting**: Denylisting dangerous actions (e.g., `git push --force`, piping to shell) independent of content trust.
- **Trust Boundary Enforcement**: Strict separation of trusted (T0) and untrusted content; never mixing untrusted text as instructions.
- **Telemetry and Refusal Logging**: Logging blocked injection attempts as `prompt_injection_blocked` for observability.

**Spec Reference:** `spec/injection-defense.md` governs defenses against external injection.

### Insider Threat

**Attack Classes:**
- **Social-Pressure Disproportionate Response**: Insider manipulated via social engineering to override safety policies (e.g., fake urgency, authority impersonation).
- **Memory Poisoning** (Insider variant): Privileged actor altering agent memory or training data to introduce biases or backdoors.
- **Test Poisoning** (Insider variant): Insider compromising validation suites to hide malicious behavior.
- **Coordinated Inauthentic Contributions** (Insider variant): Collusion among privileged actors to submit seemingly benign changes that collectively enable attack.

**Defenses (Runtime Controls):**
- **Trust Ladder Promotion** (spec/trust-ladder.md): Evidence-based promotion requiring multiple independent verifications for privilege escalation.
- **Adversarial Review**: Mandatory review of changes by independent parties with conflicting incentives.
- **Audit Trail Integrity**: Cryptographic signing of all agent actions and logs to detect tampering.
- **Least Privilege Enforcement**: Runtime restrictions on what actions agents can perform based on current trust level.
- **Behavioral Anomaly Detection**: Monitoring for deviations from established patterns of privileged activity.

**Spec Reference:** `spec/trust-ladder.md` governs defenses against insider threat through evidence-based trust elevation.

### Denial of Service (DoS)

**Attack Classes:**
- **Resource Exhaustion**: Flooding agent with complex requests to consume CPU, memory, or API quotas.
- **Trigger Loops**: Crafting inputs that cause the agent to enter expensive infinite or recursive computations.
- **Amplification Attacks**: Using agent to generate large outputs from small inputs (e.g., asking for exhaustive lists).
- **Dependency Saturation**: Overloading external services (e.g., web fetch, code repositories) via agent requests.

**Defenses (Runtime Controls):**
- **Bounded Execution**: All agent steps bounded by `timeout-minutes`, `max_steps`, `max_repairs`.
- **Rate Limiting**: Per-agent and per-tool request limits to prevent flooding.
- **Cost Awareness**: Tracking token usage and aborting when thresholds exceeded.
- **Input Complexity Scoring**: Rejecting or deferring inputs exceeding computational complexity thresholds.
- **Circuit Breaker Patterns**: Automatic disengagement from failing external services.

**Spec Reference:** `spec/ai-sdlc.yaml` (runtime configuration) contains bounds; `spec/definition-of-ready.md` includes complexity gates.

### Drift

**Attack Classes:**
- **Objective Drift**: Gradual shift in agent's optimization goal due to feedback loops or reward hacking.
- **Data Drift**: Changes in input data distribution causing degraded performance without explicit malicious intent.
- **Model Drift**: Degradation of fine-tuned adaptations over time due to lack of retraining.
- **Policy Drift**: Slow erosion of safety constraints through cumulative small violations.

**Defenses (Runtime Controls):**
- **Continuous Evaluation**: Regular benchmarking against held-out test sets to detect performance degradation.
- **Feedback Loop Sanitization**: Ensuring reward signals are not manipulable by agent outputs.
- **Distribution Monitoring**: Statistical tests for drift in input and intermediate representations.
- **Scheduled Realignment**: Periodic retraining or fine-tuning on verified data.
- **Constraint Anchoring**: Hard-coded invariants that cannot be overridden by learning or feedback.

**Spec Reference:** `spec/risk-taxonomy.md` includes drift as a risk category; `spec/postmortem-lifecycle.md` outlines drift detection and remediation.

### Supply Chain

**Attack Classes:**
- **Dependency Confusion**: Malicious packages with names mimicking internal dependencies.
- **Compromised Tools**: Poisoned versions of linters, formatters, or scanners used in the pipeline.
- **Data Source Tampering**: Corruption of training data, evaluation sets, or knowledge bases.
- **Credential Theft via Supply Chain**: Stealing agent credentials through compromised build or deploy tools.

**Defenses (Runtime Controls):**
- **Locked Dependency Manifests**: Using `package-lock.json` or equivalent to prevent dependency confusion.
- **Tool Integrity Verification**: Cryptographic hashes and signatures for all third-party tools.
- **Data Provenance Tracking**: Recording origin and transformations of all data used by the agent.
- **Network Egress Restrictions**: Limiting outbound connections to known, approved endpoints.
- **Build Environment Isolation**: Running agent in sandboxed environments with minimal privileges.

**Spec Reference:** `spec/generated-artifact-publish-contract.md` governs secure publishing; `spec/org-policy.md` includes supply chain controls.

## Open Questions

1. **Measurement**: How do we quantitatively measure the effectiveness of each defense against adaptive attackers?
2. **Trade-offs**: What is the optimal balance between security boundaries and agent utility? Where do we draw the line on capability restrictions?
3. **Emergent Threats**: As agent capabilities grow, new attack classes may emerge (e.g., manipulation of multi-modal inputs, exploitation of tool chaining). How do we proactively identify and mitigate these?
4. **Cross-Class Interactions**: How do defenses in one class (e.g., input fencing) affect the risk profile of others (e.g., could increased friction lead to more insider risk)?
5. **Human-in-the-Loop Efficacy**: What is the reliability of adversarial review and trust ladder promotion against determined insiders with social engineering capabilities?
6. **Supply Chain Visibility**: How can we achieve end-to-end visibility of dependencies in heterogeneous agent toolchains without prohibitive overhead?
7. **Drift Detection Latency**: What is the acceptable time window for detecting drift, and how do we minimize false positives in production systems?
8. **Legal and Regulatory Alignment**: How do these threat classes map to emerging AI regulations (e.g., EU AI Act, NIST AI RMF), and what evidence is required for compliance?

## References

1. OWASP LLM Top 10 (2023): https://owasp.org/www-project-top-10-for-large-language-model-applications/
2. NIST AI Risk Management Framework (AI RMF 1.0): https://nvlpubs.nist.gov/nistpubs/ai/NIST.AI.100-1.pdf
3. Google: "Safety Challenges for AI in Large Language Models" (2022): https://ai.googleblog.com/2022/09/safety-challenges-for-ai-in-large.html
4. Microsoft: "Threat Modeling for AI/ML Systems" (2021): https://learn.microsoft.com/en-us/azure/security/develop/threat-modeling-ai
5. IBM: "Adversarial Threat Landscape for Artificial-Intelligence Systems" (ATLAS): https://atlas.mitre.org/
6. Axel-DaMage/hermes#19: Prompt guard implementation for issue-body sanitization.
7. os-santiago/sc-agent-cli: Engine implementation of trust ladder and bounded execution.
8. Axel-DaMage/hermes: Control plane for OmniRoute and trust machinery.
9. NIST SP 800-161: Supply Chain Risk Management Practices for Federal Information Systems and Organizations.
10. MITRE ATLAS™ v2: Adversarial Threat Landscape for Artificial-Intelligence Systems.