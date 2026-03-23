# Example: streaming chat surface

Use when building an AI assistant or command surface.

## Ingredients

- `AppShell`
- `Panel`
- `MessageBubble`
- `ToolResultCard`
- `ChatComposer`
- `ChatModel`

## Pattern

- left sidebar = context, history, model picker, or controls
- main panel = timeline + composer
- state lives in `ChatModel`
- composer only emits intent
- model action handles async work and cancellation

## Skeleton

```tsx
const chat = useModel(() => new ChatModel());

async function handleSubmit() {
  await chat.submit(async (draft) => {
    await wait(200);
    return {
      role: 'assistant',
      content: `Processed: ${draft}`,
    };
  });
}

return (
  <AppShell
    sidebar={<Panel title="Context">...</Panel>}
    topbar={<div>Assistant</div>}
  >
    <Panel title="Conversation" padded={false}>
      <div class="htk-chat">
        {chat.messages.value.map((message) => (
          <MessageBubble key={message.id} message={message} />
        ))}
      </div>
    </Panel>

    <ChatComposer
      value={chat.draft.value}
      onValueChange={chat.setDraft}
      onSubmit={handleSubmit}
      onCancel={chat.abort}
      busy={chat.busy.value}
      hint="Enter to send, Shift+Enter for newline."
    />
  </AppShell>
);
```

## Why this is the preferred pattern

- behavior is centralized
- components stay dumb enough for agents to reuse
- async work is easy to test
- cancellation is not bolted on later
