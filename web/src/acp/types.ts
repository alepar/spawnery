export interface RpcError { code: number; message: string; data?: unknown }

export interface Message {
  jsonrpc?: string;
  id?: number;
  method?: string;
  params?: any;
  result?: any;
  error?: RpcError;
}

// spawn/history replay item (mirrors the in-container acpadapter's transcript item).
export interface HistoryItem {
  role: "user" | "agent" | "thought" | "tool" | "system";
  text?: string;
  title?: string;
  status?: string;
}

// session/update notification: { sessionId, update: { sessionUpdate, ... } }
export interface SessionUpdate {
  sessionId: string;
  update: {
    sessionUpdate: string; // agent_message_chunk | agent_thought_chunk | tool_call | tool_call_update
    content?: { type: string; text?: string };
    toolCallId?: string;
    title?: string;
    status?: string;
  };
}
