# Memory Quickstart — govern-searchable memory from a fresh clone

> Goal: from a clean checkout, stand up a `COMPOUND_HOME`, feed it only this repo's public `docs/`
> tree, point it at any OpenAI-compatible embedder you already run locally, and get
> ledger-governed semantic search over that corpus — in under ten minutes, with zero personal data
> in the path. This is the memory slice of the substrate; the ledger stays authority (see
> [`memory-architecture.md`](./memory-architecture.md)).

A **corpus lane** is an imported, class-separated index. It is *not* ambient recall: it never
promotes source text into authority. `corpus index` embeds; `memory search` reads; neither mutates
the ledger. You can prove that last claim with `memory engine-doctor` (below).

## Prerequisites

- Go (to build the `compound` binary).
- One local OpenAI-compatible **embeddings** endpoint. Either works:
  - an Ollama server exposing `POST /v1/embeddings` with an embedding model pulled
    (e.g. `nomic-embed-text`, 768-dim), or
  - any other server that speaks the OpenAI `/v1/embeddings` shape (LM Studio, etc.).

  Don't have one running yet? With Ollama installed it is two commands, once:
  `ollama serve` (if it isn't already running) and `ollama pull nomic-embed-text` (~274 MB).
- Nothing else. No account, no cloud key, no external service.

## 1. Build

```sh
go build -o compound ./cmd/compound
```

## 2. Initialize a scratch home

All generated state lives under `COMPOUND_HOME`, never the repo tree.

```sh
export COMPOUND_HOME=/tmp/compound-quickstart
./compound home init
```

## 3. Register the public docs as a source family

`source manifest-create` records a content-hashed manifest of source files (it observes; it never
promotes source text into authority). Feed it the repo's `docs/` markdown:

```sh
find docs -name '*.md' -type f -print0 \
  | xargs -0 ./compound source manifest-create --family docs-public
./compound source manifest-doctor docs-public
```

`manifest-doctor` should report `ok` with `hash_mismatch=0`.

## 4. Configure a corpus lane + embedder

Write the memory-engine config. The `embedder` block is the only endpoint-specific part — swapping
it is a pure config change (Step 7).

```sh
mkdir -p "$COMPOUND_HOME/config"
cat > "$COMPOUND_HOME/config/memory-engine.yaml" <<'YAML'
schema_version: compound.memory_engine_config.v0.1
corpus_lanes:
  - lane: docs
    source_family: docs-public
    privacy: public
    embedder:
      provider: ollama
      model: nomic-embed-text
      dims: 768
      bits: f32
      base_url: 127.0.0.1:11434/v1
    chunk:
      unit: runes
      size: 4000
      overlap: 400
      boundary: structure
YAML
```

Notes:

- `base_url` may be given without a scheme for loopback hosts; `http://` is added automatically.
- `provider` is `ollama` or `openai` (both drive the OpenAI `/v1/embeddings` shape).
- `dims` must match the model's real output dimension (`nomic-embed-text` is 768). A mismatch is
  rejected at index time — it is a checked invariant, not a silent truncation.
- `privacy: public` allows any reachable endpoint. `privacy: private` refuses a non-loopback
  embedder by construction, so private corpora cannot be shipped to a remote service by accident.

## 5. Index

```sh
./compound corpus index --lane docs
./compound corpus doctor --lane docs
```

`index` reports `items` and `chunks embedded=…`; `doctor` should report `status: ok`.

## 6. Search (governed read)

```sh
./compound memory search --lane docs "policy decision receipt"
```

Each hit carries lane provenance (`lane=docs`), the embedder identity, the source item, and a
cosine score. To prove a read leaves the ledger untouched:

```sh
./compound memory engine-doctor "policy decision receipt"
```

Look for `ledger_unchanged: true` (the before/after ledger hashes match). On a docs-only home the
claim ledger is empty, so `engine-doctor` returns zero *claim* hits — that is expected; the corpus
lane is the fed read surface, and `memory search` is where the docs come back.

## 7. Swap the embedder (portability)

The index is keyed by embedder identity, so pointing at a different endpoint re-indexes cleanly with
no code change. Edit only the `embedder` block, e.g. for a second OpenAI-compatible server:

```yaml
    embedder:
      provider: openai
      model: text-embedding-nomic-embed-text-v1.5
      dims: 768
      bits: f32
      base_url: 127.0.0.1:1234/v1
```

Then re-run Step 5. `corpus doctor` will show the new embedder identity; the old index is left in
place under its own identity directory.

## Verification checklist

- `corpus index` → `items > 0`, `chunks embedded > 0`.
- `corpus doctor` → `status: ok`.
- `memory search --lane docs "policy decision receipt"` → a relevant `docs/` hit with `lane=docs`.
- `memory engine-doctor …` → `ledger_unchanged: true`.

If every step above passed and you never typed a personal path, account, or private hostname, the
memory layer is working for a stranger with zero personal data in the path.
