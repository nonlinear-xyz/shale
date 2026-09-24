#!/usr/bin/env python3
"""Render a private suite as an offline labeling sheet; no provider calls.

python3 scripts/render_jev_review.py --suite /path/suite.json --output /path/review.html
"""
import argparse
import json
import pathlib


def render(suite):
    # JSON is embedded as data, never as executable text or interpolated HTML.
    data = json.dumps(suite).replace('<', '\\u003c').replace('>', '\\u003e').replace('&', '\\u0026')
    return TEMPLATE.replace('__SUITE_JSON__', data)


TEMPLATE = '''<!doctype html>
<meta charset="utf-8"><title>Jev tuning label review</title>
<style>
body{font:16px/1.5 system-ui,sans-serif;max-width:1000px;margin:32px auto;padding:0 20px;color:#202329;background:#fafafa}
h1{font-size:28px}h2{font-size:22px}article{background:white;border:1px solid #ddd;padding:18px;margin:16px 0;border-radius:8px}
section{margin:40px 0}pre{white-space:pre-wrap;overflow-wrap:anywhere;font:14px/1.5 ui-monospace,monospace}code{overflow-wrap:anywhere}
label{display:inline-block;margin:8px 16px 8px 0}select,button{font:inherit;padding:6px 10px}button{cursor:pointer}blockquote{border-left:3px solid #ccc;padding-left:12px;margin:12px 0}.muted{color:#62666c}.bar{position:sticky;top:0;background:#fafafa;padding:12px 0;border-bottom:1px solid #ddd}textarea{width:100%;min-height:65px;font:inherit;box-sizing:border-box}
</style>
<h1>Review the tuning labels</h1>
<p>These are authored diagnostic tasks over real memory snapshots, not historical requests. Labels are assistant drafts made before Jev scoring. Review the task, each label, and the supporting evidence; full candidate text is expandable. An omitted required fact matters more than keyword overlap.</p>
<p>No model scores or ordering are shown. This file runs offline. It does not send data anywhere or change the original suite.</p>
<div class="bar"><button id="download">Download reviewed suite</button> <span id="status"></span></div>
<div id="cases"></div>
<script id="suite" type="application/json">__SUITE_JSON__</script>
<script>
const suite=JSON.parse(document.getElementById('suite').textContent());
const el=(tag,text,parent)=>{const n=document.createElement(tag);if(text!==undefined)n.textContent=text;if(parent)parent.append(n);return n};
const selections=new Map();
function refresh(){document.getElementById('status').textContent=suite.cases.filter(c=>c.reviewed).length+' / '+suite.cases.length+' cases reviewed';}
for(const c of suite.cases){
 const section=el('section',undefined,document.getElementById('cases'));
 el('h2',c.id,section);el('p',c.candidates.packet.task,section);
 el('p',c.mode+' · '+c.split+' · budget '+c.candidates.packet.budget.maxTokens+' tokens',section).className='muted';
 const notes=el('textarea',undefined,section);notes.value=c.labelNotes;notes.setAttribute('aria-label','Review notes for '+c.id);notes.oninput=()=>{c.labelNotes=notes.value;invalidate()};
 const choices=new Map();selections.set(c.id,choices);
 const all=Object.values(c.candidates.packet.sections).flat();const unique=[...new Map(all.map(e=>[e.ref,e])).values()].sort((a,b)=>a.title.localeCompare(b.title)||a.ref.localeCompare(b.ref));
 let taskCheck,reviewCheck;
 const invalidate=()=>{c.reviewed=false;if(reviewCheck)reviewCheck.checked=false;refresh()};
 for(const e of unique){
  const article=el('article',undefined,section);el('h3',e.title,article);el('code',e.ref,article);
  const row=el('div',undefined,article);const label=el('label','Label ',row);const select=el('select',undefined,label);
  for(const v of ['unlabeled','required','helpful','irrelevant']){const opt=el('option',v,select);opt.value=v;}
  select.value=['required','helpful','irrelevant'].find(k=>(c[k]||[]).includes(e.ref))||'unlabeled';
  const criticalLabel=el('label',undefined,row);const critical=el('input',undefined,criticalLabel);critical.type='checkbox';critical.checked=(c.critical||[]).includes(e.ref);el('span',' Critical omission',criticalLabel);
  choices.set(e.ref,{select,critical});
  select.onchange=()=>{if(select.value!=='required')critical.checked=false;invalidate()};critical.onchange=()=>{if(critical.checked)select.value='required';invalidate()};
  const evidence=(c.labelEvidence||{})[e.ref];if(evidence){el('p',evidence.rationale,article);el('blockquote',evidence.quote,article)}
  const details=el('details',undefined,article);el('summary','Full source text and provenance',details);el('pre',e.content+'\\n\\n'+JSON.stringify(e.provenance,null,2),details);
 }
 const taskLabel=el('label',undefined,section);taskCheck=el('input',undefined,taskLabel);taskCheck.type='checkbox';taskCheck.checked=!!c.taskReviewed;el('span',' I reviewed this task and its assumptions',taskLabel);
 taskCheck.onchange=()=>{c.taskReviewed=taskCheck.checked;invalidate()};
 const reviewLabel=el('label',undefined,section);reviewCheck=el('input',undefined,reviewLabel);reviewCheck.type='checkbox';reviewCheck.checked=!!c.reviewed;el('span',' I reviewed all labels and critical omissions',reviewLabel);
 reviewCheck.onchange=()=>{if(reviewCheck.checked&&(!taskCheck.checked||[...choices.values()].some(v=>v.select.value==='unlabeled')||![...choices.values()].some(v=>v.select.value==='required'))){alert('Review the task, label every candidate, and identify at least one required reference.');reviewCheck.checked=false;}c.reviewed=reviewCheck.checked;refresh()};
}
document.getElementById('download').onclick=()=>{
 for(const c of suite.cases){const choices=selections.get(c.id);for(const k of ['required','helpful','irrelevant','critical'])c[k]=(c[k]||[]).filter(ref=>!choices.has(ref));for(const [ref,v]of choices){if(v.select.value!=='unlabeled')c[v.select.value].push(ref);if(v.critical.checked)c.critical.push(ref);}}
 const url=URL.createObjectURL(new Blob([JSON.stringify(suite,null,2)+'\\n'],{type:'application/json'}));const a=el('a');a.href=url;a.download='jev-tuning-reviewed.json';a.click();setTimeout(()=>URL.revokeObjectURL(url),1000);
};refresh();
</script>
'''


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--suite', required=True, type=pathlib.Path)
    parser.add_argument('--output', required=True, type=pathlib.Path)
    args = parser.parse_args()
    suite = json.loads(args.suite.read_text())
    with args.output.open('x') as file:
        file.write(render(suite))
    args.output.chmod(0o600)
