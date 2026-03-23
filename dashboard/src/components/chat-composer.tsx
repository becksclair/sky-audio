import type { JSX } from 'preact';

import { Button } from './button';
import { cx } from '../lib/cx';

export interface ChatComposerProps {
  value: string;
  onValueChange(value: string): void;
  onSubmit(): void | Promise<void>;
  onCancel?(): void;
  disabled?: boolean;
  busy?: boolean;
  placeholder?: string;
  hint?: string;
  rows?: number;
  className?: string;
  textareaProps?: Omit<JSX.TextareaHTMLAttributes<HTMLTextAreaElement>, 'value' | 'onInput'>;
}

export function ChatComposer({
  value,
  onValueChange,
  onSubmit,
  onCancel,
  disabled = false,
  busy = false,
  placeholder = 'Ask anything…',
  hint = 'Enter to send, Shift+Enter for newline.',
  rows = 6,
  className,
  textareaProps,
}: ChatComposerProps) {
  const canSubmit = value.trim().length > 0 && !disabled && !busy;

  async function handleSubmit() {
    if (!canSubmit) {
      return;
    }

    await onSubmit();
  }

  function handleKeyDown(event: JSX.TargetedKeyboardEvent<HTMLTextAreaElement>) {
    if (event.key !== 'Enter' || event.shiftKey) {
      return;
    }

    event.preventDefault();
    void handleSubmit();
  }

  return (
    <div class={cx('card htk-composer', className)}>
      <section class="grid gap-3">
        <textarea
          {...textareaProps}
          rows={rows}
          class={cx('textarea min-h-32', textareaProps?.className as string | undefined)}
          value={value}
          disabled={disabled || busy}
          placeholder={placeholder}
          onInput={(event) =>
            onValueChange((event.currentTarget as HTMLTextAreaElement).value)
          }
          onKeyDown={handleKeyDown}
        />

        <div class="flex flex-wrap items-center justify-between gap-3">
          <p class="text-muted-foreground text-sm">{hint}</p>

          <div class="flex items-center gap-2">
            {busy && onCancel ? (
              <Button variant="outline" onClick={onCancel}>
                Cancel
              </Button>
            ) : null}
            <Button onClick={() => void handleSubmit()} loading={busy} disabled={!canSubmit}>
              Send
            </Button>
          </div>
        </div>
      </section>
    </div>
  );
}
