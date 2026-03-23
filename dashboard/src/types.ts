export type MessageRole = 'user' | 'assistant' | 'system' | 'tool';

export type ToolResultStatus = 'ok' | 'working' | 'error';

export interface ToolResult {
  title: string;
  status?: ToolResultStatus;
  summary?: string;
  payload?: unknown;
}

export interface ChatMessage {
  id: string;
  role: MessageRole;
  content: string;
  createdAt: number;
  toolResult?: ToolResult;
}

export interface ChatSubmitContext {
  messages: readonly ChatMessage[];
  signal: AbortSignal;
}

export type ChatSubmitResult =
  | void
  | Omit<ChatMessage, 'id' | 'createdAt'>
  | Array<Omit<ChatMessage, 'id' | 'createdAt'>>;
