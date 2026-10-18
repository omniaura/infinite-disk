import test from "node:test";
import assert from "node:assert/strict";
import {readFile} from "node:fs/promises";
import {TOOL_NAMES,SYSTEM_PROMPT,validateInventory,createInventoryTools,validateRecommendations,validateSuggestionEvidence,constrainSuggestionEvidence,providerSettings,triage} from "../bridge.mjs";

const candidate={id:"abcdef0123456789",eligible:true,age_days:30,size_bytes:5*2**30,size_complete:true,dirty_files:2,blockers:[]};
test("Ditto provider accepts only explicit first-party routing",()=>{
 const input={provider:"ditto",model:"gm/gpt-6-luna",base_url:"https://router.heyditto.ai/v1"};
 const selected=providerSettings(input);assert.equal(selected.env,"DITTO_INFERENCE_API_KEY");assert.equal(selected.registration.models[0].id,input.model);
 for(const base_url of ["https://evil.example/v1","http://router.heyditto.ai/v1","https://router.heyditto.ai/v1?x=y"])assert.throws(()=>providerSettings({...input,base_url}));
 assert.throws(()=>providerSettings({...input,provider:"other"}));assert.throws(()=>providerSettings({...input,model:undefined}));
 assert.equal(providerSettings({}).provider,"openrouter");
});
test("real Pi runtime uses Ditto tool-call wire format without external tools or private metadata",async()=>{
 const original=globalThis.fetch, calls=[];
 globalThis.fetch=async(url,init)=>{
  calls.push({url:String(url),body:JSON.parse(init.body),headers:new Headers(init.headers)});
  const delta=calls.length===1?{role:"assistant",tool_calls:[{index:0,id:"call_inventory",type:"function",function:{name:"inventory_summary",arguments:"{}"}}]}:
   {role:"assistant",content:JSON.stringify({recommendations:[{candidate_id:candidate.id,priority:1,decision:"review",reason:"Fixture metadata only",proposed_steps:["Ask approval"],suggested_actions:[]}]})};
  const chunk={id:"chatcmpl-fixture",object:"chat.completion.chunk",created:1,model:"gm/gpt-6-luna",choices:[{index:0,delta,finish_reason:calls.length===1?"tool_calls":"stop"}]};
  return new Response(`data: ${JSON.stringify(chunk)}\n\ndata: [DONE]\n\n`,{headers:{"content-type":"text/event-stream"}});
 };
 try {
  const result=await triage({version:1,provider:"ditto",base_url:"https://router.heyditto.ai/v1",model:"gm/gpt-6-luna",candidates:[{...candidate,path:"private-path",source:"private-source"}]},{apiKey:"ditto_inf_fixture_not_real"});
  assert.equal(result.requests,2);assert.equal(result.recommendations.length,1);assert.ok(result.steps.some(x=>x.check==='inventory_summary'));assert.deepEqual(calls[0].body.tool_choice,{type:'function',function:{name:'inventory_summary'}});
  for(const call of calls){assert.equal(call.url,"https://router.heyditto.ai/v1/chat/completions");assert.equal(call.headers.get('authorization'),'Bearer ditto_inf_fixture_not_real');assert.equal(call.body.model,'gm/gpt-6-luna');assert.deepEqual(call.body.tools.map(x=>x.function.name),TOOL_NAMES);assert.equal(call.body.max_completion_tokens,4096);assert.ok(!JSON.stringify(call.body).includes('private-path'));assert.ok(!JSON.stringify(call.body).includes('private-source'));}
 } finally {globalThis.fetch=original;}
});
test("inventory projects away all private metadata",()=>{
 const data=validateInventory({version:1,candidates:[{...candidate,path:"/private/project",name:"secret project",source:"private source",remote:"private remote"}]});
 assert.deepEqual(data,[candidate]);assert.ok(!JSON.stringify(data).includes("private"));
});
test("the fourth Pi request disables tools for a final answer instead of spending a fifth request",async()=>{
 const original=globalThis.fetch,calls=[];
 const c={...candidate,eligible:false,analysis:{source_checked:false,ignored_complete:true,ignored_count:0,ignored_example_categories:{},available_suggestions:["resolve_blockers","native_session_handoff"]}};
 globalThis.fetch=async(url,init)=>{
  const body=JSON.parse(init.body);calls.push(body);
  const actions=[['inventory_summary',{}],['inspect_project',{id:c.id}],['suggest_action',{id:c.id,action:'resolve_blockers'}]];
  const final={recommendations:[{candidate_id:c.id,priority:1,decision:'analyze',reason:'Resolve checked prerequisites first',proposed_steps:['Ask approval'],suggested_actions:['resolve_blockers']}]};
  // A model would spend the fourth round on another optional suggestion if
  // tools were still available, leaving no request budget for its answer.
  const action=actions[calls.length-1]??['suggest_action',{id:c.id,action:'native_session_handoff'}];
  const delta=calls.length<4 || body.tool_choice!=='none'?{role:'assistant',tool_calls:[{index:0,id:`call_${calls.length}`,type:'function',function:{name:action[0],arguments:JSON.stringify(action[1])}}]}:{role:'assistant',content:JSON.stringify(final)};
  const chunk={id:'chatcmpl-budget',object:'chat.completion.chunk',created:1,model:'gm/gpt-6-luna',choices:[{index:0,delta,finish_reason:delta.tool_calls?'tool_calls':'stop'}]};
  return new Response(`data: ${JSON.stringify(chunk)}\n\ndata: [DONE]\n\n`,{headers:{'content-type':'text/event-stream'}});
 };
 try {
  const result=await triage({version:1,provider:'ditto',base_url:'https://router.heyditto.ai/v1',model:'gm/gpt-6-luna',candidates:[c]},{apiKey:'ditto_inf_fixture_not_real'});
  assert.equal(calls.length,4);assert.equal(result.requests,4);assert.deepEqual(calls[2].tool_choice,{type:'function',function:{name:'suggest_action'}});assert.equal(calls[3].tool_choice,'none');assert.ok(calls.slice(0,3).every(x=>x.tool_choice!=='none'));assert.ok(calls[3].messages.some(x=>typeof x.content==='string'&&x.content.includes('This is the final request')));assert.equal(result.recommendations[0].decision,'analyze');assert.deepEqual(result.recommendations[0].suggested_actions,['resolve_blockers']);
 } finally {globalThis.fetch=original;}
});
test("final proposed actions need actual suggestion-tool evidence",()=>{
 const c={...candidate,analysis:{available_suggestions:["preserve_source"]}};
 const r={candidate_id:c.id,suggested_actions:["preserve_source"]};
 assert.throws(()=>validateSuggestionEvidence([r],[c],[]));
 assert.throws(()=>validateSuggestionEvidence([r],[c],[{check:"suggest_action",detail:c.id+":keep"}]));
 validateSuggestionEvidence([r],[c],[{check:"suggest_action",detail:c.id+":preserve_source"}]);
});
test("discard unrecorded suggestions without inventing evidence or relaxing allowed-action validation",()=>{
 const c={...candidate,analysis:{available_suggestions:["preserve_source","resolve_blockers"]}};
 const raw={candidate_id:c.id,priority:1,decision:"review",reason:"Fixture",proposed_steps:[],suggested_actions:["preserve_source","resolve_blockers"]};
 const steps=[{check:"suggest_action",outcome:"performed",detail:c.id+":preserve_source"}];
 const validated=validateRecommendations({recommendations:[raw]},[c]);
 const result=constrainSuggestionEvidence(validated,steps);
 assert.deepEqual(result.recommendations[0].suggested_actions,["preserve_source"]);
 assert.deepEqual(result.warnings,["omitted_unrecorded_suggestions"]);
 assert.equal(steps.length,1);assert.equal(raw.suggested_actions.length,2);
 validateSuggestionEvidence(result.recommendations,[c],steps);
 assert.throws(()=>validateRecommendations({recommendations:[{...raw,suggested_actions:["delete_without_approval"]}]},[c]));
 assert.throws(()=>validateSuggestionEvidence(result.recommendations,[c],[]));
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
