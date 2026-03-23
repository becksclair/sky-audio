import type { ComponentChildren, JSX } from 'preact';

import { cx } from '../lib/cx';

export interface AppShellProps {
  sidebar?: ComponentChildren;
  topbar?: ComponentChildren;
  footer?: ComponentChildren;
  sidebarWidth?: string;
  className?: string;
  children?: ComponentChildren;
}

export function AppShell({
  sidebar,
  topbar,
  footer,
  sidebarWidth = '20rem',
  className,
  children,
}: AppShellProps) {
  const style = sidebar
    ? ({ '--htk-sidebar-width': sidebarWidth } as JSX.CSSProperties)
    : undefined;

  return (
    <div
      class={cx('htk-shell', sidebar && 'htk-shell--with-sidebar', className)}
      style={style}
    >
      {sidebar ? <aside class="htk-sidebar">{sidebar}</aside> : null}

      <div class="htk-main">
        {topbar ? <header class="htk-topbar">{topbar}</header> : null}
        <main class="htk-content">{children}</main>
        {footer ? <footer class="htk-footer">{footer}</footer> : null}
      </div>
    </div>
  );
}
