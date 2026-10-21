// Compatibility maintenance only. Never reads user sessions or credentials.
import fs from 'node:fs';
import path from 'node:path';
import crypto from 'node:crypto';
import { execFileSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';

export const manifestPath = 'internal/disk/claude_compatibility.json';
const distribution = 'https://downloads.claude.ai/claude-code-releases';
const signingKey = '31DDDE24DDFAB679F42D7BD2BAA929FF1A7ECACE';
export const validVersion = value => typeof value === 'string' && /^2\.1\.(0|[1-9][0-9]{0,8})$/.test(value);

export function parseManifest(raw) {
  if (Buffer.byteLength(raw) > 65536) throw Error('Compatibility manifest too large');
  const p = JSON.parse(raw);
  if (Object.keys(p).sort().join(',') !== 'adapter,schema,series,versions' || p.schema !== 1 || p.adapter !== 'claude-local-handoff-v1' || p.series !== '2.1' || !Array.isArray(p.versions) || !p.versions.length || p.versions.some(v => !validVersion(v)) || new Set(p.versions).size !== p.versions.length) throw Error('Invalid compatibility manifest');
  return p;
}

export function addVersion(raw, version) {
  const p = parseManifest(raw);
  if (!validVersion(version)) throw Error('New native series requires adapter review');
  if (!p.versions.includes(version)) p.versions.push(version);
  p.versions.sort((a, b) => Number(a.split('.')[2]) - Number(b.split('.')[2]));
  return JSON.stringify(p, null, 2) + '\n';
}

export function compatibilityRelease(before, after, files, tags) {
  const a=parseManifest(before),b=parseManifest(after);
  if(files.length!==1||files[0]!==manifestPath||a.versions.some(v=>!b.versions.includes(v))||!b.versions.some(v=>!a.versions.includes(v))) return null;
  const versions=tags.filter(v=>/^v(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})$/.test(v)).map(v=>v.slice(1).split('.').map(Number));
  versions.sort((x,y)=>y[0]-x[0]||y[1]-x[1]||y[2]-x[2]);
  if(!versions.length) throw Error('No stable release baseline');
  const [major,minor,patch]=versions[0];
  return `v${major}.${minor}.${patch+1}`;
}

