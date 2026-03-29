import { Panel } from './panel';
import { cx } from '../lib/cx';
import type { ToolResultStatus } from '../types';

export interface ToolResultCardProps {
  title: string;
  status?: ToolResultStatus;
  summary?: string;
  payload?: unknown;
  className?: string;
}

const statusTone: Record<ToolResultStatus, string> = {
  ok: 'text-emerald-600 dark:text-emerald-400',
  working: 'text-amber-600 dark:text-amber-400',
  error: 'text-red-600 dark:text-red-400',
};

export function ToolResultCard({
  title,
  status = 'ok',
  summary,
  payload,
  className,
}: ToolResultCardProps) {
  return (
    <Panel
      className={cx('htk-tool-result', className)}
      title={title}
      {...(summary && { description: summary })}
      actions={
        <span class={cx('text-xs font-medium uppercase tracking-[0.12em]', statusTone[status])}>
          {status}
        </span>
      }
    >
      {payload !== undefined ? (
        <pre class="htk-pre">{JSON.stringify(payload, null, 2)}</pre>
      ) : (
        <p class="text-muted-foreground text-sm">No structured payload.</p>
      )}
    </Panel>
  );
}
