import type {
  SessionStructuredBlock,
  SessionStructuredMessage,
  SessionStructuredToolInput,
  SessionStructuredToolResult,
} from 'gas-city-dashboard-shared';

type Assert<T extends true> = T;
type HasKey<T, K extends PropertyKey> = K extends keyof T ? true : false;
type LacksKey<T, K extends PropertyKey> = HasKey<T, K> extends false ? true : false;

type UserMessage = Extract<SessionStructuredMessage, { role: 'user' }>;
type AssistantMessage = Extract<SessionStructuredMessage, { role: 'assistant' }>;
type TextBlock = Extract<SessionStructuredBlock, { type: 'text' }>;
type ToolResultBlock = Extract<SessionStructuredBlock, { type: 'tool_result' }>;
type CommandInput = Extract<SessionStructuredToolInput, { kind: 'command' }>;
type CodeInput = Extract<SessionStructuredToolInput, { kind: 'code' }>;
type ReadResult = Extract<SessionStructuredToolResult, { kind: 'read' }>;
type BashResult = Extract<SessionStructuredToolResult, { kind: 'bash' }>;

// Compile-time contract guard: these assertions fail as soon as generated
// variants collapse back into one optional-field bag.
export type StructuredTranscriptGeneratedNarrowingAssertions = [
  Assert<HasKey<UserMessage, 'user_prompt'>>,
  Assert<LacksKey<UserMessage, 'usage'>>,
  Assert<HasKey<AssistantMessage, 'usage'>>,
  Assert<LacksKey<AssistantMessage, 'user_prompt'>>,
  Assert<HasKey<TextBlock, 'text'>>,
  Assert<LacksKey<TextBlock, 'structured'>>,
  Assert<HasKey<ToolResultBlock, 'structured'>>,
  Assert<LacksKey<ToolResultBlock, 'input'>>,
  Assert<HasKey<CommandInput, 'command'>>,
  Assert<LacksKey<CommandInput, 'code'>>,
  Assert<HasKey<CodeInput, 'code'>>,
  Assert<LacksKey<CodeInput, 'command'>>,
  Assert<HasKey<ReadResult, 'content'>>,
  Assert<LacksKey<ReadResult, 'stdout'>>,
  Assert<HasKey<BashResult, 'stdout'>>,
  Assert<LacksKey<BashResult, 'patch'>>,
];
