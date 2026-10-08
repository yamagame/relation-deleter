# Project Structure

> updated_at: 2026-10-08 — 実装後の実態に合わせて同期した（初版は実装前の想定構成）

## Organization Philosophy

標準的な Go プロジェクトのレイアウトに従います。
- エントリポイント（`cmd/`）は薄く保ち、ロジックは責務ごとに `internal/` 配下のパッケージに分ける
- 処理はパイプラインとして組む。各段は純粋なデータを受け渡し、副作用（DB、標準出力、対話入力）は端に寄せる
- 依存の向きは一方向にする。逆向きの import は禁止:
  `schema` → `relations` → `graph` → `collect` → `plan` → `execute` / `report` → `cli` → `cmd`
  - `report` は `execute.TableResult` を表示するので、`execute` を import してよい
  - DB に触れるのは `sqlstore`（取得）と `dbconn`（接続）だけで、どちらも `cli` だけが使う。`sqlstore` は `schema` と `collect` だけを import する（`plan` は import しない）

## Directory Patterns

### コマンドエントリポイント
**Location**: `/cmd/<command-name>/`
**Purpose**: シグナル付きの ctx を作り、`cli.Run(ctx, args, cli.StdIO(), cli.DefaultDeps())` の戻り値で `os.Exit` するだけ
**Example**: `cmd/relation-deleter/main.go`

### ドメイン・入力パッケージ
**Location**: `/internal/<domain>/`
**Purpose**: 責務ごとのパッケージ（`schema`, `relations`, `graph`, `collect`, `plan`, `execute`, `report`）。DB に依存せず、単体テストで完結する
**Example**: `collect` は `RowSource` interface 越しに行を取得するので、テストではインメモリのフェイクに差し替える

### アダプタパッケージ
**Location**: `/internal/sqlstore/`, `/internal/dbconn/`
**Purpose**: ドメインが定義した interface の SQL 実装と、接続設定の解決。DB 固有の事情（型変換、チャンク分割、照合順序の注意）はここに閉じ込める

### CLI パッケージ
**Location**: `/internal/cli/`
**Purpose**: フラグ解析（`flags.go`）、DB に接続する前の検証と全体の進行（`run.go`）、dry-run と execute の流れ（`flow.go`）、確認（`confirm.go`）、ヘルプ（`usage.go`）
**Pattern**: DB 側の副作用は `Deps`（`Open`, `NewRowSource`）、入出力は `IO`（`In/Out/Err/IsTerminal/Getenv`）として注入する。テストでは両方をフェイクにする

### スクリプト
**Location**: `/scripts/`
**Purpose**: Go 以外の運用スクリプト。Go コードには依存しない
**Example**: `scripts/dump-schema.sh`（macOS の bash 3.2 でも動くこと、shellcheck を通すこと）

### テスト資材
- **パッケージごとの入力サンプル**: 各パッケージの `testdata/`（例: `internal/relations/testdata/unknown_key.yaml`）
- **共有の資材**: リポジトリ直下の `/testdata/`
  - ゴールデンファイル（`schema.golden.<version>.json`）
  - 手動定義のサンプル（`relations.sample.yaml`）
  - 結合テスト用の環境（`testdata/integration/` に docker-compose とフィクスチャ SQL）
- **フィクスチャの約束事**: `fixture.sql` の先頭コメントに、削除起点・削除される行（閉包）・残る行を書く。フィクスチャを変えたら、その一覧と E2E テストの期待値を一緒に更新する

### 結合テスト
**Location**:
- E2E（ダンプ → CLI → DB）はリポジトリ直下の `/integration/`
- パッケージ単位の DB テストは `<pkg>/<name>_integration_test.go`

**Pattern**:
- 必ず `//go:build integration` を付ける
- 共通ヘルパーは `internal/testutil/mysqltest`（`ForEach` で 5.7 と 8.0 の両方を回し、`IsolatedDB` でテスト専用の DB を作る）

## Naming Conventions

- **Go ファイル**: snake_case（`plan_text.go`）。テストは `_test.go`、DB を使うテストは `_integration_test.go`
- **パッケージ**: 短い小文字の単数形（`schema`, `graph`, `plan`）
- **型・関数**: Go 標準（公開は PascalCase、非公開は camelCase）
- **センチネルエラー**: `ErrXxx`（例: `plan.ErrTotalMismatch`）。`errors.Is` / `errors.As` で判定できるように `%w` でラップする
- **スクリプト**: kebab-case（`dump-schema.sh`）
- **CLI フラグ**: kebab-case（`--max-records`）。mysql クライアントに合わせた短縮形（`-h -P -u -D`）を同じ変数に割り当てる

## Import Organization

```go
import (
    "fmt" // 標準ライブラリ

    "github.com/go-sql-driver/mysql" // 外部依存

    "github.com/yamagame/relation-deleter/internal/schema" // 内部パッケージ
)
```
グループは標準・外部・内部の3つに分け、`goimports` の順序に従う。パスエイリアスは使わない（モジュールパスで import する）。

## Code Organization Principles

- DB アクセスは、使う側のパッケージが定義した最小の interface の背後に置く（`collect.RowSource`、`execute.Execer`、`sqlstore.Querier`）。`*sql.Tx` がそのまま満たせる形にし、sqlmock は使わずフェイクで差し替える
- 純粋なロジック（グラフ構築、削除順の決定、計画の生成）と副作用（DB、標準出力、対話入力）を分離する
- 表示はすべて `report` に集める。計画と結果は stdout、進捗・警告・エラーは stderr に出す。進捗の間引きも `report` の責務で、ロジック側はすべての取得・実行を通知する
- トランザクションの確定・取り消しは `cli` の責務にする。`execute.Run` はコミットもロールバックもしない
- タスクの境界で後続タスクに処理を残すときは、TODO を書かずに、明示的なセンチネルを置いてドキュメントに残す（後続タスクで置き換える）

---
_Document patterns, not file trees. New files following patterns shouldn't require updates_
