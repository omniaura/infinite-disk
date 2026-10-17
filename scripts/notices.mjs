// Generate notices from the locked installation. npm's licenses describe each
// dependency; upstream license files remain in the installed packages.
import fs from "node:fs";
import path from "node:path";
import {fileURLToPath} from "node:url";
const root=path.resolve(path.dirname(fileURLToPath(import.meta.url)),"..");
const lock=JSON.parse(fs.readFileSync(path.join(root,"agent/package-lock.json"),"utf8"));
const records=[];
for(const [location,entry] of Object.entries(lock.packages)){
 if(!location||!location.includes("node_modules/"))continue;
 const file=path.join(root,"agent",location,"package.json");
 if(!fs.existsSync(file)){if(entry.optional)continue;throw new Error(`Missing dependency ${location}`);}
 const pkg=JSON.parse(fs.readFileSync(file,"utf8"));
 records.push(`${pkg.name}@${pkg.version} | ${typeof pkg.license==="string"?pkg.license:JSON.stringify(pkg.license??"See upstream license")} | ${typeof pkg.repository==="string"?pkg.repository:pkg.repository?.url??"https://www.npmjs.com/package/"+pkg.name}`);
}
const unique=[...new Set(records)].sort();
const body="# Third-party notices\n\nInfinite Disk is MIT licensed. The optional Pi worker installs the following locked dependencies without lifecycle scripts. Their upstream licenses and copyright notices are distributed in the installed packages and remain applicable. Statically linked Go components: Cobra (Apache-2.0), pflag (BSD-3-Clause), mousetrap (Apache-2.0), and the Go runtime (BSD-3-Clause). Full license texts are in licenses/.\n\nPlatform-optional dependencies may be listed only when installed on the generating host; the lockfile is the complete dependency inventory.\n\n```text\n"+unique.join("\n")+"\n```\n";
const target=path.join(root,"THIRD_PARTY_NOTICES.md");
if(process.argv.includes("--check")){
 // Cross-platform optional packages differ. Validate every installed required
 // package has a notice, while retaining notices generated on another OS.
 const existing=fs.readFileSync(target,"utf8");for(const record of unique){if(!existing.includes(record)&&!record.includes("@silvia-odwyer/")){throw new Error("Dependency notices are stale: "+record.split(" | ")[0]);}}
 console.log("Third-party notices cover the installed dependency set.");
}else{fs.writeFileSync(target,body);console.log(`Generated notices for ${unique.length} installed packages.`);}
