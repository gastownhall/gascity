---
title: "PackV2 ロード機構 / pack 数 scale / MCP projection サマリ"
---

`engdocs/proposals/skill-materialization.md`, `skill-materialization-handoff.md`,
`skill-materialization-implementation-plan.md`, および補足として
`engdocs/proposals/mcp-materialization.md` を読了した時点の要点まとめ。

HQ beads `gc-zk6q`（vector-memory pack epic）および `gc-98xr`（pack 数ベンチ）
の前提知識として参照すること。

## 1. PackV2 のロード機構詳細

### Catalog merge / source discovery

スキル materializer が enumerate するソース集合は **union**:

1. 現在の city pack `<cityRoot>/skills/`
2. Bootstrap implicit-import パック `<gcHome>/cache/repos/<GlobalRepoCacheDirName(source, commit)>/skills/`
   - 解決パス: `config.ReadImplicitImports()` → `resolveImplicitImport`
     → `config.GlobalRepoCachePath`（`internal/config/implicit.go:26-93`）
   - 対象: `internal/bootstrap/bootstrap.go:BootstrapPacks`
     （`import`, `registry`, v0.15.1 で追加される `core`）
3. 各 agent 自身の `<source-pack>/agents/<name>/skills/`

### Precedence（高優先 → 低優先）

MCP 側でより明示的に整理されている総合 precedence（`mcp-materialization.md`）:

1. agent-local
2. rig-shared
3. city pack graph
4. explicit city imports
5. non-bootstrap implicit imports
6. bootstrap implicit imports

Skills 側の short version: 「agent-local が同名 skill で勝つ; shared 内では
city pack が bootstrap implicit より勝つ」（`skill-materialization.md` §No
attachment filtering / Skill source discovery）。

### Fragment resolution の現状制限

- ユーザ宣言の third-party `[imports.foo]/skills/` は v0.15.1 では **walk
  しない**（明示 non-goal）。将来追加は `ResolvedImports` 列挙を拡張する
  だけで済む additive な変更として設計されている。
- 同名衝突は **whole-definition replacement**（部分マージなし）。
- bootstrap pack 名と user `[imports.<name>]` の衝突は v0.15.1 で **hard
  error**（compose 側 + `gc init`/`gc import install` の両面で検出）。
  従来の silent shadowing から仕様変更されている点に注意。

### Agent prompt assembly（materialize)

- **Stage 1（scope root）**: 各 supervisor tick で agent ごとに
  `<scope-root>/.<vendor>/skills/<name>` への **per-skill symlink** を
  原子的に書き換える（write-temp + `os.Rename`）。
- **Stage 2（session workdir）**: WorkDir が scope root と異なる pool
  instance に対して、`PreStart` に `gc internal materialize-skills` を
  append。worktree 作成後・provider 起動前に実行される。
- Stage 2 は `subprocess` / `tmux` のみ対応。`k8s` / `acp` はスキップして
  ログ出力（host filesystem への `gc` バイナリアクセス前提のため）。
- 削除追跡: manifest を持たず、symlink target prefix が GC-managed root
  かどうかで ownership を判定する「ownership-by-target-prefix」方式。
- レガシー stub（v0.15.0 の regular directory）はパスとファイル shape の
  両方一致したときのみ削除し、ユーザ配置物は warning で温存。

### Fingerprint への寄与

- 各 materialize 済 skill に対し
  `runtime.Config.FingerprintExtra["skills:"+name] = runtime.HashPathContent(source)`。
- `FingerprintExtra` は `CoreFingerprint` に `"fp"` prefix sentinel 付きで
  参加する（`internal/runtime/fingerprint.go:132-136`）。
- `CopyEntry` 経由にはしない（各 runtime の `cfg.CopyFiles` が無条件
  staging するため、materializer の symlink を上書き / 重複する）。
- ステージ 2 ineligible（k8s/acp）agent には `skills:*` エントリを populate
  **しない**（spurious drain-restart 防止）。

## 2. pack 数増加時の scale 特性

### 計算量（per supervisor tick）

- **Stage 1 reconcile**: O(agents × union_size)。union_size = (city skills
  + bootstrap skills + agent-local skills) ≒ pack 数に対し概ね線形。
- **Cleanup walk**: `<sink>/.<vendor>/skills/` 直下を `os.Lstat` +
  `os.Readlink` で 1 エントリずつ判定（matrix は spec §Safety and decision
  matrix）。エントリ数に線形。
- **Fingerprint hashing**: `HashPathContent` は skill 内容ツリーを再帰的に
  ハッシュ。skill 1 個あたり O(files × bytes)。skill 数に線形だが定数倍
  大きい可能性あり。
- **Source discovery**: `ReadImplicitImports()` を materializer pass ごと
  に再読する（cache 言及なし）。implicit imports 数に線形。

