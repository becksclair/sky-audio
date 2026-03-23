# Component catalog

This is the canonical source for what agents should use first.

## Components

### `Button`
Import:
```ts
import { Button } from '@heliasar/toolkit';
```

Use when:
- primary or secondary actions
- form submission
- inline surface actions

Avoid when:
- plain text links
- icon-only controls without an accessibility label

Props:
- `variant`: `primary | secondary | destructive | outline | ghost | link`
- `size`: `md | sm | lg`
- `loading?: boolean`

---

### `Panel`
Import:
```ts
import { Panel } from '@heliasar/toolkit';
```

Use when:
- bounded surface sections
- settings cards
- side panels
- tool result groups

Props:
- `title?: string`
- `description?: string`
- `actions?: ComponentChildren`
- `footer?: ComponentChildren`
- `padded?: boolean`

---

### `TextField`
Import:
```ts
import { TextField } from '@heliasar/toolkit';
```

Use when:
- text, email, password, search, and numeric input
- form fields with helper or error text

Props:
- standard input props
- `label: string`
- `description?: string`
- `error?: string`
- `fieldClassName?: string`
- `inputClassName?: string`

---

### `TextAreaField`
Import:
```ts
import { TextAreaField } from '@heliasar/toolkit';
```

Use when:
- notes
- prompts
- feedback
- multiline text forms outside the dedicated composer

Props:
- standard textarea props
- `label: string`
- `description?: string`
- `error?: string`

---

### `AppShell`
Import:
```ts
import { AppShell } from '@heliasar/toolkit';
```

Use when:
- two-pane or three-region app shells
- assistant sidebars
- settings + content layouts

Props:
- `sidebar?: ComponentChildren`
- `topbar?: ComponentChildren`
- `footer?: ComponentChildren`
- `sidebarWidth?: string`

---

### `ChatComposer`
Import:
```ts
import { ChatComposer } from '@heliasar/toolkit';
```

Use when:
- prompt entry
- multiline assistant input
- async send/cancel flows

Props:
- `value: string`
- `onValueChange(value: string): void`
- `onSubmit(): void | Promise<void>`
- `onCancel?(): void`
- `disabled?: boolean`
- `busy?: boolean`
- `placeholder?: string`
- `hint?: string`

Behavior:
- Enter submits
- Shift+Enter adds newline
- submit is blocked when empty or disabled

---

### `MessageBubble`
Import:
```ts
import { MessageBubble } from '@heliasar/toolkit';
```

Use when:
- rendering chat or assistant timelines

Props:
- `message: ChatMessage`

---

### `ToolResultCard`
Import:
```ts
import { ToolResultCard } from '@heliasar/toolkit';
```

Use when:
- structured tool outputs
- logs
- command results
- API payload summaries

Props:
- `title: string`
- `status?: 'ok' | 'working' | 'error'`
- `summary?: string`
- `payload?: unknown`

## Models

### `ChatModel`
Import:
```ts
import { ChatModel } from '@heliasar/toolkit';
```

Use when:
- a surface has prompt input
- messages stream over time
- cancellation is needed
- a feature needs assistant orchestration

Core signals:
- `messages`
- `draft`
- `busy`
- `error`
- `canSubmit`

Core actions:
- `setDraft`
- `appendMessage`
- `patchMessage`
- `submit`
- `abort`
- `reset`

## Selection rules

1. Prefer an existing component.
2. If two existing components are close, compose them.
3. If repetition appears across features, promote the pattern.
4. Do not add a new public component for a one-off layout unless the pattern is clearly recurring.
