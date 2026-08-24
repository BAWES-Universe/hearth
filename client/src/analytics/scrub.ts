// Client-side PII scrubber — mirrors server/analytics.go scrubPII (defense in
// depth: the client scrubs before send, the server scrubs again before
// persist). Keys/emails/IPs/bearer tokens NEVER leave the browser in raw form.

const KEY_RE = /sk-or-v1-[A-Za-z0-9]{8,}/g;
const EMAIL_RE = /[a-z0-9._%+-]+@[a-z0-9.-]+\.[a-z]{2,}/gi;
const IPV4_RE = /\b\d{1,3}(?:\.\d{1,3}){3}\b/g;
const IPV6_RE = /\b[0-9a-fA-F:]{2,39}:[0-9a-fA-F:]{2,39}\b/g;
const BEARER_RE = /bearer\s+[a-z0-9\-._~+/]+=*/gi;

export function scrubPII(s: string): string {
  return s
    .replace(KEY_RE, '[REDACTED_KEY]')
    .replace(EMAIL_RE, '[REDACTED_EMAIL]')
    .replace(IPV4_RE, '[REDACTED_IP]')
    .replace(IPV6_RE, '[REDACTED_IP]')
    .replace(BEARER_RE, '[REDACTED_TOKEN]');
}

const MAX_KEY_LEN = 64;
const MAX_VALUE_LEN = 512;

/** Returns a scrubbed flat copy, or null if the payload is invalid (nested
 *  objects/arrays or over-length props are rejected — v0 keeps flat payloads). */
export function scrubProps(props: Record<string, unknown>): Record<string, unknown> | null {
  const out: Record<string, unknown> = {};
  for (const [k, v] of Object.entries(props)) {
    const sk = scrubPII(k);
    if (sk.length > MAX_KEY_LEN) return null;
    if (typeof v === 'string') {
      const sv = scrubPII(v);
      if (sv.length > MAX_VALUE_LEN) return null;
      out[sk] = sv;
    } else if (typeof v === 'number' || typeof v === 'boolean' || v === null || v === undefined) {
      out[sk] = v;
    } else {
      return null;
    }
  }
  return out;
}
