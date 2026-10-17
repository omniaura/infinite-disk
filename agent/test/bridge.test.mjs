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
test("only two metadata tools, no shell, file access, or approval",async()=>{
 const steps=[];const tools=createInventoryTools([candidate],steps);assert.deepEqual(tools.map(t=>t.name),TOOL_NAMES);
 const inventory=await tools[0].execute("call",{});assert.deepEqual(JSON.parse(inventory.content[0].text),[candidate]);
 const detail=await tools[1].execute("call",{id:candidate.id});assert.deepEqual(JSON.parse(detail.content[0].text),candidate);
 await assert.rejects(()=>tools[1].execute("call",{id:"unknown"}));assert.deepEqual(steps.map(s=>s.check),TOOL_NAMES);
 assert.match(SYSTEM_PROMPT,/cannot read files, run commands, approve, commit, push, or delete/);
});
test("models cannot invent IDs or override blockers",()=>{
 const r={candidate_id:candidate.id,priority:1,decision:"review",reason:"large and old",proposed_steps:["Ask for project approval"]};
 assert.equal(validateRecommendations({recommendations:[r]},[{...candidate,eligible:false}])[0].decision,"blocked");
 for(const recommendations of [[{...r,candidate_id:"bad"}],[r,r],[{...r,priority:99}],[{...r,decision:"delete"}]])assert.throws(()=>validateRecommendations({recommendations},[candidate]));
});
test("resource loading and model calls are explicitly bounded",async()=>{
 const source=await readFile(new URL("../bridge.mjs",import.meta.url),"utf8");
 for(const setting of ["noExtensions:true","noSkills:true","noContextFiles:true","noTools:\"builtin\"","SessionManager.inMemory","credentials:memoryCredentials()","cacheWarming:\"off\"","requests>4","maxTokens:1024","retry:{enabled:false}"])assert.ok(source.includes(setting),setting);
 assert.ok(source.includes("session.agent.streamFunction="));
});
