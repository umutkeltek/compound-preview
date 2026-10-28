#!/usr/bin/env bun
import { existsSync, readFileSync, statSync, writeFileSync } from "node:fs";
import { join } from "node:path";

type KnowledgeExport = {
  schema_version: "compound.knowledge_export.v0.1";
  source: { kind: "chatgpt"; ref: string; captured_at: string };
  privacy: "private";
  producer: { kind: "edge-script"; name: "chatgpt-conversation-edge"; version: "0.1" };
  items: KnowledgeItem[];
};

type KnowledgeItem = {
  ref: string;
  title?: string;
  published_at?: string;
  topics?: string[];
  content: string;
  lang: "en";
};

type EdgeOptions = {
  sourceRef: string;
  capturedAt: string;
  limit: number;
  minChars: number;
  maxContentChars: number;
  topics: string[];
};

type NormalizedOptions = Pick<EdgeOptions, "limit" | "minChars" | "maxContentChars">;

type NormalizedConversation = {
  ref: string;
  title: string;
  publishedAt: string;
  content: string;
  contentChars: number;
  sortTime: number;
};

type EdgeReport = {
  export: KnowledgeExport;
  selected: Array<{
    ref: string;
    title: string;
    published_at: string;
    content_chars: number;
  }>;
};

type ChatGPTConversation = {
  id?: unknown;
  conversation_id?: unknown;
  uuid?: unknown;
  title?: unknown;
  create_time?: unknown;
  update_time?: unknown;
  mapping?: Record<string, ChatGPTNode>;
  messages?: ChatGPTMessage[];
};

type ChatGPTNode = {
  message?: ChatGPTMessage | null;
};

type ChatGPTMessage = {
  role?: unknown;
  author?: { role?: unknown };
  create_time?: unknown;
  timestamp?: unknown;
  content?: {
    content_type?: unknown;
    parts?: unknown;
    text?: unknown;
  } | string;
};

function normalizeConversations(input: unknown, opts: NormalizedOptions): NormalizedConversation[] {
  const conversations = conversationList(input);
  const out: NormalizedConversation[] = [];
  conversations.forEach((conversation, index) => {
    const messages = extractMessages(conversation);
    const content = messages
      .map((message) => messageLine(message))
      .filter((line) => line.trim() !== "")
      .join("\n\n")
      .trim();
    if (content.length < opts.minChars) return;
    const ref = "chatgpt:" + conversationID(conversation, index);
    const title = stringValue(conversation.title) || ref;
    const sortTime = timestampMillis(conversation.update_time) || timestampMillis(conversation.create_time) || 0;
    const publishedAt = sortTime > 0 ? new Date(sortTime).toISOString() : "";
    const bounded = truncate(content, opts.maxContentChars);
    out.push({ ref, title, publishedAt, content: bounded, contentChars: bounded.length, sortTime });
  });
  out.sort((a, b) => {
    if (a.sortTime !== b.sortTime) return b.sortTime - a.sortTime;
    return a.ref.localeCompare(b.ref);
  });
  return out.slice(0, opts.limit);
}

function buildChatGPTKnowledgeExport(input: unknown, opts: Partial<EdgeOptions> = {}): EdgeReport {
  const full: EdgeOptions = {
    sourceRef: opts.sourceRef || "conversation-export:chatgpt",
    capturedAt: opts.capturedAt || new Date().toISOString(),
    limit: opts.limit ?? 5,
    minChars: opts.minChars ?? 500,
    maxContentChars: opts.maxContentChars ?? 24000,
    topics: opts.topics || [],
  };
  const conversations = normalizeConversations(input, full);
  const topics = unique(["chatgpt", "conversation", ...full.topics.map(slugish).filter(Boolean)]);
  const exportDoc: KnowledgeExport = {
    schema_version: "compound.knowledge_export.v0.1",
    source: { kind: "chatgpt", ref: full.sourceRef, captured_at: full.capturedAt },
    privacy: "private",
    producer: { kind: "edge-script", name: "chatgpt-conversation-edge", version: "0.1" },
    items: conversations.map((conversation) => ({
      ref: conversation.ref,
      title: conversation.title,
      published_at: conversation.publishedAt || undefined,
      topics,
      content: conversation.content,
      lang: "en",
    })),
  };
  return {
    export: exportDoc,
    selected: conversations.map((conversation) => ({
      ref: conversation.ref,
      title: conversation.title,
      published_at: conversation.publishedAt,
      content_chars: conversation.contentChars,
    })),
  };
}

function conversationList(input: unknown): ChatGPTConversation[] {
  if (Array.isArray(input)) return input.filter(isObject) as ChatGPTConversation[];
  if (!isObject(input)) return [];
  const maybe = input as Record<string, unknown>;
  for (const key of ["conversations", "items", "data"]) {
    const value = maybe[key];
    if (Array.isArray(value)) return value.filter(isObject) as ChatGPTConversation[];
  }
  return [];
}

function loadConversationInput(path: string): unknown {
  const resolved = resolveInputPath(path);
  if (resolved.endsWith(".jsonl")) {
    return readFileSync(resolved, "utf8")
      .split(/\r?\n/)
      .map((line) => line.trim())
      .filter(Boolean)
      .map((line, index) => {
        try {
          return JSON.parse(line);
        } catch (err) {
          throw new Error(`parse ${resolved}:${index + 1}: ${err instanceof Error ? err.message : String(err)}`);
        }
      });
  }
  return JSON.parse(readFileSync(resolved, "utf8"));
}

