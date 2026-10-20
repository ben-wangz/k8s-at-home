import { bytes, element, node, time } from './dom';
import type { Connection, Connections, LogEvent, StreamState, Traffic } from './types';

export function showTraffic(value: Traffic): void {
  element('upload-rate').textContent = value.up === undefined ? 'Unavailable' : `${bytes(value.up)}/s`;
  element('download-rate').textContent = value.down === undefined ? 'Unavailable' : `${bytes(value.down)}/s`;
  element('upload-total').textContent = bytes(value.upTotal);
  element('download-total').textContent = bytes(value.downTotal);
}

export class ConnectionView {
  private values: Connection[] = [];
  private updated?: string;
  constructor() { element('connection-filter').addEventListener('input', () => this.render()); }

  receive(value: Connections): void {
    if (value.connections !== undefined && value.connections !== null && !Array.isArray(value.connections)) throw new Error('Invalid connection snapshot');
    this.values = value.connections ?? [];
    this.updated = new Date().toLocaleTimeString();
    element('connection-count').textContent = `${this.values.length} active`;
    this.status('Connected');
    this.render();
  }

  status(state: StreamState, detail?: string): void {
    element('connection-status').textContent = `${state}${this.updated ? ` · Last update: ${this.updated}` : ' · No snapshot received'}${state !== 'Connected' ? ' · Data unavailable or stale' : ''}${detail ? ` · ${detail}` : ''}`;
  }

  reset(): void {
    this.values = [];
    this.updated = undefined;
    element('connection-count').textContent = 'Unavailable';
    this.status('Disconnected');
    this.render();
  }

  private render(): void {
    const filter = element<HTMLInputElement>('connection-filter').value.toLowerCase();
    const rows: HTMLElement[] = [];
    for (const connection of this.values) {
      if (filter && !JSON.stringify(connection).toLowerCase().includes(filter)) continue;
      const meta = connection.metadata ?? {};
      const destination = `${meta.host || meta.destinationIP || 'Unavailable'}${meta.destinationPort ? `:${meta.destinationPort}` : ''}`;
      const source = `${meta.sourceIP || 'Unavailable'}${meta.sourcePort ? `:${meta.sourcePort}` : ''}`;
      const row = node('tr');
      row.title = meta.processPath || meta.process || '';
      const values = [destination, source, [meta.network, meta.type].filter(Boolean).join(' / ') || 'Unavailable', connection.chains?.join(' → ') || 'Unavailable', [connection.rule, connection.rulePayload].filter(Boolean).join(' · ') || 'Unavailable', time(connection.start), bytes(connection.upload), bytes(connection.download)];
      row.append(...values.map(value => node('td', value)));
      rows.push(row);
    }
    if (!rows.length) {
      const cell = node('td', this.updated ? 'No matching active connections.' : 'Waiting for a controller snapshot.', 'muted');
      cell.colSpan = 8;
      const row = node('tr');
      row.append(cell);
      rows.push(row);
    }
    element('connection-rows').replaceChildren(...rows);
  }
}

export class LogView {
  readonly limit = 1000;
  private events: LogEvent[] = [];
  private paused = false;
  private scheduled = false;

  constructor() {
    element('log-filter').addEventListener('input', () => { if (!this.paused) this.render(); });
    element('pause-logs').addEventListener('click', () => {
      this.paused = !this.paused;
      const button = element('pause-logs');
      button.textContent = this.paused ? 'Resume display' : 'Pause display';
      button.setAttribute('aria-pressed', String(this.paused));
      if (!this.paused) this.render();
    });
    element('clear-logs').addEventListener('click', () => { this.events = []; this.render(); });
  }

  receive(event: LogEvent): void {
    if (typeof event !== 'object' || event === null) throw new Error('Invalid log event');
    this.events.push(event);
    if (this.events.length > this.limit) this.events.splice(0, this.events.length - this.limit);
    if (this.scheduled || this.paused) return;
    this.scheduled = true;
    requestAnimationFrame(() => { this.scheduled = false; if (!this.paused) this.render(); });
  }

  status(state: StreamState, detail?: string): void {
    element('log-status').textContent = `${state}${detail ? ` · ${detail}` : ''}`;
  }

  reset(): void { this.events = []; this.render(); this.status('Disconnected'); }

  private render(): void {
    const filter = element<HTMLInputElement>('log-filter').value.toLowerCase();
    const entries: HTMLElement[] = [];
    for (const event of this.events) {
      const message = event.message ?? event.payload ?? '';
      if (filter && !message.toLowerCase().includes(filter)) continue;
      const level = event.level ?? event.type ?? 'info';
      const entry = node('div', '', 'log-entry');
      const badge = node('span', level, 'log-level');
      badge.dataset.level = level;
      entry.append(node('time', event.time ?? '—', 'muted'), badge, node('span', message));
      entries.push(entry);
    }
    element('log-entries').replaceChildren(...entries);
    element('log-count').textContent = `${this.events.length} / ${this.limit} events`;
    if (element<HTMLInputElement>('log-scroll').checked) {
      const container = element('log-entries');
      container.scrollTop = container.scrollHeight;
    }
  }
}
