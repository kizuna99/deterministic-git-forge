// The request context every surface handler shares, built once by app.ts.

export const JSON_PARSE_ERROR: unique symbol = Symbol('json-parse-error');

export interface Ctx {
  method: string;
  /** URL pathname, still percent-encoded per segment. */
  path: string;
  /** Pathname split into decoded segments (empty segments dropped). */
  segments: string[];
  query: URLSearchParams;
  /** Raw request body. */
  body: Buffer;
  /** Parsed JSON body when the request carried one; JSON_PARSE_ERROR when malformed. */
  json: unknown;
  /** Absolute origin of this server as the client addressed it, for Link headers. */
  origin: string;
  send: (status: number, contentType: string, body: Buffer | string, headers?: Record<string, string>) => void;
  sendJson: (status: number, value: unknown, headers?: Record<string, string>) => void;
}

export function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

export function strField(obj: Record<string, unknown>, key: string): string | undefined {
  const value = obj[key];
  return typeof value === 'string' ? value : undefined;
}

export function boolField(obj: Record<string, unknown>, key: string): boolean | undefined {
  const value = obj[key];
  return typeof value === 'boolean' ? value : undefined;
}
