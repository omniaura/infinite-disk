import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import crypto from 'node:crypto';
import { addVersion, parseManifest, validVersion, installVerifiedClaude, compatibilityRelease, manifestPath } from './claude-compat.mjs';

const raw = JSON.stringify({ schema: 1, adapter: 'claude-local-handoff-v1', series: '2.1', versions: ['2.1.302', '2.1.305'] });
test('candidate updates are exact, deduplicated and deterministic', () => {
  assert.deepEqual(parseManifest(addVersion(raw, '2.1.306')).versions, ['2.1.302', '2.1.305', '2.1.306']);
  assert.equal(addVersion(addVersion(raw, '2.1.306'), '2.1.306'), addVersion(raw, '2.1.306'));
  for (const v of ['2.2.0','3.0.0','2.1.0305','2.1.306\ninjected','latest','../../file']) assert.equal(validVersion(v), false);
  assert.throws(() => addVersion(raw, '2.2.0'));
  assert.throws(() => parseManifest(raw.slice(0,-1)+',"extra":true}'));
  assert.throws(() => parseManifest(raw.replace('2.1.302','2.1.305')));
});

test('patch releases require additive manifest-only changes',()=>{
  const added=addVersion(raw,'2.1.306');
  assert.equal(compatibilityRelease(raw,added,[manifestPath],['v0.1.22','v0.1.23','v9.0.0-rc1']),'v0.1.24');
  assert.equal(compatibilityRelease(raw,raw,[manifestPath],['v0.1.23']),null);
  assert.equal(compatibilityRelease(raw,added,[manifestPath,'internal/disk/source.go'],['v0.1.23']),null);
  assert.equal(compatibilityRelease(raw,added.replace('2.1.302','2.1.301'),[manifestPath],['v0.1.23']),null);
  assert.throws(()=>compatibilityRelease(raw,added,[manifestPath],[]));
});

function fixture(t, failure) {
  const root = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(),'infinite-disk-native-download-')));
  t.after(() => fs.rmSync(root, { recursive: true, force: true })); // owned fixture only
  const bytes = Buffer.from('fixture native bytes'), checksum = crypto.createHash('sha256').update(bytes).digest('hex');
  const calls = [];
  const request = async url => {
    if (url.endsWith('/manifest.json')) return Buffer.from(JSON.stringify({ version:failure==='manifest-version'?'2.1.307':'2.1.306', platforms:{'linux-x64':{checksum:failure==='checksum'?'0'.repeat(64):checksum}} }));
    return url.endsWith('/claude') ? bytes : Buffer.from('fixture public key/signature');
  };
  const run = (exe,args) => {
    calls.push([exe,args]);
    if (args.includes('show-only')) return 'pub:::::::::\nfpr:::::::::31DDDE24DDFAB679F42D7BD2BAA929FF1A7ECACE:\n'.replace('31DDDE24',failure==='key'?'00000000':'31DDDE24');
    if (args.includes('--verify') && failure==='signature') throw Error('fixture bad signature');
    return args.includes('--version') ? (failure==='version'?'2.1.307':'2.1.306')+' (Claude Code)\n' : '';
  };
  return {root,calls,request,run};
}

test('signed manifest and hash are verified before native execution; no installer', async t => {
  const f=fixture(t), directory=path.join(f.root,'verified');
  const r=await installVerifiedClaude('2.1.306',directory,{platform:'linux',arch:'x64',request:f.request,run:f.run});
  assert.equal(r.version,'2.1.306');
  assert.ok(f.calls.findIndex(c=>c[1].includes('--verify')) < f.calls.findIndex(c=>c[1].includes('--version')));
  assert.ok(f.calls.every(c=>c[0]==='gpg'||c[0]===path.join(directory,'claude')));
  await assert.rejects(()=>installVerifiedClaude('2.1.306',directory,{platform:'linux',arch:'x64',request:f.request,run:f.run}));
});

for(const failure of ['key','signature','checksum','manifest-version','version']) test(`invalid ${failure} fails closed`, async t => {
  const f=fixture(t,failure);
  const reason = { key:/fingerprint/, signature:/signature/, checksum:/checksum/, 'manifest-version':/manifest identity/, version:/version/ }[failure];
  await assert.rejects(()=>installVerifiedClaude('2.1.306',path.join(f.root,'bad'),{platform:'linux',arch:'x64',request:f.request,run:f.run}),reason);
  if(failure!=='version') assert.equal(f.calls.some(c=>c[1].includes('--version')),false);
});
