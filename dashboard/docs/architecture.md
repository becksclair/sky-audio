# Toolkit architecture

## Layering

### Layer 1: visual primitives
Own the stable reusable pieces:
- `Button`
- `Panel`
- `TextField`
- `TextAreaField`

These are thin wrappers around Basecoat plus a small amount of toolkit structure.

### Layer 2: product surface compositions
Own the recurring application surfaces:
- `AppShell`
- `ChatComposer`
- `MessageBubble`
- `ToolResultCard`

These are reusable but also encode product-level conventions.

### Layer 3: feature models
Own long-lived feature state with Signals Models:
- `ChatModel`
- future models like `CommandPaletteModel`, `WorkspaceModel`, `SettingsModel`

Models should encapsulate:
- signals
- derived state
- actions
- abort controllers
- lifecycle cleanup

### Layer 4: host app adapters
The host app should assemble routes, plugin views, or platform adapters around the toolkit.

Examples:
- Obsidian view adapter
- web app page adapter
- internal admin console shell

## Public API shape

The package exports:
- components
- models
- shared types
- compiled CSS

It does **not** expose third-party widget internals as first-class API.

## Styling contract

- Basecoat provides the baseline visual language.
- Toolkit components provide stable contracts.
- Toolkit CSS provides a little glue and layout behavior.
- Feature code should not invent new visual systems.

## Behavior contract

- Preact owns interactive behavior.
- Signals Models own long-lived feature state.
- Avoid hidden magic.
- Avoid ad hoc lifecycle side effects in leaf components.

## Agent contract

Agents should be able to answer these questions locally:
- Which component should I use?
- Where should state live?
- What does the preferred composition look like?
- What is forbidden?
- Which example is closest?

If those answers are not obvious from local docs, add docs.

## Promotion rules

Promote code into the toolkit when at least one of these is true:
- it is reused in 2+ features
- it defines an important visual contract
- it encodes a state pattern agents keep needing
- it is a recurring source of inconsistency

Leave code in the host app when:
- it is one-off platform glue
- it is business-logic specific
- it only exists for a single feature with no reuse pressure