function resolveInputPath(path: string): string {
  const stat = statSync(path);
  if (!stat.isDirectory()) return path;
  const candidates = [
    join(path, "clean_output", "conversations.jsonl"),
    join(path, "conversations.jsonl"),
    join(path, "conversations.json"),
  ];
  for (const candidate of candidates) {
    if (existsSync(candidate)) return candidate;
  }
  throw new Error(`no conversations.jsonl or conversations.json found under ${path}`);
}

function extractMessages(conversation: ChatGPTConversation): ChatGPTMessage[] {
  if (Array.isArray(conversation.messages)) return conversation.messages;
  if (!conversation.mapping) return [];
  return Object.entries(conversation.mapping)
    .map(([key, node]) => ({ key, message: node?.message }))
    .filter((row): row is { key: string; message: ChatGPTMessage } => !!row.message)
    .sort((a, b) => {
      const at = timestampMillis(a.message.create_time);
      const bt = timestampMillis(b.message.create_time);
      const ats = at || timestampMillis(a.message.timestamp);
      const bts = bt || timestampMillis(b.message.timestamp);
      if (ats !== bts) return ats - bts;
      return a.key.localeCompare(b.key);
    })
    .map((row) => row.message);
}

function messageLine(message: ChatGPTMessage): string {
  const role = stringValue(message.author?.role) || stringValue(message.role) || "unknown";
  const text = messageText(message);
  if (text.trim() === "") return "";
  return `[${role}] ${text.trim()}`;
}

function messageText(message: ChatGPTMessage): string {
  if (typeof message.content === "string") return message.content.trim();
  const parts = message.content?.parts;
  if (Array.isArray(parts)) {
    return parts.map(partText).filter(Boolean).join("\n").trim();
  }
  return stringValue(message.content?.text);
}

function partText(part: unknown): string {
  if (typeof part === "string") return part;
  if (isObject(part)) {
    const value = part as Record<string, unknown>;
    return stringValue(value.text) || stringValue(value.content);
  }
  return "";
}

function conversationID(conversation: ChatGPTConversation, index: number): string {
  const raw =
    stringValue(conversation.id) ||
    stringValue(conversation.conversation_id) ||
    stringValue(conversation.uuid) ||
    `${stringValue(conversation.title) || "conversation"}-${index + 1}`;
  return slugish(raw) || `conversation-${index + 1}`;
}

function timestampMillis(value: unknown): number {
  if (typeof value === "number" && Number.isFinite(value)) {
    return value < 1_000_000_000_000 ? Math.round(value * 1000) : Math.round(value);
  }
  const text = stringValue(value);
  if (text === "") return 0;
  const parsed = Date.parse(text);
  return Number.isFinite(parsed) ? parsed : 0;
}

function truncate(value: string, maxChars: number): string {
  if (maxChars <= 0 || value.length <= maxChars) return value;
  const suffix = "\n[truncated]";
  if (maxChars <= suffix.length) return suffix.slice(0, maxChars);
  return value.slice(0, maxChars - suffix.length).trimEnd() + suffix;
}

function slugish(value: string): string {
  return value
    .trim()
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "");
}

function unique(values: string[]): string[] {
  return Array.from(new Set(values.filter((value) => value.trim() !== "")));
}

function stringValue(value: unknown): string {
  return typeof value === "string" ? value.trim() : "";
}

function isObject(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null;
}

function parseArgs(argv: string[]): Record<string, string | boolean> {
  const out: Record<string, string | boolean> = {};
  for (let i = 0; i < argv.length; i++) {
    const arg = argv[i];
    if (!arg.startsWith("--")) {
      if (!out.input) out.input = arg;
      else usage();
      continue;
    }
    const key = arg.slice(2);
    const next = argv[i + 1];
    if (!next || next.startsWith("--")) out[key] = true;
    else {
      out[key] = next;
      i++;
    }
  }
  return out;
}

function usage(): never {
  console.error(
    "usage: bun tools/chatgpt_conversation_edge.ts <conversations.json> --out export.json [--limit 5] [--min-chars 500] [--max-content-chars 24000] [--source-ref REF] [--captured-at RFC3339] [--topic T]",
  );
  process.exit(2);
}

function numberFlag(value: string | boolean | undefined, fallback: number): number {
  if (typeof value !== "string") return fallback;
  const n = Number(value);
  return Number.isFinite(n) ? n : fallback;
}

async function main() {
  const args = parseArgs(process.argv.slice(2));
  const inputPath = typeof args.input === "string" ? args.input : "";
  if (!inputPath) usage();
  const input = loadConversationInput(inputPath);
  const topics = ([] as string[]).concat(
    ...Object.entries(args)
      .filter(([key]) => key === "topic")
      .map(([, value]) => (typeof value === "string" ? [value] : [])),
  );
  const report = buildChatGPTKnowledgeExport(input, {
    sourceRef: typeof args["source-ref"] === "string" ? args["source-ref"] : "conversation-export:chatgpt",
    capturedAt: typeof args["captured-at"] === "string" ? args["captured-at"] : new Date().toISOString(),
    limit: numberFlag(args.limit, 5),
    minChars: numberFlag(args["min-chars"], 500),
    maxContentChars: numberFlag(args["max-content-chars"], 24000),
    topics,
  });
  const output = JSON.stringify(report.export, null, 2) + "\n";
  if (typeof args.out === "string") writeFileSync(args.out, output);
  else process.stdout.write(output);
  console.error(
    `chatgpt-conversation-edge selected=${report.selected.length} source=${report.export.source.ref}`,
  );
}

if (import.meta.main) {
  main().catch((err) => {
    console.error(err instanceof Error ? err.message : String(err));
    process.exit(1);
  });
}

export { buildChatGPTKnowledgeExport, normalizeConversations };
