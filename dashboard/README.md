# @heliasar/toolkit

Reusable internal UI toolkit built on **Preact + Signals + Basecoat**, with a fast local toolchain centered on **Vite 8**, **tsgo**, **Oxlint type-aware linting**, and **Oxfmt**.

This starter is optimized for:

- rich, state-heavy application surfaces
- internal reuse across apps and plugins
- coding agents that need strong local conventions, canonical examples, and machine-readable references
- keeping **behavior in Preact** and **styling in Basecoat + toolkit wrappers**
- fast feedback loops without leaning on a slow ESLint + TS stack

## What this package is

This is not just “some components”.

It is an **internal UI platform** made of:

1. a reusable package: `@heliasar/toolkit`
2. a local skill bundle in `skills/heliasar-toolkit/`
3. an `AGENTS.md` contract that tells agents how to work with the stack
4. examples and references that make code generation predictable

## Core choices

- **Preact** for rendering and DOM-friendly component behavior
- **@preact/signals** for fast state and models
- **Basecoat** for readable visual primitives like `btn`, `card`, `input`, `textarea`
- **Vite 8** for dev + library builds
- **TypeScript-go (`tsgo`)** for fast local typechecking
- **Oxlint + oxlint-tsgolint** for type-aware linting
- **Oxfmt** for fast formatting

## Repo shape

```text
heliasar-toolkit/
├── .oxlintrc.json
├── .vscode/
│   └── settings.json
├── AGENTS.md
├── README.md
├── docs/
│   ├── architecture.md
│   ├── component-catalog.json
│   ├── component-catalog.md
│   ├── references.md
│   └── tooling.md
├── examples/
│   ├── App.tsx
│   └── main.tsx
├── index.html
├── package.json
├── skills/
│   └── heliasar-toolkit/
│       ├── REFERENCES.md
│       ├── SKILL.md
│       └── examples/
│           ├── settings-panel.md
│           └── streaming-chat.md
├── src/
│   ├── components/
│   ├── lib/
│   ├── state/
│   ├── global.d.ts
│   ├── index.ts
│   ├── styles.css
│   └── types.ts
├── tsconfig.build.json
├── tsconfig.json
└── vite.config.ts
```

## Install

```bash
npm install
```

## Fast local workflow

```bash
npm run dev
npm run typecheck
npm run lint
npm run fmt
npm run build
```

## Scripts

- `npm run dev` -> local Vite 8 playground using `index.html`
- `npm run build` -> build library with Vite and emit declarations with `tsc`
- `npm run typecheck` -> fast local check using `tsgo`
- `npm run typecheck:compat` -> fallback check using `tsc`
- `npm run lint` -> Oxlint with `--type-aware --type-check`
- `npm run fmt` -> Oxfmt write
- `npm run fmt:check` -> Oxfmt check only

## Why both tsgo and tsc?

Use `tsgo` as the **fast local path**.

Keep `tsc` around for **declaration emission** and as a compatibility fallback, because TypeScript-go is still a preview and does not yet have full feature parity with classic TypeScript. This starter treats `tsgo` as the default for feedback speed, not as the only source of truth.

## Why no tsconfig path aliases?

To keep Oxlint's type-aware mode happy and avoid weird tsgo / tooling edge cases, this template intentionally avoids `baseUrl` and `paths`. Use relative imports inside the toolkit package.

That trade is worth it for speed and reliability.

## Public package API

The package exports:

- toolkit components
- shared types
- signals models
- compiled CSS as `@heliasar/toolkit/style.css`

The JS entry also imports the CSS, so bundlers that support CSS side effects will usually pick it up automatically.

## Extension workflow

When you add a new primitive or surface component:

1. Add it under `src/components/`.
2. Export it from `src/index.ts`.
3. Add it to `docs/component-catalog.md`.
4. Add it to `docs/component-catalog.json`.
5. Add or update a recipe in `skills/heliasar-toolkit/examples/`.
6. Update `skills/heliasar-toolkit/SKILL.md` if the preferred pattern changed.

## What not to do

- do not import Basecoat's JS-powered widgets directly in feature code
- do not add tsconfig alias tricks that break type-aware linting
- do not let agents invent new UI variants without updating the catalog
- do not put async orchestration in leaf components
- do not bypass toolkit wrappers when an approved component already exists

## First read

1. `AGENTS.md`
2. `skills/heliasar-toolkit/SKILL.md`
3. `docs/tooling.md`
4. `docs/component-catalog.md`
