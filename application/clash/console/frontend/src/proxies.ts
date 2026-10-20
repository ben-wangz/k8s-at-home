import { Controller, ControllerError } from './controller';
import { element, errorMessage, node } from './dom';
import type { Proxy } from './types';

export class ProxyView {
  private controller?: Controller;
  private proxies: Record<string, Proxy> = {};
  private pending = new Set<string>();
  private message = 'Connect to a controller to load proxies.';

  constructor(private authenticationFailed: () => void) {
    element('proxy-filter').addEventListener('input', () => this.render());
    element('refresh-proxies').addEventListener('click', () => { void this.refresh().catch(() => {}); });
  }

  setController(controller?: Controller, preserve = false): void {
    this.controller = controller;
    this.pending.clear();
    if (!preserve) this.proxies = {};
    this.message = controller ? 'Loading proxies…' : preserve ? 'Disconnected; proxy data is stale.' : 'Connect to a controller to load proxies.';
    this.render();
  }

  async refresh(): Promise<void> {
    const controller = this.controller;
    if (!controller) return;
    try {
      const result = await controller.request<{ proxies: Record<string, Proxy> }>('/proxies');
      if (controller !== this.controller) return;
      if (!result.proxies || typeof result.proxies !== 'object') throw new Error('The controller returned an invalid proxy inventory.');
      this.proxies = result.proxies;
      this.message = Object.keys(this.proxies).length ? `Updated ${new Date().toLocaleTimeString()}` : 'The controller returned no proxies.';
      this.render();
    } catch (error) {
      if (controller !== this.controller) return;
      this.message = `${errorMessage(error)} Previous data may be stale.`;
      this.render();
      if (error instanceof ControllerError && error.status === 401) this.authenticationFailed();
      throw error;
    }
  }

  private async select(group: Proxy, member: string): Promise<void> {
    const controller = this.controller;
    if (!controller || this.pending.has(group.name)) return;
    this.pending.add(group.name);
    this.message = `Changing ${group.name} to ${member}…`;
    this.render();
    try {
      await controller.request(`/proxies/${encodeURIComponent(group.name)}`, 'PUT', { name: member });
      const confirmed = await controller.request<Proxy>(`/proxies/${encodeURIComponent(group.name)}`);
      if (controller !== this.controller) return;
      this.proxies[group.name] = confirmed;
      this.message = confirmed.now === member ? `${group.name} now selects ${member}.` : `The controller currently selects ${confirmed.now ?? 'an unavailable member'} for ${group.name}.`;
    } catch (error) {
      if (controller !== this.controller) return;
      this.message = `${errorMessage(error)} The last confirmed selection is shown; refresh to confirm controller state.`;
      if (error instanceof ControllerError && error.status === 401) this.authenticationFailed();
    } finally {
      if (controller === this.controller) { this.pending.delete(group.name); this.render(); }
    }
  }

  private resolved(name?: string): string {
    const visited = new Set<string>();
    while (name && !visited.has(name)) {
      visited.add(name);
      const target: Proxy | undefined = this.proxies[name];
      if (!target?.now) return name;
      name = target.now;
    }
    return 'Unavailable';
  }

  private groupCard(group: Proxy, filter: string): HTMLElement {
    const card = node('article', '', 'panel group');
    card.setAttribute('aria-label', `Proxy group ${group.name}`);
    card.append(node('h2', group.name), node('span', group.type, 'badge'));
    const selectable = group.type.toLowerCase() === 'selector';
    card.append(node('p', `Current selection: ${group.now ?? 'Unavailable'}`, 'selection'));
    if (group.now && this.proxies[group.now]?.all) card.append(node('p', `Resolved node: ${this.resolved(group.now)}`, 'muted'));
    if (!selectable) card.append(node('p', 'Selection is managed automatically by the controller.', 'muted'));
    const members = node('div', '', 'members');
    for (const name of group.all ?? []) {
      if (filter && !`${group.name} ${name}`.toLowerCase().includes(filter)) continue;
      const proxy = this.proxies[name];
      const latest = proxy?.history?.at(-1)?.delay;
      const delay = latest && latest > 0 ? `${latest} ms` : 'Delay unavailable';
      const health = proxy?.alive === undefined ? '' : proxy.alive ? ' · Available' : ' · Unavailable';
      const button = node('button', '', `member${group.now === name ? ' selected' : ''}`);
      button.type = 'button';
      button.setAttribute('aria-label', `Select ${name} in ${group.name}`);
      button.setAttribute('aria-pressed', String(group.now === name));
      button.disabled = !selectable || !this.controller || this.pending.has(group.name) || group.now === name;
      button.append(node('strong', name), node('span', `${proxy?.type ?? 'Unknown type'} · ${delay}${health}`, 'muted'));
      button.addEventListener('click', () => { void this.select(group, name); });
      members.append(button);
    }
    card.append(members);
    return card;
  }

  render(): void {
    const filter = element<HTMLInputElement>('proxy-filter').value.toLowerCase();
    const groups = Object.values(this.proxies).filter(proxy => Array.isArray(proxy.all));
    groups.sort((a, b) => a.name === 'PROXY' ? -1 : b.name === 'PROXY' ? 1 : a.name.localeCompare(b.name));
    const cards = groups.filter(group => !filter || `${group.name} ${group.all?.join(' ')}`.toLowerCase().includes(filter)).map(group => this.groupCard(group, filter));
    element('proxy-groups').replaceChildren(...cards);
    element('proxy-message').textContent = this.message;
    element<HTMLButtonElement>('refresh-proxies').disabled = !this.controller;
  }
}
