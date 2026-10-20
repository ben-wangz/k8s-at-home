export class ControllerError extends Error {
  constructor(message: string, readonly status = 0) { super(message); }
}

export function controllerURL(value: string): URL {
  const url = new URL(value);
  if (!['http:', 'https:'].includes(url.protocol) || url.username || url.password || url.search || url.hash) {
    throw new ControllerError('Use an HTTP or HTTPS controller URL without credentials, query parameters, or a fragment.');
  }
  if (location.protocol === 'https:' && url.protocol !== 'https:') {
    throw new ControllerError('This HTTPS Console requires an HTTPS controller endpoint.');
  }
  url.pathname = `${url.pathname.replace(/\/+$/, '')}/`;
  return url;
}

export class Controller {
  readonly url: URL;
  private lifetime = new AbortController();

  constructor(endpoint: string, private secret: string) { this.url = controllerURL(endpoint); }

  async request<T>(path: string, method = 'GET', body?: unknown): Promise<T> {
    const signal = AbortSignal.any([this.lifetime.signal, AbortSignal.timeout(12000)]);
    const headers: Record<string, string> = {};
    if (this.secret) headers.Authorization = `Bearer ${this.secret}`;
    if (body !== undefined) headers['Content-Type'] = 'application/json';
    let response: Response;
    try {
      response = await fetch(new URL(path.replace(/^\//, ''), this.url), {
        method, headers, body: body === undefined ? undefined : JSON.stringify(body), signal,
        cache: 'no-store', credentials: 'omit', referrerPolicy: 'no-referrer',
      });
    } catch {
      throw new ControllerError(this.lifetime.signal.aborted ? 'Controller disconnected.' : 'Controller unreachable or request timed out. Check the URL, network, and allowed origins.');
    }
    if (!response.ok) {
      const message = response.status === 401 ? 'Authentication failed. Check the controller secret.'
        : response.status === 404 ? 'The requested controller API is unavailable (404).'
        : `The controller rejected the request (HTTP ${response.status}).`;
      throw new ControllerError(message, response.status);
    }
    if (response.status === 204) return undefined as T;
    try { return await response.json() as T; }
    catch { throw new ControllerError('The controller returned an invalid JSON response.'); }
  }

  streamURL(path: string): string {
    const url = new URL(path.replace(/^\//, ''), this.url);
    url.protocol = url.protocol === 'https:' ? 'wss:' : 'ws:';
    if (this.secret) url.searchParams.set('token', this.secret);
    return url.href;
  }

  close(): void { this.lifetime.abort(); this.secret = ''; }
}
