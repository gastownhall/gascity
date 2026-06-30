<<<<<<<< HEAD:internal/api/dashboardspa/dist/assets/agentReads-NY5zZttz.js
import{J as e,K as i}from"./index-zPatq59W.js";async function n(){const r=await e().listAgents(i("list supervisor agents"));return{...r,items:r.items??[]}}async function a(r){const t=r.trim();if(t.length===0)throw new Error("agent alias is required");return e().agentPrime(i("fetch supervisor agent prime"),t)}export{a as f,n as l};
========
import{I as e,J as i}from"./index-BRyrQZhc.js";async function n(){const r=await e().listAgents(i("list supervisor agents"));return{...r,items:r.items??[]}}async function a(r){const t=r.trim();if(t.length===0)throw new Error("agent alias is required");return e().agentPrime(i("fetch supervisor agent prime"),t)}export{a as f,n as l};
>>>>>>>> a1dd81a3b (chore(dashboard): rebuild dist after rebase onto origin/main):internal/api/dashboardspa/dist/assets/agentReads-DoEqmTdt.js
