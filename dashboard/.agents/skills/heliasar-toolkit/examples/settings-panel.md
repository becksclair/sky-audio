# Example: settings panel

Use when building settings or configuration UI.

## Ingredients

- `Panel`
- `TextField`
- `TextAreaField`
- `Button`

## Pattern

- each settings group gets a `Panel`
- use helper text for explanation
- use explicit footer actions
- keep tiny local form state local unless multiple surfaces depend on it

## Skeleton

```tsx
<Panel
  title="Assistant configuration"
  description="Defaults for generation and tool usage."
  footer={
    <div class="flex justify-end gap-2">
      <Button variant="outline">Reset</Button>
      <Button>Save changes</Button>
    </div>
  }
>
  <div class="grid gap-4">
    <TextField
      id="model"
      label="Default model"
      value={model}
      onInput={(event) => setModel(event.currentTarget.value)}
      description="Used when a session doesn't override the model."
    />
    <TextAreaField
      id="system-prompt"
      label="System prompt"
      value={systemPrompt}
      onInput={(event) => setSystemPrompt(event.currentTarget.value)}
      rows={6}
      description="Kept intentionally high-level."
    />
  </div>
</Panel>
```
