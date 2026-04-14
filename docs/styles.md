# Explorer UI styles (Reference app alignment)

This document defines how the Explorer frontend should **visually align** with the reference Electron app while staying **Tailwind-first** and consistent with [`frontend/TAILWIND.md`](../frontend/TAILWIND.md).

## Source of truth

| Location | Role |
| -------- | ---- |
| **Reference app** | Reference app: Electron + React + Tailwind CSS v4 (`@import "tailwindcss"` in `src/index.css`). |
| **Explorer** (`frontend/`) | Implementation target: Vite + React 19 + Tailwind v4. |

Reference-app global styling, scrollbars, markdown/code surfaces, and panel-specific overrides live primarily in:

- `src/index.css` in the reference app

Component-level patterns use explicit hex utilities (examples: `bg-[#2a2a2a]`, `border-[#333333]`, `text-[#b2b2b2]`) alongside Tailwind layout utilities—see e.g. `src/components/chat/ChatInput.tsx`, `src/components/sidebar/ChatList.tsx`.

## Reference imagery (reference app repo root)

Use these as **visual references** for density, contrast, and chrome (not as runtime assets unless copied into Explorer):

| File (reference app root) | Typical use |
| ----------------------- | ----------- |
| `image1.png` | Overall shell / dark IDE feel |
| `image2.png` | Secondary layout or panel reference |
| `image3.png` | Additional UI reference |
| `overlay.png` | Overlay / modal reference |

When implementing Explorer, prefer **theme tokens** in `frontend/src/styles.css` (`@theme`) that match these screenshots rather than one-off hex scattered everywhere.

## Design tokens (from the reference app `index.css`)

Map these into Explorer via `@theme` (CSS variables) so components use `bg-background`, `text-foreground`, `border-border`, etc., or keep explicit `bg-[var(--color-…)]` for parity during migration.

### Surfaces

| Token idea | Approximate value | Usage |
| ---------- | ----------------- | ----- |
| App background | `#181818` | Root / canvas |
| Panel / elevated | `#2b2b2b` | Inputs, tables, light “cards” in dark mode |
| Row hover (lists) | `#2d2d2d` | Sidebar rows, `hover:bg-gray-50` overrides |
| User message bubble | `#2a2a2a` | User chat bubbles, menus, popovers |
| Border | `#333333` | Dividers, inputs, table borders |

### Text

| Token idea | Approximate value | Usage |
| ---------- | ----------------- | ----- |
| Primary text | `#d4d4d4` | Body |
| Secondary | `#aaaaaa` | Muted labels (`text-gray-700` overrides in sidebar/main) |
| Tertiary | `#888888` | Timestamps, meta (`text-gray-400` in sidebar) |
| Interactive muted | `#b2b2b2` | Icons, secondary buttons |
| Headings on dark | `#ffffff` | `h1`–`h4` in main content |

### Accents

| Token idea | Approximate value | Usage |
| ---------- | ----------------- | ----- |
| Links | `text-blue-400`, hover `text-blue-300` | Markdown links |
| Info callouts | `#1e293b` bg, `border-[#1e3a8a]`, `text-blue-200` | Blue-50 replacements in main |
| Checkbox | `#4a4a4a` | `accent-color` globally |

### Reasoning / secondary markdown

The reference app uses a **muted gray** for reasoning blocks (e.g. `.reasoning-markdown` at `#6d6d6d`). Reserve a named token if Explorer renders reasoning separately from main assistant text.

### Syntax highlighting (code blocks)

`index.css` documents a **VS Code Dark Modern**-style palette (e.g. keywords `#569cd6`, strings `#ce9178`, comments `#6a9955`). When Explorer renders fenced code, reuse the same palette via a single `.hljs` scope or Shiki theme—not ad hoc per-block colors.

## Scrollbars

The reference app standardizes **thin, neutral gray** scrollbars with **no rounded thumbs** on WebKit (square corners). Patterns include:

- Global `*` rules (width 5px, thumb `rgba(136,136,136,0.4)`).
- Named classes: `.scrollbar-thin`, `.sidebar-scrollbar`, `.chat-list-scrollbar` (hidden thumb until hover on container).

In Explorer:

- Prefer one **canonical** scrollbar treatment in `@layer base` or a single utility class imported from `styles.css`.
- Avoid duplicating full WebKit blocks per component; reuse a class like `explorer-scrollbar` once defined.

## Layout regions (semantic hooks)

The reference app uses ID-scoped overrides for light Tailwind grays remapped to dark:

- `#main-content` — primary reading surface; remaps `bg-white`, `text-gray-*`, borders.
- `#sidebar` — list chrome; same idea for gray text tiers.

Explorer should prefer **data attributes or layout wrapper classes** (e.g. `data-panel="main"`) over `#id` selectors unless we truly need a single global root—IDs are brittle in multi-pane layouts.

## Markdown and rich content

The reference app styles `.markdown-content` for headings, lists, blockquotes, tables, inline code, and fenced blocks. When Explorer adds markdown:

- Scope prose under one wrapper class (e.g. `.explorer-markdown`) to match spacing and link colors.
- Keep **table** and **blockquote** borders aligned with `#333333` / `#444444` per reference.

## Tailwind best practices (Explorer)

These rules apply on top of [`frontend/TAILWIND.md`](../frontend/TAILWIND.md):

1. **Tokens first** — Add shared colors, radii, and fonts to `@theme` in `frontend/src/styles.css`; use semantic names in JSX where it improves consistency with the reference app.
2. **Static class strings** — Prefer full utility strings in source; avoid `` `bg-${color}-500` ``-style dynamic assembly (breaks Tailwind’s static scan).
3. **Small global CSS** — Global file is for base resets, scrollbars, keyframes, and a **limited** set of shared shells (glass panels, markdown container)—not every screen’s layout.
4. **Co-locate intent** — Layout and spacing stay in the component; repeated “reference app card” patterns become one `@layer components` class or a tiny set of theme colors.
5. **Accessibility** — When using very dark surfaces, keep **focus-visible** styles explicit; the reference app removes focus rings in some sidebar areas for Electron—**web** Explorer should retain visible focus for keyboard users unless a deliberate exception is documented.
6. **Resizable panels** — Pane chrome (resize handles, borders) should use the same border token (`#333333` family) so splits match sidebar dividers in the reference app.

## Implementation checklist

When porting a screen or component:

- [ ] Match background and text tokens to the table above (or to `@theme` aliases).
- [ ] Reuse shared scrollbar class(es) from `styles.css`.
- [ ] Use borders `#333333` / `#2a2a2a` family for panels and inputs unless the design token says otherwise.
- [ ] Compare against `image1.png` / `image2.png` in the reference app root for overall contrast.
- [ ] Update this doc if new canonical tokens are added to `frontend/src/styles.css`.

## Related files

- [`frontend/TAILWIND.md`](../frontend/TAILWIND.md) — Tailwind v4 setup and project rules.
- [`frontend/src/styles.css`](../frontend/src/styles.css) — `@import "tailwindcss"`, `@theme`, `@layer base/components`.
