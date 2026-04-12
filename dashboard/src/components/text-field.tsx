import type { JSX } from 'preact';

import { cx } from '../lib/cx';
import { resolveFieldId } from '../lib/field-id';

export interface TextFieldProps
  extends Omit<JSX.InputHTMLAttributes<HTMLInputElement>, 'children'> {
  label: string;
  description?: string;
  error?: string;
  fieldClassName?: string;
  inputClassName?: string;
}

export function TextField({
  id,
  label,
  description,
  error,
  fieldClassName,
  inputClassName,
  ...props
}: TextFieldProps) {
  const fieldID = resolveFieldId(id);
  const descriptionId = description && fieldID ? `${fieldID}-description` : undefined;
  const errorId = error && fieldID ? `${fieldID}-error` : undefined;
  const describedBy = [descriptionId, errorId].filter(Boolean).join(' ') || undefined;

  return (
    <div class={cx('grid gap-2', fieldClassName)}>
      <label for={fieldID} class="label">
        {label}
      </label>
      <input
        {...props}
        id={fieldID}
        class={cx('input', inputClassName)}
        aria-invalid={error ? 'true' : undefined}
        aria-describedby={describedBy}
      />
      {description ? (
        <p id={descriptionId} class="text-muted-foreground text-sm">
          {description}
        </p>
      ) : null}
      {error ? (
        <p id={errorId} class="text-sm text-red-600 dark:text-red-400">
          {error}
        </p>
      ) : null}
    </div>
  );
}