### Pool scale-up コスト

- pool 0→N で **N 個の `gc internal materialize-skills` invocation** が
  並列発生。各 invocation がカタログを独立 walk。**per-pool caching なし**
  （`skill-materialization.md` §Open questions に follow-up として明記）。
- spec の見積りは「single-digit to low tens pool × dozens skills で
  spawn あたり < 1s」。理論的には pack 数増加で線形悪化。

### 上限・lazy load の有無

- 上限なし（spec に明示なし、bounded queue / cap も非言及）。
- **lazy load なし**: 毎 tick 全 agent × 全 source を walk。skills は実体
  symlink、MCP は実体 native config file としてすべて writes-through。
- 例外: stage-2 ineligible runtime は skill 配信を行わない → 該当 agent
  分の materializer cost は発生せず（fingerprint も populate しない）。

### MCP 側の追加コスト要因（`mcp-materialization.md`）

- target ごとに serialized writer + OS-level per-target lock。
- shared-target equality を「fully-expanded normalized model」で比較。
- 失敗 target は unhealthy-state record で churn 抑制（同じ failure
  signature の repeated tick は drain しない）。

### 理論的根拠（HQ gc-98xr ベンチ）

- スケーリング基底: 線形 O(packs × skills_per_pack × agents)。
- 定数項の支配項候補:
  - `HashPathContent` の I/O コスト（skill 内ファイル数依存）
  - `ReadImplicitImports` 毎回再読
  - per-target lock / write-temp-rename の syscall コスト（MCP 側）
- ベンチ設計上の注目点: pack 数を増やすときに **skills_per_pack** と
  **MCP servers per pack** をそれぞれ独立変数として扱うべき。配信機構が
  別物（symlink vs native config write）。
- v0.15.1 vs post-fold-in ベースラインの分離: `core` への `maintenance`
  / `dolt` fold-in は v0.15.1 後の main で着地予定 → ベンチ取得時期で
  bootstrap pack 件数の baseline が変動する。

## 3. provider-native MCP projection の現状仕様

### 現状（v0.15.1 時点）

- **MCP activation は v0.15.1 hotfix の non-goal**。`gc mcp list` は
  list-only のまま。`mcp-materialization.md` の仕様は **post-v0.15.1 で
  main に着地予定**。
- v0.15.0 時点で `mcp/` および `agents/<name>/mcp/` は pack/agent
  attachment root として認識されているが、provider-native config への
  projection 経路はまだ存在しない。

### 着地予定の projection 経路

| Provider | Target | 所有範囲 |
|----------|--------|----------|
| Claude family | `<workdir>/.mcp.json` | ファイル全体 |
| Gemini family | `<workdir>/.gemini/settings.json` | top-level `mcpServers` のみ |
| Codex family | `<workdir>/.codex/config.toml` | `[mcp_servers]` subtree のみ |

- Provider 判定は `ResolvedProvider.Kind` 基準（`fast-codex` 等の alias
  もネイティブ surface に正しくマップ）。
- Codex の project-local target はドキュメントと実装挙動が乖離している
  ため、現在の Codex build に対する acceptance test を **merge gate** と
  して持つ。失敗時は v1 形態では merge しない（user-global fallback は
  取らない）。

### Delivery model（skills と同形）

- **Stage 1**: scope-root reconcile を毎 `gc start` + supervisor tick。
- **Stage 2**: per-session で `gc internal project-mcp --agent <name>
  --workdir <path>` を pre-start に注入。worktree 作成後 / provider 起動
  前。
- すべての書き込みは `(provider-kind, target-path)` キーの **serialized
  writer** + OS-level per-target lock。
- 原子 temp-write + rename、最終ファイルに `0600`。symlink を経由する
  target は hard-error。

### 透過保護とクリーンアップ

- Gemini/Codex は GC-managed subtree 外の sibling 設定を温存（formatting
  保存は保証せず、canonical reserialize）。
- 初回 adoption: 既存 provider-native MCP 内容を `.gc/` 配下にバックアップ
  し warning 出力、その後 GC owned。secrets はバックアップにのみ残り、
  active 状態には引き継がない。
- 効果集合が空になると managed subtree / ファイルを削除。

### 同一 target 共有検査

- 複数 agent が同じ `(provider-kind, target path)` に projection する
  場合、template 展開 + 相対パス解決 + provider mapping すべて後の
  payload が **完全一致** することを hard 要求。違えば start/reconcile
  hard error。
- 既稼働 target が衝突に陥った場合は last-good を保持しつつ unhealthy
  マークのみ（新規 session start を block、既存 session は触らない）。

### Runtime support（v1）

- 対応: `tmux`、`subprocess`（scope root 完結のときのみ）
- 非対応（effective MCP がある場合は **hard-error**）:
  `subprocess` (workdir ≠ scope root), `k8s`, `acp`, `hybrid`
