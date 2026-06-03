import { Conn, type WebSocketLike } from "./conn";
import type { Message, SessionUpdate, HistoryItem } from "./types";
import type { Item } from "../views/chat/types";

export interface PromptHandlers {
  onText?: (t: string) => void;
  onThought?: (t: string) => void;
  onToolCall?: (tc: { id: string; title: string; status?: string }) => void;
  onToolUpdate?: (tc: { id: string; status?: string }) => void;
  // return true to allow, false to deny
  requestPermission?: (req: any) => Promise<boolean>;
}

export class Client {
  private conn: Conn;
  private nid = 0;
  private sessionId = "";
  private pending = new Map<number, (m: Message) => void>();
  private handlers: PromptHandlers = {};
  // Replayed transcript from the in-container adapter on (re)connect. Settable by the caller;
  // fires independently of any in-flight prompt (handled directly in route()).
  onHistory?: (items: HistoryItem[]) => void;

  constructor(ws: WebSocketLike) {
    this.conn = new Conn(ws, (m) => this.route(m));
  }

  private next() { return ++this.nid; }

  private call(method: string, params: any): Promise<Message> {
    const id = this.next();
    return new Promise((resolve, reject) => {
      this.pending.set(id, (m) => {
        if (m.error) reject(new Error(`acp ${method}: ${m.error.code} ${m.error.message}`));
        else resolve(m);
      });
      this.conn.send({ id, method, params });
    });
  }

  private route(m: Message) {
    if (m.method === "spawn/history") {
      this.onHistory?.((m.params?.items as HistoryItem[]) ?? []);
      return;
    }
    if (m.method === "session/update") {
      this.dispatchUpdate(m.params as SessionUpdate);
      return;
    }
    if (m.method === "session/request_permission" && m.id != null) {
      this.handlePermission(m);
      return;
    }
    if (m.id != null && this.pending.has(m.id)) {
      const r = this.pending.get(m.id)!;
      this.pending.delete(m.id);
      r(m);
    }
  }

  private dispatchUpdate(p: SessionUpdate) {
    const u = p.update;
    switch (u.sessionUpdate) {
      case "agent_message_chunk":
        if (u.content?.text) this.handlers.onText?.(u.content.text);
        break;
      case "agent_thought_chunk":
        if (u.content?.text) this.handlers.onThought?.(u.content.text);
        break;
      case "tool_call":
        this.handlers.onToolCall?.({ id: u.toolCallId ?? "", title: u.title ?? "tool", status: u.status });
        break;
      case "tool_call_update":
        this.handlers.onToolUpdate?.({ id: u.toolCallId ?? "", status: u.status });
        break;
    }
  }

  private async handlePermission(m: Message) {
    const allow = this.handlers.requestPermission ? await this.handlers.requestPermission(m.params) : true;
    const options: Array<{ optionId: string; kind?: string }> = m.params?.options ?? [];
    // pick an allow-ish option for allow, a reject-ish one for deny; fall back to first option.
    const pick = (want: string[]) =>
      options.find((o) => want.some((w) => (o.kind ?? "").includes(w)))?.optionId ?? options[0]?.optionId ?? "";
    const outcome = allow
      ? { outcome: "selected", optionId: pick(["allow"]) }
      : { outcome: "selected", optionId: pick(["reject", "deny"]) };
    this.conn.send({ id: m.id, result: { outcome } });
  }
  // NOTE: confirm the exact session/request_permission response shape against the ACP spec
  // during the live Goose run; the secret-word demo may not trigger a permission request at all.

  async initialize(): Promise<void> {
    await this.call("initialize", { protocolVersion: 1, clientCapabilities: {} });
  }

  async newSession(cwd: string): Promise<void> {
    const m = await this.call("session/new", { cwd, mcpServers: [] });
    this.sessionId = m.result?.sessionId ?? "";
  }

  async prompt(text: string, handlers: PromptHandlers): Promise<void> {
    this.handlers = handlers;
    await this.call("session/prompt", { sessionId: this.sessionId, prompt: [{ type: "text", text }] });
  }
}

// historyToItems maps replayed adapter history items to chat Items (without ids — the caller assigns
// stable ids). The adapter's "system" marker (e.g. the truncation notice) renders as a plain agent line.
export function historyToItems(items: HistoryItem[]): Omit<Item, "id">[] {
  return items.map((h): Omit<Item, "id"> => {
    switch (h.role) {
      case "user":
        return { kind: "user", text: h.text ?? "" };
      case "thought":
        return { kind: "thought", text: h.text ?? "" };
      case "tool":
        return { kind: "tool", title: h.title ?? "tool", status: h.status };
      case "agent":
      case "system":
      default:
        return { kind: "agent", text: h.text ?? "" };
    }
  });
}
