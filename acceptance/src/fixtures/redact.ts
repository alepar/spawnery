/**
 * Artifact redaction: trace/HAR/video and any persisted auth state are scrubbed of bearer
 * tokens, cookies, and keys before upload (design §Isolation, artifact redaction).
 */

const REDACTED = "<redacted>";

// Applied in order: longer/more-specific patterns first so a later pass doesn't re-match inside
// an already-redacted span (unlikely here, but keep the intent explicit).
export function redactString(s: string): string {
  let out = s;
  out = out.replace(/\bBearer\s+[A-Za-z0-9._~+/-]+=*/gi, `Bearer ${REDACTED}`);
  out = out.replace(/\b(access_token|refresh_token)=[A-Za-z0-9._~+/-]+=*/gi, `$1=${REDACTED}`);
  out = out.replace(/\b([A-Za-z0-9_-]*(?:token|session|rth|sid)[A-Za-z0-9_-]*)=[^;\s"']+/gi, (_m, name: string) => `${name}=${REDACTED}`);
  out = out.replace(/\b[A-Za-z0-9_-]{32,}={0,2}\b/g, REDACTED);
  return out;
}

interface HarHeader {
  name: string;
  value: string;
}
interface HarQueryParam {
  name: string;
  value: string;
}
interface HarPostData {
  text?: string;
  [k: string]: unknown;
}
interface HarMessage {
  headers?: HarHeader[];
  queryString?: HarQueryParam[];
  postData?: HarPostData;
  cookies?: unknown[];
  [k: string]: unknown;
}
interface HarEntry {
  request?: HarMessage;
  response?: HarMessage;
  [k: string]: unknown;
}
export interface Har {
  log?: {
    entries?: HarEntry[];
    [k: string]: unknown;
  };
  [k: string]: unknown;
}

function redactHeaders(headers: HarHeader[] | undefined): HarHeader[] | undefined {
  if (!headers) return headers;
  return headers
    .filter((h) => h.name.toLowerCase() !== "set-cookie")
    .map((h) => ({ ...h, value: redactString(h.value) }));
}

function redactMessage(msg: HarMessage | undefined): HarMessage | undefined {
  if (!msg) return msg;
  const out: HarMessage = { ...msg };
  out.headers = redactHeaders(msg.headers);
  if (msg.queryString) out.queryString = msg.queryString.map((q) => ({ ...q, value: redactString(q.value) }));
  if (msg.postData?.text) out.postData = { ...msg.postData, text: redactString(msg.postData.text) };
  delete out.cookies; // drop cookie jars entirely rather than try to scrub each value
  return out;
}

/** redactHar scrubs bearer/cookie/key material from a HAR object's request/response entries. */
export function redactHar(har: Har): Har {
  if (!har.log?.entries) return har;
  return {
    ...har,
    log: {
      ...har.log,
      entries: har.log.entries.map((entry) => ({
        ...entry,
        request: redactMessage(entry.request),
        response: redactMessage(entry.response),
      })),
    },
  };
}
