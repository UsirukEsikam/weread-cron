## Development environment

Development uses pi agent. Use the tools provided by pi. Do not invoke or delegate work to other coding agents or agent CLIs.

## Language

- Use natural Chinese in conversation with the user unless requested otherwise.
- Use ASD-STE100-style Simplified Technical English for prose in project documents unless the document has an established different style.

## Agent skills

### Issue tracker

Issues and specs live as markdown files under `.scratch/` (one directory per feature). See `docs/agents/issue-tracker.md`.

### Triage labels

Default five-role vocabulary: `needs-triage`, `needs-info`, `ready-for-agent`, `ready-for-human`, `wontfix`. See `docs/agents/triage-labels.md`.

### Domain docs

Single-context: root `CONTEXT.md` (glossary) + `docs/adr/` (decisions). See `docs/agents/domain.md`.