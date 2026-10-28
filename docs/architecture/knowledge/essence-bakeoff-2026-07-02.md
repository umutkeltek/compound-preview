# Essence-First Extraction Bake-Off — 2026-07-02

Scope: `work_8e74b24e3d11`, commit `c3ce67b`.

Method: reconstructed the same eight live arxiv knowledge items from Compound evidence rows into a temporary `compound.knowledge_export.v0.1` file, imported that file into fresh temporary `COMPOUND_HOME` directories per lane, then ran:

```bash
compound knowledge extract --pending \
  --openai-compatible-url <lane-url> \
  --model <lane-model> \
  --eval-corpus fixtures/distiller \
  --json
```

No real `COMPOUND_HOME` knowledge records were mutated during the bake-off. No model-router, provider, account, fallback, or cost-policy configuration was changed.

## Results

| Lane | Model | Result |
|---|---|---:|
| OpenRouter free router | `openrouter/openrouter/free` through gateway | 8/8 items shaped; 22 shaped candidates; 0 residue; 0 hallucination rejections |
| Mac local qwen direct | `qwen/qwen3.6-27b` through local LM Studio OpenAI-compatible endpoint | 8/8 items shaped; 38 shaped candidates; 0 residue; 0 hallucination rejections |
| OpenRouter qwen free | `openrouter/qwen/qwen3-coder:free` through gateway | 0/8; upstream 429 then no eligible target |
| OpenRouter GPT-OSS free | `openrouter/openai/gpt-oss-120b:free` through gateway | 0/8; upstream 429 then no eligible target |
| NVIDIA minimax free | `nvidia/minimaxai/minimax-m3` through gateway | 0/8; no eligible target |

Threshold verdict: the explicit falsifier passed for the required lanes. The free-pool router and local qwen both exceeded the `>=80%` item-shaped threshold, each reaching `100%` on the eight-item set.

Boundary note: lane failures above are availability/rate-limit/eligibility failures, not grounded-shaper failures. The two lanes that returned essence produced no residue and no hallucination rejections under the unchanged grounded extractor gate.
