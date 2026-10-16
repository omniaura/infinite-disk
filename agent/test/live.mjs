// Explicit local-only provider smoke. Never runs in CI or npm test.
import {triage} from "../bridge.mjs";
const input={version:1,candidates:[
 {id:"aaaaaaaaaaaaaaaa",eligible:true,age_days:45,size_bytes:20*2**30,dirty_files:1,blockers:[]},
 {id:"bbbbbbbbbbbbbbbb",eligible:false,age_days:1,size_bytes:8*2**30,dirty_files:3,blockers:["project_in_use"]}
]};
try{
 const result=await triage(input);
 if(!result.recommendations.length||result.requests>4||result.recommendations.some(r=>r.candidate_id==="bbbbbbbbbbbbbbbb"&&r.decision!=="blocked"))throw new Error("Failed smoke assertions");
 console.log(JSON.stringify({result:"passed",sdk:"1.3.0",provider:"openrouter",model:"openai/gpt-4.1-mini",requests:result.requests,capabilities:result.capabilities,performed_steps:result.steps,recommendations:result.recommendations.length,projects_mutated:0}));
}catch(error){
 // Emit only exception class, never provider messages, payloads, or credentials.
 console.error(JSON.stringify({result:"failed",error_type:error?.constructor?.name??"Error",stage:/^Pi triage failed at ([a-z_]+);/.exec(error?.message??"")?.[1]??"unknown"}));process.exitCode=1;
}
