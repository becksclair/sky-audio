# AGENTS.md

This repo is an **internal UI platform**, not a random component pile.

If you are an agent touching UI code here, read in this order:

1. `skills/heliasar-toolkit/SKILL.md`
2. `docs/tooling.md`
3. `docs/component-catalog.md`
4. `docs/architecture.md`
5. `examples/App.tsx`

## Mission

Build consistent, stateful, accessible product UI with:

- **Preact** for rendering and DOM-friendly composition
- **Signals Models** for long-lived state and async orchestration
- **Basecoat** for readable visual primitives
- **Toolkit wrappers** for the public UI API
- **Vite 8 + tsgo + Oxlint + Oxfmt** for fast feedback loops

## Non-negotiable rules

### 1) Prefer toolkit components over raw markup

If `Button`, `Panel`, `TextField`, `TextAreaField`, `ChatComposer`, `MessageBubble`, `ToolResultCard`, or `AppShell` fit the need, use them.

### 2) Wrap interaction in Preact

Do **not** import Basecoat's JavaScript-powered widgets directly in feature code.

Bad:
- direct Basecoat JS imports in a feature
- manual DOM mutation for dropdowns/popovers/tabs
- feature-local shadow APIs for layout primitives

Good:
- toolkit wrapper component
- Preact state + semantic HTML + Basecoat classes
- promoting repeated patterns into the toolkit package

### 3) Put long-lived state in Models

Use `createModel()` for:
- chat state
- streaming or async workflows
- abort/cancelable operations
- multi-panel coordination
- feature-level orchestration

Keep leaf component state local only for:
- temporary UI toggles
- ephemeral draft affordances
- focus or hover details

### 4) Use Preact's DOM event model

Unless the project intentionally uses `preact/compat`, prefer:
- `onInput` for text entry
- `onClick`
- `onKeyDown`
- standard DOM event names

### 5) Keep tsgo and Oxlint happy

- avoid `baseUrl` and `paths` in tsconfig
- prefer relative imports inside the toolkit package
- keep config simple and modern
- if `tsgo` behaves oddly, use `npm run typecheck:compat` before changing architecture

### 6) Do not introduce visual drift

If you add a new surface or variant:
- update `docs/component-catalog.md`
- update `docs/component-catalog.json`
- add an example in `skills/heliasar-toolkit/examples/`
- document when to use it

### 7) Always handle the full state set

For stateful surfaces, include:
- empty
- loading
- success
- error
- disabled/busy
- keyboard behavior

## File placement

- primitives and reusable compositions -> `src/components/`
- shared helpers -> `src/lib/`
- feature models -> `src/state/`
- public exports -> `src/index.ts`
- agent-facing docs -> `docs/` and `skills/`

## Coding defaults

- prefer semantic HTML
- prefer small focused components
- prefer composition over inheritance
- prefer explicit props over hidden context
- prefer immutable updates for arrays/objects in signals
- prefer one exported component per file
- prefer one model per feature surface
- prefer local examples over abstract explanation

## Forbidden patterns

- raw utility-class soup in feature code when an existing toolkit component fits
- async request orchestration inside button components
- global mutable singletons for feature state
- direct DOM querying as state management
- adding a new component without updating docs/examples
- shipping a Preact surface that has no empty/error/loading story

## Tooling workflow

Run these before you stop:

```bash
npm run typecheck
npm run lint
npm run fmt:check
npm run build
```

If the fast path looks wrong:

```bash
npm run typecheck:compat
```

## Review checklist

Before you stop:
- Does this use the preferred toolkit component?
- Does state live in the right layer?
- Are DOM events Preact-correct?
- Did you update the catalog?
- Did you add or update an example?
- Would another agent understand the intended pattern from local docs alone?