async function download(url, limit) {
  const u = new URL(url);
  if (u.origin !== 'https://downloads.claude.ai' || !/^\/(claude-code-releases|keys)\//.test(u.pathname)) throw Error('Unapproved download origin');
  try {
    // No shell, redirects, curlrc, netrc, auth or user-provided origin. Buffer
    // and transfer size are both bounded; transient failures get two retries.
    return execFileSync('curl', ['--disable','--fail','--silent','--show-error','--proto','=https','--max-time','60','--retry','2','--retry-delay','1','--max-filesize',String(limit),'--',u.href], {timeout:185000,maxBuffer:limit,stdio:['ignore','pipe','pipe']});
  } catch { throw Error('Official Claude download unavailable or exceeded size/time bound'); }
}

function execute(exe, args) {
  try {
    return execFileSync(exe, args, { encoding: 'utf8', timeout: 30000, maxBuffer: 1 << 20, stdio: ['ignore', 'pipe', 'pipe'] });
  } catch { throw Error('Claude signature or binary verification command failed'); }
}

// Signed official manifest -> exact binary checksum -> exact --version. The
// native installer is not executed, and the user's Claude install is untouched.
export async function installVerifiedClaude(version, directory, { platform = process.platform, arch = process.arch, request = download, run = execute } = {}) {
  if (!validVersion(version)) throw Error('Invalid native version');
  const target = `${platform}-${{ x64: 'x64', arm64: 'arm64' }[arch]}`;
  if (!/^(darwin|linux)-(x64|arm64)$/.test(target)) throw Error('Unsupported native platform');
  if (!path.isAbsolute(directory) || fs.realpathSync(path.dirname(directory)) !== path.dirname(directory)) throw Error('Noncanonical install parent');
  fs.mkdirSync(directory, { mode: 0o700 }); // must be new, never overwrite
  // GnuPG socket paths have a small Unix limit; an otherwise valid long CI or
  // private-state path can fail on first import. Use a new short public-only
  // keyring, never the user's keyring, and discard only this owned scratch.
  const home = fs.mkdtempSync(path.join(fs.realpathSync('/tmp'), 'id-public-gpg-'));
  try {
  const key = path.join(directory, 'key.asc');
  const manifest = path.join(directory, 'manifest.json');
  const signature = path.join(directory, 'manifest.json.sig');
  fs.writeFileSync(key, await request('https://downloads.claude.ai/keys/claude-code.asc', 1 << 20), { flag: 'wx', mode: 0o600 });
  const gpg = ['--no-options', '--batch', '--no-autostart', '--homedir', home];
  const info = run('gpg', [...gpg, '--with-colons', '--import-options', 'show-only', '--import', key]);
  if (info.split('\n').filter(l => l.startsWith('pub:')).length !== 1 || info.split('\n').find(l => l.startsWith('fpr:'))?.split(':')[9] !== signingKey) throw Error('Anthropic release signing key fingerprint changed; review required');
  run('gpg', [...gpg, '--import', key]);
  const raw = await request(`${distribution}/${version}/manifest.json`, 1 << 20);
  fs.writeFileSync(manifest, raw, { flag: 'wx', mode: 0o600 });
  fs.writeFileSync(signature, await request(`${distribution}/${version}/manifest.json.sig`, 1 << 20), { flag: 'wx', mode: 0o600 });
  run('gpg', [...gpg, '--no-auto-key-retrieve', '--verify', signature, manifest]);
  const release = JSON.parse(raw);
  const checksum = release.platforms?.[target]?.checksum;
  if (release.version !== version || !/^[a-f0-9]{64}$/.test(checksum ?? '')) throw Error('Signed release manifest identity invalid');
  const data = await request(`${distribution}/${version}/${target}/claude`, 512 << 20);
  if (crypto.createHash('sha256').update(data).digest('hex') !== checksum) throw Error('Claude binary checksum mismatch');
  const binary = path.join(directory, 'claude');
  fs.writeFileSync(binary, data, { flag: 'wx', mode: 0o700 });
  if (run(binary, ['--version']).trim() !== `${version} (Claude Code)`) throw Error('Verified binary native version differs');
  return { version, platform: target, sha256: checksum, signing_fingerprint: signingKey };
  } finally {
    fs.rmSync(home, { recursive: true, force: true });
  }
}

async function main() {
  const [mode, value, directory] = process.argv.slice(2);
  const raw = fs.readFileSync(manifestPath, 'utf8');
  const manifest = parseManifest(raw);
  if (mode === 'latest-tested') {
    console.log(manifest.versions.slice().sort((a,b) => Number(a.split('.')[2]) - Number(b.split('.')[2])).at(-1));
  } else if (mode === 'discover') {
    const version = value || (await download(`${distribution}/latest`, 128)).toString().trim();
    if (!validVersion(version)) throw Error('New native series or invalid upstream version; manual adapter review required');
    const result = { version, needed: !manifest.versions.includes(version) };
    if (process.env.GITHUB_OUTPUT) fs.appendFileSync(process.env.GITHUB_OUTPUT, `version=${version}\nneeded=${result.needed}\n`);
    console.log(JSON.stringify(result));
  } else if (mode === 'add-version') {
    fs.writeFileSync(manifestPath, addVersion(raw, value));
  } else if (mode === 'install') {
    console.log(JSON.stringify(await installVerifiedClaude(value, directory)));
  } else if (mode === 'pr-body') {
    if(!validVersion(value)) throw Error('Invalid native version');
    const body=`## Claude Code ${value} compatibility\n\nAdd only this exact native version to the embedded compatibility manifest.\n\nThe scheduled probe verified the pinned Anthropic signing-key fingerprint, detached manifest signature, binary SHA-256 and exact native version, then passed isolated offline cleanup, transcript-prefix/sidecar preservation, independent archive restore and both path/ID resume on macOS and Linux. The candidate included current and older conversation headers. No user data or provider credentials were available.\n\nProbe evidence: https://github.com/${process.env.GITHUB_REPOSITORY}/actions/runs/${process.env.GITHUB_RUN_ID}\n\nRequired CI reruns on this exact branch. Unknown versions remain blocked; no runtime download, fuzzy admission, settings change, source deletion or session rewrite is introduced. Refs #6.\n\nAfter protected merge and successful main CI, the compatibility-only release workflow publishes the next checksummed patch release.\n`;
    fs.writeFileSync(directory,body,{flag:'wx',mode:0o600});
  } else if(mode === 'release-plan') {
    const files=execute('git',['diff','--name-only','HEAD^','HEAD']).trim().split('\n');
    if(files.length!==1||files[0]!==manifestPath) { console.log('Not a compatibility-only change');return; }
    const before=execute('git',['show',`HEAD^:${manifestPath}`]);
    const tags=execute('git',['tag','--list']).trim().split('\n');
    let tag=compatibilityRelease(before,raw,files,tags);
    if(tag) {
      const existing=execute('git',['tag','--points-at','HEAD']).trim().split('\n').filter(t=>/^v[0-9]+\.[0-9]+\.[0-9]+$/.test(t));
      if(existing.length>1) throw Error('Multiple release tags on compatibility commit; manual review required');
      tag=existing[0]||tag;
    }
    if(tag&&process.env.GITHUB_OUTPUT) fs.appendFileSync(process.env.GITHUB_OUTPUT,`tag=${tag}\neligible=true\n`);
    console.log(JSON.stringify({eligible:!!tag,tag}));
  } else throw Error('Expected latest-tested, discover, add-version, or install');
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  main().catch(error => { console.error(error instanceof SyntaxError ? 'Invalid compatibility JSON' : error.message); process.exitCode = 1; });
}
