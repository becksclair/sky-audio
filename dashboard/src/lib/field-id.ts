export function resolveFieldId(id: unknown): string | undefined {
  if (typeof id === 'string') {
    return id;
  }
  if (id && typeof id === 'object') {
    const maybeSignal = id as { value?: unknown };
    if (typeof maybeSignal.value === 'string') {
      return maybeSignal.value;
    }
  }
  return undefined;
}
