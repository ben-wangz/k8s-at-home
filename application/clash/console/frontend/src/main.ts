import './style.css';
import { Controller, ControllerError } from './controller';
import { element, errorMessage } from './dom';
import { ConnectionView, LogView, showTraffic } from './observability';
import { ProxyView } from './proxies';
import { ControllerStream } from './stream';
import type { Connections, LogEvent, StreamState, Traffic } from './types';

let controller: Controller | undefined;
let trafficStream: ControllerStream<Traffic> | undefined;
let viewStream: { close(): void } | undefined;
let view = '';
let generation = 0;
const streamStates = new Map<string, StreamState>();
const connections = new ConnectionView();
const logs = new LogView();
const proxies = new ProxyView(authenticationFailed);

function message(value = ''): void {
  element('controller-error').textContent = value;
  element('controller-error').hidden = !value;
}

function status(value: string): void {
  element('controller-status').textContent = value;
  element('controller-status').dataset.state = value;
}

function streamStatus(name: string, state: StreamState, detail?: string): void {
  streamStates.set(name, state);
  status([...streamStates.values()].some(value => value !== 'Connected') ? 'Reconnecting' : 'Connected');
  if (name === 'traffic') element('traffic-status').textContent = `${state === 'Connected' ? 'Live traffic' : 'Traffic unavailable or stale'}${detail ? ` · ${detail}` : ''}`;
}

function stop(preserve = false): void {
  generation++;
  trafficStream?.close();
  viewStream?.close();
  trafficStream = undefined;
  viewStream = undefined;
  streamStates.clear();
  controller?.close();
  controller = undefined;
  proxies.setController(undefined, preserve);
  connections.status('Disconnected');
  logs.status('Disconnected');
  element('traffic-status').textContent = 'Traffic unavailable or stale';
  status('Disconnected');
}

function authenticationFailed(): void {
  stop(true);
  status('Authentication failed');
  message('Authentication failed. Enter a valid controller secret and reconnect.');
  element<HTMLDetailsElement>('controller-settings').open = true;
}

function startView(): void {
  viewStream?.close();
  viewStream = undefined;
  streamStates.delete('view');
  const client = controller;
  if (!client) return;
  const current = generation;
  const update = (state: StreamState, detail?: string) => {
    if (current !== generation) return;
    streamStatus('view', state, detail);
    if (view === 'connections') connections.status(state, detail);
    if (view === 'logs') logs.status(state, detail);
  };
  if (view === 'connections') {
    viewStream = new ControllerStream<Connections>(client, '/connections?interval=1000', data => {
      if (current === generation) connections.receive(data);
    }, update, authenticationFailed);
  } else if (view === 'logs') {
    const level = element<HTMLSelectElement>('log-level').value;
    viewStream = new ControllerStream<LogEvent>(client, `/logs?level=${level}&format=structured`, data => {
      if (current === generation) logs.receive(data);
    }, update, authenticationFailed);
  } else {
    streamStatus('traffic', streamStates.get('traffic') ?? 'Connecting');
  }
}

async function connect(): Promise<void> {
  let next: Controller;
  try { next = new Controller(element<HTMLInputElement>('controller-url').value, element<HTMLInputElement>('controller-secret').value); }
  catch (error) { message(errorMessage(error)); return; }
  stop();
  const current = generation;
  controller = next;
  connections.reset();
  logs.reset();
  showTraffic({});
  message();
  status('Connecting');
  proxies.setController(next);
  try {
    const version = await next.request<{ version: string }>('/version');
    if (current !== generation) return;
    if (typeof version.version !== 'string') throw new Error('This endpoint does not provide the Mihomo controller API.');
    element('core-version').textContent = `Mihomo ${version.version}`;
    await proxies.refresh();
    if (current !== generation) return;
    element<HTMLDetailsElement>('controller-settings').open = false;
    status('Connected');
    trafficStream = new ControllerStream<Traffic>(next, '/traffic', data => {
      if (current === generation) showTraffic(data);
    }, (state, detail) => { if (current === generation) streamStatus('traffic', state, detail); }, authenticationFailed);
    startView();
  } catch (error) {
    if (current !== generation) return;
    if (error instanceof ControllerError && error.status === 401) authenticationFailed();
    else { stop(true); message(errorMessage(error)); }
  }
}

function navigate(): void {
  const requested = location.hash.slice(1);
  const next = ['proxies', 'connections', 'logs'].includes(requested) ? requested : 'proxies';
  if (next === view) return;
  view = next;
  for (const name of ['proxies', 'connections', 'logs']) element(`${name}-view`).hidden = name !== view;
  for (const link of document.querySelectorAll<HTMLAnchorElement>('nav a')) link.setAttribute('aria-current', link.hash === `#${view}` ? 'page' : 'false');
  startView();
}

element<HTMLInputElement>('controller-url').value = `${location.origin}/`;
element('controller-form').addEventListener('submit', event => { event.preventDefault(); void connect(); });
element('disconnect').addEventListener('click', () => { stop(true); message(); element<HTMLInputElement>('controller-secret').value = ''; });
element('log-level').addEventListener('change', startView);
window.addEventListener('hashchange', navigate);
window.addEventListener('pagehide', () => stop(true));
navigate();
void connect();
