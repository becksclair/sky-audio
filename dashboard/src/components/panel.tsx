import type { ComponentChildren } from 'preact';

import { cx } from '../lib/cx';

export interface PanelProps {
  title?: string;
  description?: string;
  actions?: ComponentChildren;
  footer?: ComponentChildren;
  padded?: boolean;
  className?: string;
  children?: ComponentChildren;
}

export function Panel({
  title,
  description,
  actions,
  footer,
  padded = true,
  className,
  children,
}: PanelProps) {
  return (
    <section class={cx('card htk-panel', className)}>
      {(title || description || actions) && (
        <header class="htk-panel__header">
          <div class="grid gap-1">
            {title ? <h2 class="text-lg font-semibold">{title}</h2> : null}
            {description ? (
              <p class="text-muted-foreground text-sm">{description}</p>
            ) : null}
          </div>

          {actions ? <div class="htk-panel__actions">{actions}</div> : null}
        </header>
      )}

      <section class={cx(padded ? 'htk-panel__body' : 'htk-panel__body--flush')}>
        {children}
      </section>

      {footer ? <footer class="htk-panel__footer">{footer}</footer> : null}
    </section>
  );
}
