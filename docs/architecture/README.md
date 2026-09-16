# Architecture diagrams

Every diagram in this directory is a `.mmd` file — the **canonical source**.
`ARCHITECTURE.md` embeds each one verbatim under a `> Source:` link right
after its section heading, so a diagram is only ever correct in one place.
If you edit a flow, edit the `.mmd` file here first, then copy the fenced
block into `ARCHITECTURE.md` — keep both in sync by hand (there is no
generator).

| File | Diagram | Related section |
|---|---|---|
| [layer-model.mmd](mermaid/layer-model.mmd) | `graph TD` | ARCHITECTURE.md § Layer model; LLD §3 |
| [package-dependencies.mmd](mermaid/package-dependencies.mmd) | `graph LR` | ARCHITECTURE.md § Package dependency graph; `.go-arch-lint.yml` |
| [credential-issue-rotate-flow.mmd](mermaid/credential-issue-rotate-flow.mmd) | `sequenceDiagram` | ARCHITECTURE.md § Credential issue/rotate flow (TS-1); LLD §5.4, §9.3 |
| [credential-revoke-flow.mmd](mermaid/credential-revoke-flow.mmd) | `sequenceDiagram` | ARCHITECTURE.md § Credential revoke flow (TS-2); LLD §5.4, TS-INV-7 |
| [offboarding-cascade-flow.mmd](mermaid/offboarding-cascade-flow.mmd) | `sequenceDiagram` | ARCHITECTURE.md § Offboarding cascade flow; LLD §7.1, §8.4 |
| [reconciler-sweep-flow.mmd](mermaid/reconciler-sweep-flow.mmd) | `flowchart TD` | ARCHITECTURE.md § Reconciler sweep flow (cmd/rotator); LLD §8.3, §8.6 |
| [rls-guc-flow.mmd](mermaid/rls-guc-flow.mmd) | `sequenceDiagram` | ARCHITECTURE.md § Row-Level Security and GUC injection; LLD §4.3 |
| [event-outbox-flow.mmd](mermaid/event-outbox-flow.mmd) | `sequenceDiagram` | ARCHITECTURE.md § Event and outbox flow; LLD §7.3.1, §7.4 |
| [openbao-credential-lifecycle.mmd](mermaid/openbao-credential-lifecycle.mmd) | `sequenceDiagram` | ARCHITECTURE.md § OpenBao credential custody lifecycle; LLD §6.3, §10.5 |
| [observability-stack.mmd](mermaid/observability-stack.mmd) | `graph LR` | ARCHITECTURE.md § Observability stack; LLD §11 |

**Rendering locally:** any Mermaid-aware editor plugin (VS Code "Markdown
Preview Mermaid Support", IntelliJ/GoLand's Mermaid plugin) renders these
directly; GitHub also renders `.mmd`/fenced-mermaid blocks natively. For a
quick one-off, paste a file's contents into
[mermaid.live](https://mermaid.live).
