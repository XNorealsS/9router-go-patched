# AGENTS.md -- 9router-go

High-performance Go proxy gateway (`9router/proxy`, Go 1.26.0) with `chi/v5` and `modernc.org/sqlite`.

## Context
- See [PROJECT_CONTEXT.md](./PROJECT_CONTEXT.md) for architecture, packages, and key files.
- See [CLAUDE.md](./CLAUDE.md) for project-specific tooling notes (`graphify`, `rtk`).

## Scope & Hygiene
- Keep changes focused and minimal.
- Do not modify `.git/`, `.env`, or generated binaries.
- Never hardcode or log secrets, tokens, or OAuth credentials.
