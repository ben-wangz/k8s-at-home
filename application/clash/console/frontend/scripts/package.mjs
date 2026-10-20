import { createHash } from 'node:crypto';
import { mkdir, readdir, readFile, rm, copyFile, writeFile } from 'node:fs/promises';
import { fileURLToPath } from 'node:url';
import { resolve } from 'node:path';

const root = fileURLToPath(new URL('../', import.meta.url));
const source = resolve(root, 'dist');
const destination = resolve(root, '../../chart/files/console');
await rm(destination, { recursive: true, force: true });
await mkdir(destination, { recursive: true });
const hashes = [];
for (const name of (await readdir(source)).sort()) {
  const contents = await readFile(resolve(source, name));
  await copyFile(resolve(source, name), resolve(destination, name));
  hashes.push(`${createHash('sha256').update(contents).digest('hex')}  ${name}`);
}
await writeFile(resolve(destination, 'SHA256SUMS'), `${hashes.join('\n')}\n`);
