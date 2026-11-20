# DESIGN.md -- 9Router Unified 1-Gateway Frontend

Dial: ENERGY 2 / RHYTHM 3 / MOTION 1

## Identity & Purpose
- **Product**: 9Router Unified 1-Gateway (self-hosted multi-provider AI proxy and Antigravity automation gateway on port `:20128`).
- **Audience**: AI coding-agent engineers (Hermes Agent, Claude Code, OpenCode, Codex CLI) and self-hosted server operators.
- **Visual Language**: High-contrast technical editorial dark default with crisp `1px` border elevation, zero external font/CDN dependencies (air-gapped Linux ready), and full Dark/Light theme parity.

## Color System (R-29, R-25, R-21)
- **Dark Theme (default, developer API/terminal workflow)**:
  - `--bg`: `#09090d`
  - `--surface`: `#111117`
  - `--surface-2`: `#171720`
  - `--border`: `#252532`
  - `--text`: `#f8fafc`
  - `--muted`: `#a1a1aa` (WCAG AA+ contrast >= 7.0:1 on dark surfaces)
  - `--accent`: `#dc2626` / `--accent-hover`: `#b91c1c` (Crimson aligned with `/gateway` and `/dashboard`)
- **Light Theme (`[data-theme="light"]`, R-34 parity)**:
  - `--bg`: `#fafafa`
  - `--surface`: `#ffffff`
  - `--surface-2`: `#f4f4f5`
  - `--border`: `#e4e4e7`
  - `--text`: `#09090b`
  - `--muted`: `#52525b` (WCAG AA+ contrast >= 6.8:1)
  - `--accent`: `#b91c1c` / `--accent-hover`: `#991b1b` (6.47:1 white-on-crimson contrast)

## Typography & Spacing Decisions (R-06, R-11, R-31)
- **Sans (`--sans`)**: `"Segoe UI", system-ui, -apple-system, BlinkMacSystemFont, sans-serif` (instant zero-latency local rendering on Windows/Linux/macOS without external requests; reserved for headlines, body, and labels).
- **Mono (`--mono`)**: `ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace` with `font-variant-numeric: tabular-nums` (used strictly for API paths, model IDs, code snippets, and live numeric counters).
- **Radius Scale**: `8px` primary containers, `6px` interactive controls/buttons, `4px` compact status tags (no pill-shaped cards).

## Structural Rules
- **R-05 / R-14**: Mixed section compositions matching RHYTHM 3 (Split Hero + Live Inspector -> 4-Stage Pipeline Strip -> Searchable Live Model Catalog -> Asymmetric Quota Ledger & CLI Integration Workbench -> 2-Group Footer).
- **R-07**: Clean solid surface contrast replaces decorative background grids or radial glow blobs.
- **R-17 / R-38**: All hero stats, failover orders, model limits, and usage totals come directly from live `/api/gateway/public-overview` SQLite + engine state, with explicit loading, empty, and error states (R-27).
