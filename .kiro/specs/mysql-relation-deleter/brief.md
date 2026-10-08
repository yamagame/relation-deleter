# Brief: mysql-relation-deleter

## Problem
MySQLデータベースで特定のレコードを削除したいとき、そのレコードを参照している子レコード（さらにその子孫）を手作業で洗い出して正しい順序で削除するのは手間がかかり、漏れや誤削除のリスクが高い。外部キー制約が定義されていない関連も存在するため、DBの制約情報だけでは関連を網羅できない。

## Current State
- リポジトリはgreenfield（初回コミットのみ、コード・steeringなし）
- スキーマ情報を取り出す仕組みも、関連を辿って削除するツールも存在しない

## Desired Outcome
- ダンプスクリプトで対象DBのスキーマ（テーブル・カラム・主キー・外部キー）をJSONファイルとして取得できる
- Goコマンドに「テーブル＋主キー値」を指定すると、そのレコードを参照する子孫レコードを再帰的に特定できる
- 既定（dry-run）では削除対象レコード一覧と実行予定のDELETE文を表示するだけで、DBは変更しない
- `--execute` 指定時のみ、1トランザクション内で子→親の順に削除し、失敗時はロールバックする

## Approach
**A: シェルスクリプト（mysql CLI）でダンプ ＋ Go削除コマンド**
- ダンプスクリプト: `mysql` CLI で `information_schema`（TABLES / COLUMNS / KEY_COLUMN_USAGE / REFERENTIAL_CONSTRAINTS）を `JSON_OBJECT` / `JSON_ARRAYAGG` で集約し、`-N -B -r` で生JSONを出力してファイル保存する
- Goコマンド: スキーマJSON＋手動リレーション定義（YAML）を読み込んで参照グラフを構築 → 指定レコードから逆参照方向にBFSで子孫のPKを収集 → 依存順（子→親）に並べてDELETE（dry-run／execute）
- 選定理由: 要望（ダンプスクリプト＋golangコマンド）にそのまま対応。スキーマJSONがスクリプトとGoの明確な境界（契約）になり、独立して開発・テストできる。削除時のスキーマ解析を本番DBに依存させず、ダンプ結果をレビュー・バージョン管理できる
- 不採用案: B（Go単一バイナリにdump機能も内包）→ スクリプトが形骸化するため不採用。C（PKを収集せずJOIN付きDELETEを生成）→ dry-runで対象一覧を出せず、循環参照に弱いため不採用

## Scope
- **In**:
  - スキーマダンプスクリプト（接続情報は引数/環境変数、出力先指定）
  - スキーマJSONのフォーマット定義（テーブル、カラム、主キー（複合含む）、外部キー（複合含む））
  - 手動リレーション定義ファイル（YAML）: FK制約のない「子テーブル.カラム → 親テーブル.カラム」を追加定義
  - Go CLI: `--table` と `--id`（複数指定可、複合PKへの対応方法は設計で決定）で対象を指定
  - 子孫レコードの再帰的収集（参照されている側→参照している側の方向）、循環参照の検出・訪問済み管理
  - 削除順序の決定（子→親）と、dry-run出力（テーブル別件数、PK一覧、DELETE文）
  - `--execute` 時のトランザクション内削除とロールバック
  - 子のFKがNULL許容やON DELETE SET NULL/CASCADEであっても、参照している子は**常に削除**する（挙動を単純・予測可能に保つ）
- **Out**:
  - WHERE句による任意条件での対象指定
  - ON DELETE ルールに応じた挙動切替（SET NULL の UPDATE 等）
  - 命名規約（`xxx_id`）からのリレーション自動推測
  - 削除前データのバックアップ／リストア機能
  - MySQL以外のDB（PostgreSQL等）対応
  - GUI / Web UI

## Boundary Candidates
- **スキーマダンプ（スクリプト）**: DB → スキーマJSON。Goコードに依存しない
- **スキーマ/リレーション読み込み（Go）**: スキーマJSON＋手動定義YAML → 統合された参照グラフ
- **対象収集（Go）**: 参照グラフ＋DB接続 → 削除対象PK集合（SELECTのみ、読み取り専用）
- **削除計画と実行（Go）**: 対象PK集合 → 順序付きDELETE計画 → dry-run表示 or トランザクション実行
- 境界の契約: スキーマJSONフォーマットと手動定義YAMLフォーマット

## Out of Boundary
- データのバックアップ・復元、監査ログの永続化
- スキーマのマイグレーション・変更
- 削除以外のデータ操作（更新・コピー・匿名化）

## Upstream / Downstream
- **Upstream**: MySQL 5.7.22以上（JSON_ARRAYAGG）／8.0、`mysql` CLIクライアント、`github.com/go-sql-driver/mysql`、YAMLライブラリ（`go.yaml.in/yaml/v3`。`gopkg.in/yaml.v3`はアーカイブ済み）
- **Downstream**: 将来的なバックアップ機能、WHERE句指定、ON DELETEルール対応、他DB対応などの拡張

## Existing Spec Touchpoints
- **Extends**: なし（初のspec）
- **Adjacent**: なし

## Constraints
- 言語: Go（CLI）、ダンプはシェルスクリプト（bash想定）
- 安全性最優先: 既定はdry-run、実行は明示フラグ必須、単一トランザクション
- 大量レコード時のSELECT/DELETEはバッチ（IN句のチャンク分割）を考慮する
- 複合主キー・複合外部キー・自己参照・循環参照に対応する
- パスワード等の認証情報をプロセス引数に露出させない配慮（環境変数 / `--defaults-extra-file` 等）
