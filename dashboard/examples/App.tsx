import { useModel } from '@preact/signals';

import {
  AppShell,
  Button,
  ChatComposer,
  ChatModel,
  MessageBubble,
  Panel,
  TextAreaField,
  TextField,
} from '../src';

function wait(ms: number) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

export function App() {
  const chat = useModel(
    () =>
      new ChatModel({
        draft: 'Summarize the failing test and propose the smallest safe fix.',
        messages: [
          {
            id: 'seed-1',
            role: 'assistant',
            content:
              'Toolkit ready. Ask for a patch, a review, or a plan and I will structure the result.',
            createdAt: Date.now() - 1000 * 60 * 4,
          },
        ],
      }),
  );

  async function handleSubmit() {
    await chat.submit(async (draft) => {
      await wait(450);

      return [
        {
          role: 'assistant',
          content: `I would break this into three steps: inspect the failing path, patch the narrowest surface, then rerun checks. Prompt received: "${draft}"`,
        },
        {
          role: 'tool',
          content: 'Captured a structured result from a fake tool call.',
          toolResult: {
            title: 'code-search',
            status: 'ok',
            summary: 'Found 4 likely matches and one suspicious mutation site.',
            payload: {
              files: [
                'src/state/chat-model.ts',
                'src/components/chat-composer.tsx',
                'src/components/message-bubble.tsx',
                'docs/component-catalog.md',
              ],
              suspiciousMutation: 'messages.push(...)',
            },
          },
        },
      ];
    });
  }

  return (
    <AppShell
      sidebar={
        <div class="grid gap-4">
          <Panel
            title="Heliasar Toolkit"
            description="Internal Preact + Basecoat platform for agent-generated UI."
            footer={<Button variant="outline">View docs</Button>}
          >
            <p class="text-sm text-muted-foreground">
              This playground demonstrates the preferred shell, chat, and form patterns.
            </p>
          </Panel>

          <Panel title="Assistant settings">
            <div class="grid gap-4">
              <TextField
                id="default-model"
                label="Default model"
                value="gpt-fast-ui"
                description="Used when the surface doesn't override the model."
                onInput={() => {}}
              />
              <TextAreaField
                id="system-prompt"
                label="System prompt"
                rows={5}
                value="Be concise, structured, and tool-aware."
                description="Agents should keep changes local and update the catalog."
                onInput={() => {}}
              />
            </div>
          </Panel>
        </div>
      }
      topbar={
        <div class="flex w-full items-center justify-between gap-3">
          <div>
            <h1 class="text-xl font-semibold">Agent-friendly toolkit playground</h1>
            <p class="text-muted-foreground text-sm">
              Vite 8 + Preact + Basecoat + Signals Models
            </p>
          </div>

          <div class="flex items-center gap-2">
            <Button variant="outline">Reset</Button>
            <Button>Publish package</Button>
          </div>
        </div>
      }
      footer={
        <p class="text-muted-foreground text-sm">
          Enter to send, Shift+Enter for newline, and keep long-lived state in models.
        </p>
      }
    >
      <Panel
        title="Conversation"
        description="The model owns async orchestration. Components stay composable."
      >
        <div class="htk-chat">
          {chat.messages.value.map((message) => (
            <MessageBubble key={message.id} message={message} />
          ))}
        </div>
      </Panel>

      {chat.error.value ? (
        <Panel
          title="Last error"
          description="Errors live in model state, not inside the composer."
          actions={
            <Button variant="ghost" onClick={() => chat.clearError()}>
              Dismiss
            </Button>
          }
        >
          <p class="text-sm text-red-600 dark:text-red-400">{chat.error.value}</p>
        </Panel>
      ) : null}

      <ChatComposer
        value={chat.draft.value}
        onValueChange={(value) => chat.setDraft(value)}
        onSubmit={handleSubmit}
        onCancel={() => chat.abort()}
        busy={chat.busy.value}
        hint="Enter to send. Shift+Enter for a newline. Model state handles the rest."
      />
    </AppShell>
  );
}
