# Tailwind Setup

This frontend uses Tailwind CSS v4 through the official Vite integration.

## Current Setup

- Tailwind is installed in `frontend/package.json` as:
  - `tailwindcss`
  - `@tailwindcss/vite`
- The Vite plugin is enabled in `frontend/vite.config.ts`.
- Tailwind is loaded from `frontend/src/styles.css` with:

```css
@import "tailwindcss";
```

- Theme tokens live in `frontend/src/styles.css` under `@theme`.
- Shared base styles live in `frontend/src/styles.css` under `@layer base`.
- A very small set of reusable shells and animations live in `frontend/src/styles.css` under `@layer components`.

## Project Rules

This frontend should follow Tailwind-first conventions.

- Prefer Tailwind utility classes directly in React components.
- Prefer static, complete class names in code.
- Prefer `@theme` for design tokens such as fonts and shared visual values.
- Prefer `@layer base` for element defaults and global resets.
- Prefer `@layer components` only for a small number of shared structural patterns.
- Keep custom CSS small and intentional.
- Keep styling close to the component that uses it.

## What To Avoid

- Do not introduce CSS Modules unless there is a very strong reason.
- Do not introduce Sass, Less, or Stylus.
- Do not move routine layout and spacing back into large hand-written stylesheets.
- Do not build class names dynamically with string fragments like ``bg-${tone}-500``.
- Do not create a large library of custom utility aliases when Tailwind utilities already express the intent clearly.

## Recommended Patterns

### Use utilities in JSX

Prefer this:

```tsx
<button className="rounded-full bg-emerald-200 px-4 py-2 font-semibold text-slate-950">
  Send
</button>
```

Instead of pushing normal component styling into separate CSS selectors.

### Keep tokens in `@theme`

When we need shared fonts or reusable design tokens, add them in `frontend/src/styles.css`:

```css
@theme {
  --font-sans: "IBM Plex Sans", "Avenir Next", "Segoe UI", sans-serif;
  --font-display: "Iowan Old Style", "Palatino Linotype", serif;
}
```

### Use CSS only where it is actually better

CSS is still appropriate for:

- keyframes and named animations
- complex background treatments
- a few shared shell classes like glass panels
- base element styling

If a style is mostly spacing, layout, color, border, or typography for one component, it should usually stay in Tailwind classes.

## File Ownership

- `frontend/src/App.tsx`
  - primary example of the Tailwind-first approach in this repo
- `frontend/src/styles.css`
  - global Tailwind entrypoint
  - theme tokens
  - base styles
  - small shared component layer
- `frontend/vite.config.ts`
  - Tailwind Vite plugin registration

## Working Agreement

When adding or changing frontend UI in this repo:

1. Start with Tailwind utilities in the component.
2. Add or reuse theme tokens if the design concept is shared.
3. Only add CSS to `styles.css` if utilities are not the right tool.
4. Keep the visual language deliberate and consistent with the existing frontend direction.

This is the default frontend styling system for the project.
