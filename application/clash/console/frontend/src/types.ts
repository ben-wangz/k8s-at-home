export interface Proxy {
  name: string;
  type: string;
  all?: string[];
  now?: string;
  alive?: boolean;
  history?: { time: string; delay: number }[];
}

export interface Connection {
  id: string;
  metadata?: { host?: string; destinationIP?: string; destinationPort?: string; sourceIP?: string; sourcePort?: string; network?: string; type?: string; process?: string; processPath?: string };
  upload?: number;
  download?: number;
  start?: string;
  chains?: string[];
  rule?: string;
  rulePayload?: string;
}

export interface Connections {
  connections?: Connection[] | null;
  uploadTotal?: number;
  downloadTotal?: number;
}

export interface Traffic { up?: number; down?: number; upTotal?: number; downTotal?: number }
export interface LogEvent { time?: string; type?: string; payload?: string; level?: string; message?: string; fields?: unknown }
export type StreamState = 'Connecting' | 'Connected' | 'Reconnecting' | 'Disconnected';
