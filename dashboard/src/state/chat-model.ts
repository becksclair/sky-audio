import {
  computed,
  createModel,
  signal,
  type ReadonlySignal,
} from '@preact/signals';

import { makeId } from '../lib/ids';
import type { ChatMessage, ChatSubmitContext, ChatSubmitResult } from '../types';

export interface ChatController {
  readonly messages: ReadonlySignal<ChatMessage[]>;
  readonly draft: ReadonlySignal<string>;
  readonly busy: ReadonlySignal<boolean>;
  readonly error: ReadonlySignal<string | null>;
  readonly canSubmit: ReadonlySignal<boolean>;
  setDraft(value: string): void;
  clearError(): void;
  appendMessage(message: Omit<ChatMessage, 'id' | 'createdAt'>): ChatMessage;
  patchMessage(id: string, patch: Partial<Omit<ChatMessage, 'id'>>): void;
  submit(
    run: (draft: string, context: ChatSubmitContext) => Promise<ChatSubmitResult>,
  ): Promise<void>;
  abort(): void;
  reset(): void;
}

export interface ChatModelInit {
  messages?: ChatMessage[];
  draft?: string;
}

export const ChatModel = createModel((initial: ChatModelInit = {}): ChatController => {
  const messages = signal<ChatMessage[]>(initial.messages ?? []);
  const draft = signal(initial.draft ?? '');
  const busy = signal(false);
  const error = signal<string | null>(null);
  const abortController = signal<AbortController | null>(null);

  const canSubmit = computed(() => draft.value.trim().length > 0 && !busy.value);

  function normalizeMessage(
    message: Omit<ChatMessage, 'id' | 'createdAt'>,
  ): ChatMessage {
    return {
      id: makeId('message'),
      createdAt: Date.now(),
      ...message,
    };
  }

  function setDraft(value: string) {
    draft.value = value;
  }

  function clearError() {
    error.value = null;
  }

  function appendMessage(
    message: Omit<ChatMessage, 'id' | 'createdAt'>,
  ): ChatMessage {
    const normalized = normalizeMessage(message);
    messages.value = [...messages.value, normalized];
    return normalized;
  }

  function patchMessage(
    id: string,
    patch: Partial<Omit<ChatMessage, 'id'>>,
  ) {
    messages.value = messages.value.map((message) =>
      message.id === id ? { ...message, ...patch } : message,
    );
  }

  async function submit(
    run: (draft: string, context: ChatSubmitContext) => Promise<ChatSubmitResult>,
  ) {
    const nextDraft = draft.value.trim();

    if (!nextDraft || busy.value) {
      return;
    }

    busy.value = true;
    error.value = null;

    const controller = new AbortController();
    abortController.value = controller;

    appendMessage({
      role: 'user',
      content: nextDraft,
    });

    draft.value = '';

    try {
      const result = await run(nextDraft, {
        messages: messages.value,
        signal: controller.signal,
      });

      if (!result) {
        return;
      }

      const items = Array.isArray(result) ? result : [result];
      messages.value = [
        ...messages.value,
        ...items.map((item) => normalizeMessage(item)),
      ];
    } catch (caught) {
      if (controller.signal.aborted) {
        return;
      }

      const message =
        caught instanceof Error ? caught.message : 'Something went wrong.';
      error.value = message;
    } finally {
      busy.value = false;
      abortController.value = null;
    }
  }

  function abort() {
    abortController.value?.abort();
    busy.value = false;
    abortController.value = null;
  }

  function reset() {
    messages.value = [];
    draft.value = '';
    busy.value = false;
    error.value = null;
    abortController.value?.abort();
    abortController.value = null;
  }

  return {
    messages,
    draft,
    busy,
    error,
    canSubmit,
    setDraft,
    clearError,
    appendMessage,
    patchMessage,
    submit,
    abort,
    reset,
  };
});
