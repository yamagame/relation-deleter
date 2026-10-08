# Technology Stack

## Architecture

**「ダンプ」と「削除」を、スキーマファイルという契約で分離する**構成です。

```
MySQL ──(dump script)──▶ schema.json ─┐
                      relations.yaml ─┼─▶ Go CLI ──(SELECT で収集 → DELETE)──▶ MySQL
```

- ダンプスクリプトは Go コードに依存しない。Go CLI はスキーマ解析を実行時の DB に依存しない（参照するのはダンプ結果）
- Go CLI 内部の処理の流れ: 読み込み・検証 → 参照グラフ構築 → 対象収集（読み取りのみ） → 削除計画 → 表示 or 実行
- スキーマファイルのフォーマットは両者の契約なので、変更するときは両側を同時に更新する

## Core Technologies

- **Language**: Go（CLI）、Bash（ダンプスクリプト）
- **Runtime**: Go 1.24+（ドライバ v1.10 の要件）。開発環境は asdf でプロジェクトごとに固定（`.tool-versions`: golang 1.27.1）
- **Database**: MySQL 5.7.8+ と 8.0 の両方を正式にサポートする（`JSON_OBJECT` と `GROUP_CONCAT` で順序を固定した配列を組み立てる）。どちらか一方にしかない SQL 機能（CTE、ウィンドウ関数、`utf8mb4_0900_*` 照合順序など）は使わない

## Key Libraries

- `github.com/go-sql-driver/mysql` — DB ドライバ（MPL-2.0。使うだけならライセンス上の影響はない）
- `go.yaml.in/yaml/v3` — 手動リレーション定義の読み込み（`gopkg.in/yaml.v3` はアーカイブ済みのため、こちらを使う）
- CLI の引数解析は標準ライブラリ `flag` を基本とし、外部依存は最小限にする

## Development Standards

### Code Quality
- `gofmt` / `go vet` を通すこと
- エラーは `fmt.Errorf("...: %w", err)` でラップし、ユーザー向けメッセージには原因の箇所（テーブル名、定義の行など）を含める
- パスワードを含む DSN や接続設定を、エラーやログに出力しない

### SQL
- 値は常にプレースホルダで渡す。識別子（テーブル名・カラム名）はスキーマファイル上に存在するものだけを、バッククォートでエスケープして使う
- 大量の値は IN 句をチャンクに分割して処理する（1文のサイズ上限対策）

### Testing
- 標準 `testing` パッケージとテーブル駆動テストを使う
- グラフ構築・対象収集・削除順の決定は、DB なしで単体テストできるように設計する
- DB を使う結合テストは Docker の MySQL 5.7 と 8.0 の両方で行い、build tag で通常のテストと分ける。`mysql:5.7` は amd64 イメージのみなので `platform: linux/amd64` を指定する
- ダンプスクリプトは、テスト用スキーマに対する出力をゴールデンファイルと比較して検証する

## Development Environment

### Required Tools
- Go 1.24+（asdf で `.tool-versions` の版を使う）、`mysql` クライアント 8.0、Docker（結合テスト用）
- asdf の golang プラグインが古く、macOS の `/sbin/sha256sum` で検証に失敗する場合は `PATH="/opt/homebrew/bin:$PATH" asdf install golang <ver>` を使う

### Common Commands
```bash
# Build: go build ./...
# Test:  go test ./...
# Vet:   go vet ./...
```

## Key Technical Decisions

- **スキーマの事前ダンプ**: 削除時に `information_schema` を直接見るのではなく、ダンプ済みのファイルを入力にする。理由は、レビューとバージョン管理ができ、手動定義をそのファイルに対して検証でき、本番 DB への問い合わせも減るため
- **PK 収集方式**: JOIN 付き DELETE は使わない。削除対象を収集してから削除する。理由は、dry-run で対象一覧を正確に見せられ、循環参照を訪問済み管理で扱えるため
- **ON DELETE は無視して常に削除**: ルールごとの挙動の切り替えはしない。挙動を単純で予測可能に保つため
- **認証情報**: 環境変数か接続設定ファイル（`--defaults-extra-file` 相当）で渡す。プロセス引数には出さない

---
_Document standards and patterns, not every dependency_
