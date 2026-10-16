import fs from "node:fs";
import path from "node:path";
import {execFileSync} from "node:child_process";
import {fileURLToPath} from "node:url";
const root=path.resolve(path.dirname(fileURLToPath(import.meta.url)),"..");
const files=execFileSync("git",["ls-files","-z"],{cwd:root,encoding:"utf8"}).split("\0").filter(Boolean);
const secrets=/(?:sk-or-v1-|ghp_|github_pat_|xox[baprs]-)[A-Za-z0-9_-]{20,}|AKIA[0-9A-Z]{16}/;
const failures=[];
for(const file of files){const b=fs.readFileSync(path.join(root,file));if(b.length>2_000_000||secrets.test(b.toString())||/(?:^|\/)(?:node_modules|\.env|recovery|receipts)(?:\/|$)/.test(file))failures.push(file);}
if(failures.length){console.error("Public-source guard failed for files:",failures.join(", "));process.exit(1);}
console.log(`Public-source guard passed for ${files.length} tracked files (heuristic, not comprehensive secret detection).`);
