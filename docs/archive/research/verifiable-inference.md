---
title: "Verifiable Distributed LLM Work"
---

## The Problem

You want to:
1. **Publish work** = (prompt, required model)
2. **Workers execute** = run prompt with specified model
3. **Submit results** = output + cryptographic proof that the specified model produced it
4. **Verification** = anyone can confirm without re-running

## The Three Families of Approaches

### 1. Zero-Knowledge Proofs (zkML) -- Perfect but Slow

The dream: a mathematical proof that a specific neural network produced a specific output.

| System | Max Model | Proof Time | Overhead |
|--------|-----------|------------|----------|
| **ZKTorch** | GPT-J 6B | 23 min (64 threads) | 3,500x |
| **zkLLM** | LLaMA-2 13B | 15 min (A100) | 500,000x |
| **zkPyTorch** | Llama-3 8B | 150s/token (1 CPU) | 10,000x |
| **DeepProve** | GPT-2 124M | 54-158x faster than EZKL | Unknown |
| **EZKL** | Small models | Minutes | 100-1000x |

**Model identity**: All use **cryptographic commitment to weights**. The verification key binds to exact weights -- change one parameter and the proof fails. This proves "the model with commitment X produced output Y from input Z."

**The problem**: 3,500x-500,000x overhead. Proving one GPT-J inference takes 23 minutes. Proving GPT-4-class (1.8T params) is years away. And this is per-token for autoregressive models.

### 2. Trusted Execution Environments (TEEs) -- Fast but Breakable

NVIDIA H100 confidential computing runs LLMs with **&lt;7% overhead** (approaches 0% for 70B+ models). Hardware attestation proves firmware integrity via a fuse-burned ECC-384 key.

**But**: The **TEE.Fail attack** (October 2025) broke Intel TDX, AMD SEV-SNP, and NVIDIA CC attestation with a **&lt;$1,000 DDR5 bus interposer**. Researchers forged attestation quotes indistinguishable from legitimate ones. Intel and AMD consider physical interposer attacks "out of scope" and have no planned fixes.

Worse: GPU attestation measures *firmware*, not *model weights*. Application-layer extensions can hash weights into measurement registers, but this requires trusting the inference framework code -- turtles all the way down.

**TEEs reduce to physical security**, not cryptographic security. Fine for cloud providers with locked cages. Not "unstoppable."

### 3. Optimistic + Economic Verification -- Practical and Scalable

The breakthrough insight: **don't prove every computation. Make fraud unprofitable.**

**Key developments:**

- **Deterministic inference is now solved.** Thinking Machines Lab showed that batch-invariant CUDA kernels produce **1000/1000 identical outputs** for Qwen-3-235B at temperature 0 (1.6x slowdown). SGLang (LMSYS, Sep 2025) ships this. Verification becomes a **byte-equality check**.

- **EigenAI** (Jan 2026): Workers stake capital. Results are tentative for a challenge window. Challengers re-execute deterministically inside TEEs. Disagreement = slashing. 100% bit-identical across 10,000 runs on same hardware.

- **Hyperbolic PoSP**: Game-theoretic Nash equilibrium. If challenge probability > (fraud gain / slash amount), honest computation is the dominant strategy. &lt;1% overhead. Adaptive sampling per node reputation.

- **VeriLLM**: Workers commit Merkle root of hidden-state tensors. VRF-based random sampling of intermediate states. Statistical tests distinguish hardware rounding from model substitution. ~1% verification cost.

---

## The Solution: Layered Verification Protocol (LVP)

No single approach works alone. The "unstoppable" solution is a **layered escalation protocol** where verification cost is proportional to distrust:

```
                        COST
                         ^
                         |
    Layer 4: zkML proof  |  ####  (minutes, cryptographic certainty)
                         |
    Layer 3: Deterministic|  ###   (seconds, re-execution)
             re-execution |
                         |
    Layer 2: Intermediate |  ##    (milliseconds, Merkle proofs)
             state audit  |
                         |
    Layer 1: Commitment + |  #     (zero, economic deterrent)
             stake        |
                         +------------------------------> TRUST
```

### How It Works

**Work Publication:**
```
WorkUnit {
    prompt:       "Implement the user authentication module..."
    model:        "llama-3.1-70b"
    model_hash:   SHA384(weights_file)      // commitment to exact weights
    quant:        "fp16"                     // required precision
    seed:         0x7f3a...                  // deterministic seed
    max_tokens:   4096
    reward:       0.50 USDC
    stake_req:    10.00 USDC                 // worker must stake
    escalation:   [commit, reexec, zkml]     // verification layers
}
```

**Worker Execution:**
1. Worker stakes `stake_req`
2. Downloads model weights, verifies `SHA384(weights) == model_hash`
3. Runs inference with **batch-invariant kernels** + fixed seed (deterministic)
4. Computes **Merkle tree over hidden states** at each transformer layer
5. Submits:

```
Result {
    output:       "Here's the implementation..."
    merkle_root:  0xabc123...                // commitment to all intermediates
    output_hash:  SHA384(output)
    signature:    sign(worker_key, output_hash || merkle_root)
}
```

**Verification Escalation:**

**Layer 1 (default, zero cost):** Result accepted after challenge window (e.g., 10 minutes). No challenger = trusted. The economic stake makes fraud irrational when challenge probability is calibrated per Hyperbolic's PoSP formula.

