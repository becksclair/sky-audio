# Tooling

This toolkit uses a fast JS/TS toolchain on purpose.

## Stack

- **Vite 8** for dev server and library build
- **TypeScript-go (`tsgo`)** for fast local typechecking
- **Oxlint + `oxlint-tsgolint`** for type-aware linting
- **Oxfmt** for formatting
- **TypeScript (`tsc`)** only for declaration emission and compatibility fallback

## Why this setup?

### Vite 8
Vite 8 ships with a Rolldown-powered production build pipeline and keeps the smooth dev-server flow that makes it a great fit for browser-oriented libraries.

### tsgo
`tsgo` is extremely promising for fast local typechecking and editor feedback. This repo uses it as the default local typecheck path.

### Oxlint type-aware
Oxlint's type-aware mode runs on top of `tsgolint`, which uses `typescript-go` for semantic analysis. That gives you one fast stack for lint + TS-aware rules instead of the usual slow ESLint + TS server setup.

### Oxfmt
Use Oxfmt for fast, consistent formatting. No Prettier is needed here.

## Constraints we intentionally accept

### No `baseUrl` / `paths`
Oxlint's type-aware docs explicitly call out that some legacy `tsconfig` options like `baseUrl` are not supported. This starter avoids those options to keep the fast path reliable.

### Keep `tsc` around
TypeScript-go is still a preview. We keep classic TypeScript around for:
- declaration emission
- compatibility fallback
- sanity-checking weird edge cases

That means the workflow is:

- everyday speed -> `tsgo` and Oxlint
- package correctness -> `vite build` + `tsc --emitDeclarationOnly`

## Commands

```bash
npm run dev
npm run typecheck
npm run typecheck:compat
npm run lint
npm run fmt
npm run fmt:check
npm run build
npm run check
```

## Editor setup

This repo includes `.vscode/settings.json` with:

```json
{
  "js/ts.experimental.useTsgo": true,
  "oxc.typeAware": true
}
```

## Practical advice

- if the fast path passes, trust it most of the time
- if something smells off, run `npm run typecheck:compat`
- do not reintroduce config complexity just for import aliases
- prefer the boring fast stack over clever config stunts
