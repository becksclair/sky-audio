import type { ComponentChildren, JSX } from 'preact';

import { cx } from '../lib/cx';

export type ButtonVariant =
  | 'primary'
  | 'secondary'
  | 'destructive'
  | 'outline'
  | 'ghost'
  | 'link';

export type ButtonSize = 'md' | 'sm' | 'lg';

export interface ButtonProps
  extends Omit<JSX.ButtonHTMLAttributes<HTMLButtonElement>, 'size'> {
  children?: ComponentChildren;
  variant?: ButtonVariant;
  size?: ButtonSize;
  loading?: boolean;
}

function resolveButtonClass(variant: ButtonVariant, size: ButtonSize): string {
  if (size === 'md') {
    switch (variant) {
      case 'primary':
        return 'btn';
      case 'secondary':
        return 'btn-secondary';
      case 'destructive':
        return 'btn-destructive';
      case 'outline':
        return 'btn-outline';
      case 'ghost':
        return 'btn-ghost';
      case 'link':
        return 'btn-link';
    }
  }

  const sizePrefix = size === 'sm' ? 'btn-sm' : 'btn-lg';

  switch (variant) {
    case 'primary':
      return sizePrefix;
    case 'secondary':
      return `${sizePrefix}-secondary`;
    case 'destructive':
      return `${sizePrefix}-destructive`;
    case 'outline':
      return `${sizePrefix}-outline`;
    case 'ghost':
      return `${sizePrefix}-ghost`;
    case 'link':
      return `${sizePrefix}-link`;
  }
}

export function Button({
  children,
  className,
  variant = 'primary',
  size = 'md',
  loading = false,
  disabled,
  type = 'button',
  ...props
}: ButtonProps) {
  return (
    <button
      {...props}
      type={type}
      class={cx(resolveButtonClass(variant, size), className)}
      disabled={disabled || loading}
      aria-busy={loading || undefined}
    >
      {loading ? (
        <span class="inline-flex items-center gap-2">
          <svg
            aria-hidden="true"
            class="size-4 animate-spin"
            viewBox="0 0 24 24"
            fill="none"
            stroke="currentColor"
            strokeWidth="2"
          >
            <path d="M12 2v4" />
            <path d="m16.2 7.8 2.9-2.9" />
            <path d="M18 12h4" />
            <path d="m16.2 16.2 2.9 2.9" />
            <path d="M12 18v4" />
            <path d="m4.9 19.1 2.9-2.9" />
            <path d="M2 12h4" />
            <path d="m4.9 4.9 2.9 2.9" />
          </svg>
          <span>{children}</span>
        </span>
      ) : (
        children
      )}
    </button>
  );
}
