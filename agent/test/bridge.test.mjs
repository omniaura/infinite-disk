import test from "node:test";
import assert from "node:assert/strict";
import {readFile} from "node:fs/promises";
import {TOOL_NAMES,SYSTEM_PROMPT,validateInventory,createInventoryTools,validateRecommendations} from "../bridge.mjs";

const candidate={id:"abcdef0123456789",eligible:true,age_days:30,size_bytes:5*2**30,dirty_files:2,blockers:[]};
test("inventory projects away all private metadata",()=>{
 const data=validateInventory({version:1,candidates:[{...candidate,path:"/private/project",name:"secret project",source:"private source",remote:"private remote"}]});
 assert.deepEqual(data,[candidate]);assert.ok(!JSON.stringify(data).includes("private"));
});
test("invalid or excessive inventories fail closed",()=>{
 for(const input of [{version:2,candidates:[]},{version:1,candidates:Array(201).fill(candidate)},{version:1,candidates:[{...candidate,id:"../bad"}]},{version:1,candidates:[{...candidate,size_bytes:-1}]}])assert.throws(()=>validateInventory(input));
});
test("metadata and suggestion tools have no shell, file access, or approval",async()=>{
 const steps=[];const tools=createInventoryTools([candidate],steps);assert.deepEqual(tools.map(t=>t.name),TOOL_NAMES);
 const inventory=await tools[0].execute("call",{});assert.deepEqual(JSON.parse(inventory.content[0].text),[candidate]);
 const detail=await tools[1].execute("call",{id:candidate.id});assert.deepEqual(JSON.parse(detail.content[0].text),candidate);
 await assert.rejects(()=>tools[1].execute("call",{id:"unknown"}));assert.deepEqual(steps.map(s=>s.check),TOOL_NAMES.slice(0,2));
 assert.match(SYSTEM_PROMPT,/cannot read files, run commands, approve, commit, push, or delete/);
});
test("models cannot invent IDs or override blockers",()=>{
 const r={candidate_id:candidate.id,priority:1,decision:"review",reason:"large and old",proposed_steps:["Ask for project approval"]};
 assert.equal(validateRecommendations({recommendations:[r]},[{...candidate,eligible:false}])[0].decision,"blocked");
 for(const recommendations of [[{...r,candidate_id:"bad"}],[r,r],[{...r,priority:99}],[{...r,decision:"delete"}]])assert.throws(()=>validateRecommendations({recommendations},[candidate]));
});
test("resource loading and model calls are explicitly bounded",async()=>{
 const source=await readFile(new URL("../bridge.mjs",import.meta.url),"utf8");
 for(const setting of ["noExtensions:true","noSkills:true","noContextFiles:true","noTools:\"builtin\"","SessionManager.inMemory","credentials:memoryCredentials()","cacheWarming:\"off\"","requests>4","maxTokens:4096","retry:{enabled:false}"])assert.ok(source.includes(setting),setting);
 assert.ok(source.includes("session.agent.streamFunction="));
});
test("needs-analysis can propose offered prerequisites without overriding eligibility",async()=>{
 const a={source_checked:false,ignored_complete:true,ignored_count:2,ignored_example_categories:{private_data:1,unknown_data:1},available_suggestions:["private_backup","resolve_blockers"]};
 const c={...candidate,eligible:false,analysis:a};const steps=[];const tools=createInventoryTools([c],steps);
 const result=JSON.parse((await tools[2].execute("call",{id:c.id,action:"private_backup"})).content[0].text);
 assert.equal(result.executed,false);assert.equal(result.approved,false);assert.equal(result.status,"suggested_only");
 await assert.rejects(()=>tools[2].execute("call",{id:c.id,action:"remove_checkout"}));
 const r={candidate_id:c.id,priority:1,decision:"analyze",reason:"Back up private data first",proposed_steps:[],suggested_actions:["private_backup"]};
 assert.equal(validateRecommendations({recommendations:[r]},[c])[0].decision,"analyze");
 assert.throws(()=>validateRecommendations({recommendations:[{...r,suggested_actions:["remove_checkout"]}]},[c]));
 const data=validateInventory({version:1,candidates:[{...c,analysis:{...a,ignored:["private.env"],diff:"secret",path:"private",ignored_example_categories:{...a.ignored_example_categories,secret_name:42}}}]});
 assert.deepEqual(data[0].analysis,a);assert.ok(!JSON.stringify(data).includes("secret"));
});