**Layer 2 (cheap, on challenge):** Challenger requests random intermediate states using VRF-derived indices. Worker reveals Merkle proofs for those hidden-state slices. Challenger spot-checks against their own partial re-execution. Disagreement triggers Layer 3.

**Layer 3 (moderate, on dispute):** Full deterministic re-execution by a committee. Because inference is deterministic (batch-invariant kernels + fixed seed + same hardware class), output must be **byte-identical**. Disagreement = worker slashed.

**Layer 4 (expensive, nuclear option):** If hardware class differs (can't do byte-equality), generate a ZKTorch proof for the disputed segment. For a single transformer layer, this takes seconds, not minutes. The proof is stored permanently as irrefutable evidence.

### Model Identity: The Key Innovation

For **open-weight models** (Llama, Mistral, Qwen, etc.):
- `model_hash = SHA384(canonical_weights_file)`
- A public registry maps model names to weight hashes (think: Hugging Face + content-addressable storage)
- Worker must demonstrate they loaded the exact weights
- Deterministic execution proves the committed model produced the output

For **closed-weight API models** (Claude, GPT-4):
- The API provider signs responses: `sign(provider_key, prompt_hash || response || model_version || timestamp)`
- **Token-DiFR fingerprinting**: regenerate with same seed, >98% token match confirms the claimed model
- Provider reputation + legal accountability replaces cryptographic proof
- Future: providers run inside TEEs with attestation (Phala already does this with DeepSeek on OpenRouter)

### Why This Is "Unstoppable"

1. **No single point of trust.** Hardware can be compromised (TEE.Fail). Software can be buggy. But the combination of cryptographic commitments + economic stakes + deterministic re-execution + zkML escalation has no single attack vector that defeats all layers.

2. **Economically rational honesty.** At Layer 1, if `challenge_probability * slash_amount > fraud_gain`, the Nash equilibrium is honesty. No cryptography needed -- just game theory.

3. **Cryptographic fallback exists.** If you truly need mathematical certainty, Layer 4 (zkML) is available. ZKTorch can prove GPT-J 6B in 23 minutes today. GPU acceleration will bring this to minutes. For individual disputed operations, it's seconds.

4. **Deterministic inference is production-ready.** SGLang ships it. Thinking Machines proved it at 235B scale. The "outputs are non-deterministic" objection is no longer valid.

5. **Works for any model size.** The default path (Layer 1-2) has &lt;1% overhead regardless of model size. You only pay the zkML cost if someone actually disputes AND you can't do byte-equality re-execution.

### What Exists Today vs. What Needs Building

| Component | Status | Who |
|-----------|--------|-----|
| Deterministic inference kernels | Production | SGLang, Thinking Machines |
| Weight commitment registry | Exists (Hugging Face hashes) | Needs formalization |
| Economic staking/slashing | Production | EigenLayer, Hyperbolic PoSP |
| Merkle tree over hidden states | Research prototype | VeriLLM |
| zkML for LLMs | Research (ZKTorch, zkLLM) | 6-13B proven |
| Commit-reveal protocol | Production | VeriLLM, Atoma Network |
| TEE attestation for inference | Production | Phala, Chutes |

The gap is **integration** -- combining these pieces into a single protocol. Each piece exists. Nobody has assembled the full layered stack.

## Sources

- [ZKTorch (arXiv)](https://arxiv.org/abs/2507.07031) -- 23-min proof for GPT-J 6B
- [zkLLM (CCS 2024)](https://arxiv.org/abs/2404.16109) -- LLaMA-2 13B proving
- [Definitive Guide to ZKML 2025](https://blog.icme.io/the-definitive-guide-to-zkml-2025/)
- [NVIDIA H100 Confidential Computing](https://developer.nvidia.com/blog/confidential-computing-on-h100-gpus-for-secure-and-trustworthy-ai/)
- [TEE.Fail Attack](https://tee.fail/) -- broke TEE attestation with $1K hardware
- [EigenAI (arXiv)](https://arxiv.org/html/2602.00182) -- deterministic optimistic verification
- [Hyperbolic PoSP](https://arxiv.org/html/2405.00295) -- game-theoretic verification
- [VeriLLM](https://arxiv.org/html/2509.24257v1) -- commit-reveal with Merkle proofs
- [SGLang Deterministic Inference](https://lmsys.org/blog/2025-09-22-sglang-deterministic/)
- [Thinking Machines: Defeating Nondeterminism](https://thinkingmachines.ai/blog/defeating-nondeterminism-in-llm-inference/)
- [Gensyn RepOps](https://github.com/gensyn-ai/repops-demo) -- bitwise reproducible GPU ops
- [SPEX Statistical Proofs](https://arxiv.org/html/2503.18899) -- handles non-determinism via LSH
- [Phala Network GPU TEE](https://phala.com/posts/GPU-TEEs-is-Alive-on-OpenRouter) -- TEE inference in production
- [Token-DiFR Fingerprinting](https://adamkarvonen.github.io/machine_learning/2025/11/28/difr.html) -- 98% token match for model ID
- [Inference Labs on EigenLayer](https://blog.eigencloud.xyz/ai-beyond-the-black-box-inference-labs-is-making-verifiable-decentralized-ai-a-reality-with-eigenlayer/)
- [Chutes Confidential Compute](https://chutes.ai/news/confidential-compute-for-ai-inference-how-chutes-delivers-verifiable-privacy-with-trusted-execution-environments)
- [Tolerance-Aware Verification](https://news.ycombinator.com/item?id=45655524) -- 0.3% overhead, no TEE needed
