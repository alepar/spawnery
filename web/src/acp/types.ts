export interface RpcError { code: number; message: string; data?: unknown }

export interface Message {
  jsonrpc?: string;
  id?: number;
  method?: string;
  params?: any;
  result?: any;
  error?: RpcError;
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
