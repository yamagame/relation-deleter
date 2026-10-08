# Project Structure

> 実装前（greenfield）の想定構成。実装が進んだら `/kiro-steering` で実態に合わせて更新する。

## Organization Philosophy

標準的な Go プロジェクトのレイアウトに従います。
- エントリポイント（`cmd/`）は薄く保ち、ロジックは責務ごとに `internal/` 配下のパッケージに分ける
- 依存の向きは一方向にする: 入力（スキーマ・定義）→ グラフ → 収集 → 計画 → 実行/表示

## Directory Patterns

### コマンドエントリポイント
**Location**: `/cmd/<command-name>/`
**Purpose**: フラグ解析、依存の組み立て、終了コードの決定だけを行う。ビジネスロジックは置かない
**Example**: `cmd/relation-deleter/main.go`

### 内部パッケージ
**Location**: `/internal/<domain>/`
**Purpose**: 責務ごとのパッケージ。外部から import させない
**Example**: スキーマ/定義の読み込みと検証、参照グラフ、対象収集、削除計画・実行、出力整形を、それぞれ別パッケージにする

### スクリプト
**Location**: `/scripts/`
**Purpose**: ダンプスクリプトなど、Go 以外の運用スクリプト
**Example**: `scripts/dump-schema.sh`

### テスト資材
**Location**: 各パッケージの `testdata/`、結合テスト用 SQL や Docker 設定はリポジトリ直下の `/testdata/` か `/test/`
**Purpose**: サンプルスキーマ、ゴールデンファイル、手動定義のサンプル

## Naming Conventions

- **Go ファイル**: snake_case（`schema_loader.go`）。テストは `_test.go`
- **パッケージ**: 短い小文字の単数形（`schema`, `graph`, `planner`）
- **型・関数**: Go 標準（公開は PascalCase、非公開は camelCase）
- **スクリプト**: kebab-case（`dump-schema.sh`）
- **CLI フラグ**: kebab-case（`--max-records`, `--relations`）

## Import Organization

```go
import (
    "fmt"                       // 標準ライブラリ

    "github.com/go-sql-driver/mysql" // 外部依存

    "<module>/internal/schema"  // 内部パッケージ
)
```
グループは標準・外部・内部の3つに分け、`goimports` の順序に従う。

## Code Organization Principles

- DB アクセスは interface の背後に置く。グラフ・収集・計画のロジックは、モックで単体テストできるようにする
- 純粋なロジック（グラフ構築、削除順の決定）と副作用（DB、標準出力、対話入力）を分離する
- 出力（dry-run の計画表示、進捗、結果）は専用のパッケージにまとめ、ロジック側から整形処理を追い出す

---
_Document patterns, not file trees. New files following patterns shouldn't require updates_