- 「best-effort warning」モードはなし。fail-closed。

## vector-memory pack 設計（HQ gc-zk6q）への影響

1. **配信方式の選択肢**: vector-memory 機能を skill として symlink 配信
   するか、MCP server として provider native config に projection するか
   で取得経路が完全に分かれる。両方混在も可能。
2. **bootstrap pack として ship する場合**: `internal/bootstrap/packs/` に
   ディレクトリを追加し `BootstrapPacks` に entry を足すだけ。`gc init` /
   `gc import install` 経由で `~/.gc/implicit-import.toml` に自動登録。
3. **third-party `[imports.vector-memory]` として ship する場合**:
   v0.15.1 では **shared skills が walk されない** ため、それ以降の
   imported-pack catalog 拡張を待つ必要がある（`engdocs/design/packv2/doc-pack-v2.md:59`
   tracked）。MCP 側は precedence 5（non-bootstrap implicit）として
   即時参加可能だが、その作業も MCP v1 の着地待ち。
4. **同名衝突戦略**: city pack が同名 vector-memory skill/MCP を持つと
   whole-definition で勝つ。adopter 側で意図的に override したいユース
   ケースがあるならドキュメント明記が必要。
5. **drift 影響**: vector-memory データ更新 → fingerprint 変化 → 影響
   agent が drain/restart。「データ更新で agent 強制再起動」は許容できる
   設計か再確認が必要。skill 内容が頻繁に変わるとセッションが安定しない
   危険。
6. **runtime 制約**: k8s/acp agent には skill 配信されない。MCP も
   subprocess (workdir ≠ scope root) で hard-error。vector-memory が
   pool worker 配信前提なら subprocess 経路は使えず tmux/stage-2 経路
   一択。

## pack 数ベンチ（HQ gc-98xr）の理論的根拠

- **計測軸**: (packs) × (skills_per_pack) × (mcp_per_pack) × (agents) ×
  (pool_size)。それぞれ独立して掃くべき。
- **支配項候補**: 大きい順に
  1. stage-2 invocation のプロセス起動コスト（pool スピンアップ時）
  2. `HashPathContent` の I/O（pack 内ファイル数に依存）
  3. cleanup walk の `os.Lstat`/`os.Readlink` 回数
  4. `ReadImplicitImports` 毎 pass 再読（小さいが pack 数に線形）
  5. MCP serialized writer の per-target lock / atomic rename
- **境界の見当**: pool < 10, skills < 100, packs < 20 程度では 1 tick
  あたり sub-second という spec の概算が成立しそう。それ以上のレンジを
  ベンチ対象にする場合、per-pool caching（spec の Open question §7）と
  `CoreFingerprintBreakdown` の per-skill drift 出力（§8）の不在が
  ボトルネックを見えにくくする可能性がある。
- **不在の最適化**: 全 tick で全 agent × 全 source walk（lazy load 無）。
  早期ベンチで線形性が崩れていたら何かが壊れている。

## 不明点 / 追加調査要事項

1. `ReadImplicitImports` の結果が単一 supervisor tick 内で memoize される
   かは spec 不明。tick あたり N 回読まれる可能性がある。実装側を確認
   要。
2. K8s / ACP runtime での skills/MCP 配信機構は両 spec とも明示的に
   defer（content-copy into pod or sidecar init step を想定とのみ）。
   vector-memory 配信に remote runtime を含めるなら別設計が要る。
3. Imported third-party pack の `skills/` 列挙が main に着地する時期。
   `engdocs/design/packv2/doc-pack-v2.md:59-60` で tracked とあるが
   実装スケジュール未定。
4. Per-pool PreStart caching（skills follow-up §7）が入るかどうかは未定。
   pool 大規模化のベンチ結果次第になる見込み。
5. `core` pack に `maintenance` / `dolt` を fold する作業の着地時期と
   skills の追加件数。post-v0.15.1 main 上で複数フェーズに分かれる可能性。
6. MCP v1 の Codex project-local acceptance gate が落ちた場合の plan B
   （仕様上は「v1 では merge しない」一択。fallback 設計の余地はない）。
7. MCP の `description` 除外ルールは fingerprint と equality 両方に
   適用される。skills 側に同等の「メタデータ除外」概念があるかは
   spec で言及されていない。

## 参照

- `engdocs/proposals/skill-materialization.md`
- `engdocs/proposals/skill-materialization-handoff.md`
- `engdocs/proposals/skill-materialization-implementation-plan.md`
- `engdocs/proposals/mcp-materialization.md`
- `internal/bootstrap/bootstrap.go`（`BootstrapPacks` 定義）
- `internal/config/implicit.go`（implicit import 解決）
- `internal/runtime/fingerprint.go`（`FingerprintExtra` / `CoreFingerprint`）
