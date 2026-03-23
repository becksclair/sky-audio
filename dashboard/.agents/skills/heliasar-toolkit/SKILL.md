# Heliasar Toolkit Skill

Use this skill whenever you are asked to build, modify, or extend UI in a project that uses `@heliasar/toolkit`.

## Goal

Produce consistent, agent-friendly UI by following the local contracts of this toolkit instead of inventing new patterns.

## Read first

1. `AGENTS.md`
2. `docs/tooling.md`
3. `docs/component-catalog.md`
4. `docs/architecture.md`
5. the closest example in `skills/heliasar-toolkit/examples/`

## Decision tree

### If the task is a form
Use:
- `TextField`
- `TextAreaField`
- `Button`
- `Panel`

### If the task is a chat or assistant surface
Use:
- `AppShell`
- `Panel`
- `ChatComposer`
- `MessageBubble`
- `ToolResultCard`
- `ChatModel`

### If the task is state-heavy
Use:
- `createModel()`
- `useModel()`
- signals and computed values
- explicit actions for state changes

### If the task is mostly layout
Prefer:
- composing existing toolkit components
- small helper markup inside `Panel`
- promoting a repeated pattern into the toolkit only after reuse pressure is clear

## Required coding rules

- prefer toolkit exports before raw Basecoat markup
- keep long-lived async state in models
- use Preact DOM events (`onInput`, `onClick`, `onKeyDown`)
- avoid `baseUrl` and path aliases in this package
- use relative imports inside toolkit source
- do not import Basecoat JS-powered widgets directly in feature code
- do not add public API without updating the catalog and examples

## Model pattern

Use `createModel()` when a surface needs:
- shared state across multiple components
- async workflows
- cancellation
- derived booleans like `canSubmit`
- future extensibility

Use local component state only for:
- transient visual toggles
- one-off focus or hover details
- tiny non-shared state

## Workflow

1. Identify the closest approved component or model.
2. Copy the nearest local example.
3. Adapt props and state shape.
4. Keep behavior in Preact and models.
5. Update docs if you introduced a new public pattern.
6. Run:
   - `npm run typecheck`
   - `npm run lint`
   - `npm run fmt:check`
   - `npm run build`

## Anti-patterns

- raw class soup in app code when a wrapper exists
- async request logic hidden inside buttons or message bubbles
- direct DOM querying to coordinate state
- new visual variants that are not documented
- new public API with no example

## Local references

- `docs/component-catalog.md`
- `docs/component-catalog.json`
- `docs/architecture.md`
- `docs/tooling.md`
- `src/components/*`
- `src/state/chat-model.ts`
- `examples/App.tsx`

## Fast path reminder

If `tsgo` or Oxlint looks suspicious, confirm with:

```bash
npm run typecheck:compat
```
