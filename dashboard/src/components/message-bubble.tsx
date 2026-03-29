import { cx } from '../lib/cx';
import type { ChatMessage } from '../types';
import { ToolResultCard } from './tool-result-card';

export interface MessageBubbleProps {
  message: ChatMessage;
  className?: string;
}

const roleLabel: Record<ChatMessage['role'], string> = {
  user: 'User',
  assistant: 'Assistant',
  system: 'System',
  tool: 'Tool',
};

export function MessageBubble({ message, className }: MessageBubbleProps) {
  return (
    <article class={cx('card htk-message', `htk-message--${message.role}`, className)}>
      <header class="flex items-center justify-between gap-3">
        <p class="text-xs font-medium uppercase tracking-[0.12em] text-muted-foreground">
          {roleLabel[message.role]}
        </p>
        <time class="text-xs text-muted-foreground">
          {new Date(message.createdAt).toLocaleTimeString([], {
            hour: '2-digit',
            minute: '2-digit',
          })}
        </time>
      </header>

      <section class="grid gap-3">
        <div class="whitespace-pre-wrap text-sm leading-6">{message.content}</div>

        {message.toolResult ? (
          <ToolResultCard
            title={message.toolResult.title}
            {...(message.toolResult.status && { status: message.toolResult.status })}
            {...(message.toolResult.summary && { summary: message.toolResult.summary })}
            payload={message.toolResult.payload}
          />
        ) : null}
      </section>
    </article>
  );
}
