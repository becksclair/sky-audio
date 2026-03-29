type SignalLike<T> = { value: T };

type ClassValue = string | false | null | undefined | SignalLike<string | false | null | undefined>;

function unwrap<T>(value: T | SignalLike<T>): T {
  return value && typeof value === 'object' && 'value' in value ? value.value : value;
}

export function cx(...parts: Array<ClassValue>): string {
  return parts.map(unwrap).filter(Boolean).join(' ');
}
