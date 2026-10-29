import { test } from 'node:test'
import assert from 'node:assert/strict'
import { evaluateTrial } from '../../evals/agent/verify.mjs'
const scenario={state_assertions:['approved once'],quality_checks:['specific advice']}
const valid=()=>({schema_version:1,model_kind:'real',isolated_database:true,database_id:'unique-fixture',trace:[{event:'model'}],database_before:{tasks:[]},database_after:{tasks:[]},assertions:[{assertion:'approved once',passed:true,source:'database_and_trace',evidence_ref:'redacted-evidence.json'}],invariants:{no_unapproved_structural_writes:true,no_cross_user_access:true,no_duplicate_side_effects:true},review:{quality_score:2,reviewer:'qa-reviewer',rationale:'Checked actual effects',checks:[{check:'specific advice',passed:true}]}})
test('live eval requires database, trace, every assertion and quality review',()=>{
 assert.equal(evaluateTrial(scenario,valid()).passed,true)
 for(const field of ['trace','database_before','database_after','assertions','invariants','review','database_id']){const input=valid();delete input[field];assert.equal(evaluateTrial(scenario,input).passed,false,field)}
 const fake=valid();fake.model_kind='fake';assert.equal(evaluateTrial(scenario,fake).passed,false)
 const low=valid();low.review.quality_score=1;assert.equal(evaluateTrial(scenario,low).passed,false)
})
