import { Controller, ControllerError } from './controller';
import type { StreamState } from './types';

export class ControllerStream<T> {
  private socket?: WebSocket;
  private timer?: ReturnType<typeof setTimeout>;
  private stopped = false;
  private attempt = 0;
  private watchdog?: ReturnType<typeof setTimeout>;

  constructor(
    private controller: Controller,
    private path: string,
    private receive: (data: T) => void,
    private state: (value: StreamState, detail?: string) => void,
    private unauthorized: () => void,
  ) { this.open(); }

  private open(): void {
    if (this.stopped) return;
    this.state(this.attempt ? 'Reconnecting' : 'Connecting');
    const socket = new WebSocket(this.controller.streamURL(this.path));
    this.socket = socket;
    this.watchdog = setTimeout(() => socket.close(), 12000);
    socket.onopen = () => {
      clearTimeout(this.watchdog);
      if (!this.stopped) this.state('Connected');
    };
    socket.onmessage = (event) => {
      if (this.stopped) return;
      try {
        const value = JSON.parse(event.data) as T;
        this.receive(value);
        this.attempt = 0;
        this.state('Connected');
      } catch { this.state('Reconnecting', 'Invalid stream event; waiting for fresh data.'); }
    };
    socket.onerror = () => { if (!this.stopped) this.state('Reconnecting', 'Stream interrupted; last data may be stale.'); };
    socket.onclose = () => {
      clearTimeout(this.watchdog);
      if (!this.stopped) void this.retry();
    };
  }

  private async retry(): Promise<void> {
    this.state('Reconnecting', 'Stream interrupted; last data may be stale.');
    try { await this.controller.request('/version'); }
    catch (error) {
      if (this.stopped) return;
      if (error instanceof ControllerError && error.status === 401) {
        this.close();
        this.unauthorized();
        return;
      }
    }
    if (this.stopped) return;
    const delay = Math.min(1000 * 2 ** Math.min(this.attempt++, 5), 30000);
    this.timer = setTimeout(() => this.open(), delay);
  }

  close(): void {
    this.stopped = true;
    clearTimeout(this.timer);
    clearTimeout(this.watchdog);
    this.socket?.close();
  }
}
