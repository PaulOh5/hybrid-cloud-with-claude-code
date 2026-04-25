// Fetch wrapper that always sends + receives the session cookie. Same-origin
// in production via the Next.js rewrite (/api/v1/* → main-api); in
// development the dev server proxies the same path.

export class ApiError extends Error {
  status: number;
  code: string;

  constructor(status: number, code: string, message: string) {
    super(message);
    this.status = status;
    this.code = code;
    this.name = "ApiError";
  }
}

type ErrorBody = { error?: { code?: string; message?: string } };

async function parseError(res: Response): Promise<ApiError> {
  let code = "request_failed";
  let message = res.statusText;
  try {
    const body = (await res.json()) as ErrorBody;
    if (body?.error?.code) code = body.error.code;
    if (body?.error?.message) message = body.error.message;
  } catch {
    // body may be empty or non-JSON; fall through to defaults.
  }
  return new ApiError(res.status, code, message);
}

export type RequestOptions = {
  method?: string;
  body?: unknown;
  signal?: AbortSignal;
};

// apiFetch returns the parsed JSON body (typed by the caller). 204 responses
// resolve to null.
export async function apiFetch<T>(path: string, opts: RequestOptions = {}): Promise<T> {
  const headers: Record<string, string> = { Accept: "application/json" };
  let body: BodyInit | undefined;
  if (opts.body !== undefined) {
    headers["Content-Type"] = "application/json";
    body = JSON.stringify(opts.body);
  }
  const res = await fetch(path, {
    method: opts.method ?? "GET",
    credentials: "include",
    headers,
    body,
    signal: opts.signal,
  });
  if (!res.ok) {
    throw await parseError(res);
  }
  if (res.status === 204) return null as T;
  return (await res.json()) as T;
}
